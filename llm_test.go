package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, url string, retries int, timeout time.Duration, headers map[string]string) *openaiClient {
	t.Helper()
	return newOpenAIClient(&config{
		llmBaseURL:      url,
		llmAPIKey:       "k",
		llmModel:        "m",
		llmTimeout:      timeout,
		llmMaxRetries:   retries,
		llmExtraHeaders: headers,
	}, http.DefaultClient, slog.Default())
}

func TestOpenAIClientSuccess(t *testing.T) {
	var gotModel, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "打球可以啊"}}},
		})
	}))
	defer srv.Close()

	// 故意带尾部斜杠，验证 TrimRight。
	c := testClient(t, srv.URL+"/v1/", 0, 2*time.Second, nil)
	got, err := c.Complete(context.Background(), completionRequest{System: "s", User: "u", Temperature: 0.5, MaxTokens: 10})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "打球可以啊" {
		t.Fatalf("content = %q", got)
	}
	if gotModel != "m" || gotAuth != "Bearer k" {
		t.Fatalf("model=%q auth=%q", gotModel, gotAuth)
	}
}

func TestOpenAIClientNoAPIKeyOmitsAuth(t *testing.T) {
	var gotAuth string
	var gotHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, gotHeader = r.Header["Authorization"]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()
	c := newOpenAIClient(&config{
		llmBaseURL: srv.URL,
		llmModel:   "m",
		llmTimeout: 2 * time.Second,
	}, http.DefaultClient, slog.Default())
	if _, err := c.Complete(context.Background(), completionRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotHeader || gotAuth != "" {
		t.Fatalf("Authorization 头不应存在, got %q", gotAuth)
	}
}

func TestOpenAIClientExtraHeadersOverrideAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()
	c := testClient(t, srv.URL, 0, 2*time.Second, map[string]string{"Authorization": "Bearer relay"})
	if _, err := c.Complete(context.Background(), completionRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer relay" {
		t.Fatalf("auth = %q, want relay override", gotAuth)
	}
}

func TestOpenAIClientRetries(t *testing.T) {
	for _, status := range []int{429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte("busy"))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
				})
			}))
			defer srv.Close()
			c := testClient(t, srv.URL, 1, 2*time.Second, nil)
			got, err := c.Complete(context.Background(), completionRequest{})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got != "ok" || calls.Load() != 2 {
				t.Fatalf("got=%q calls=%d", got, calls.Load())
			}
		})
	}
}

func TestOpenAIClientNoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer srv.Close()
	c := testClient(t, srv.URL, 3, 2*time.Second, nil)
	if _, err := c.Complete(context.Background(), completionRequest{}); err == nil {
		t.Fatal("400 应返回错误")
	}
	if calls.Load() != 1 {
		t.Fatalf("400 不应重试，calls=%d", calls.Load())
	}
}

func TestOpenAIClientBadResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", `not json`},
		{"缺 choices", `{"id":"x"}`},
		{"content 非字符串", `{"choices":[{"message":{"content":123}}]}`},
		{"content 缺失", `{"choices":[{"message":{}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := testClient(t, srv.URL, 0, 2*time.Second, nil)
			if _, err := c.Complete(context.Background(), completionRequest{}); err == nil {
				t.Fatal("应返回错误")
			}
		})
	}
}

func TestOpenAIClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL, 0, 30*time.Millisecond, nil)
	if _, err := c.Complete(context.Background(), completionRequest{}); err == nil {
		t.Fatal("超时应返回错误")
	}
}

func TestOpenAIClientLargeBodyLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		huge := strings.Repeat("a", 2<<20)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": huge}}},
		})
	}))
	defer srv.Close()
	c := testClient(t, srv.URL, 0, 2*time.Second, nil)
	if _, err := c.Complete(context.Background(), completionRequest{}); err == nil {
		t.Fatal("超过 1 MiB 的响应应解析失败")
	}
}

// debugClient 构造带日志捕获的客户端，debug 控制 debug_prompts。
func debugClient(t *testing.T, url string, debug bool) (*openaiClient, *logCapture) {
	t.Helper()
	cap := &logCapture{}
	c := newOpenAIClient(&config{
		llmBaseURL:   url,
		llmModel:     "m",
		llmTimeout:   2 * time.Second,
		debugPrompts: debug,
	}, http.DefaultClient, slog.New(cap))
	return c, cap
}

func TestOpenAIClientDebugLogsReturn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"打球可以啊"}}]}`))
	}))
	defer srv.Close()

	c, cap := debugClient(t, srv.URL, true)
	if _, err := c.Complete(context.Background(), completionRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !cap.hasMsg("agent llm 返回") || !cap.hasAttr("content", "打球可以啊") {
		t.Fatalf("缺少 LLM 返回 Debug 日志")
	}
}

func TestOpenAIClientDebugOffNoReturnLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c, cap := debugClient(t, srv.URL, false)
	if _, err := c.Complete(context.Background(), completionRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if cap.hasMsg("agent llm 返回") {
		t.Fatal("debug_prompts=false 时不应输出返回日志")
	}
}

func TestOpenAIClientDebugLogsUnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	c, cap := debugClient(t, srv.URL, true)
	if _, err := c.Complete(context.Background(), completionRequest{}); err == nil {
		t.Fatal("应返回错误")
	}
	if !cap.hasMsg("agent llm 响应无法解析") || !cap.hasAttr("body", `{"id":"x"}`) {
		t.Fatalf("缺少解析失败 Debug 日志")
	}
}
