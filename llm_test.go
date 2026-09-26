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
