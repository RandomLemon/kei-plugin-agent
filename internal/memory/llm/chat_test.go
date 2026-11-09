package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testOutput struct {
	Overview string `json:"overview"`
	Topics   []any  `json:"topics"`
}

func (o *testOutput) Validate() error {
	if o.Topics == nil {
		return errors.New("missing topics")
	}
	return nil
}
func answer(w http.ResponseWriter, content string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 2}})
}
func testClient(url string, host *http.Client) *Client {
	c := New(Config{BaseURL: url, Timeout: time.Second, JSONMode: true}, host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.retryWait = time.Millisecond
	return c
}

func TestRequestRepairAndUsage(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "custom" {
			t.Error("端点/请求头错误")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["model"]) != `"small"` || len(body["response_format"]) == 0 {
			t.Errorf("请求体 %s", body)
		}
		if n == 1 {
			answer(w, `{"bad":[]}`)
		} else {
			if !strings.Contains(string(body["messages"]), "上次输出不合规") {
				t.Error("没有修复原因")
			}
			answer(w, `{"topics":[]}`)
		}
	}))
	defer server.Close()
	c := testClient(server.URL, &http.Client{Timeout: time.Nanosecond})
	c.cfg.Headers = map[string]string{"Authorization": "custom"}
	var out testOutput
	usage, err := c.ChatJSON(t.Context(), Request{Model: "small", System: "system", User: "input"}, &out)
	if err != nil || calls.Load() != 2 || usage != (Usage{6, 4}) {
		t.Fatalf("usage=%+v calls=%d err=%v", usage, calls.Load(), err)
	}
}

func TestHTTPRetries(t *testing.T) {
	for _, status := range []int{400, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "" {
					t.Error("空 key 不应带鉴权")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			c := testClient(server.URL, nil)
			var out testOutput
			_, err := c.ChatJSON(t.Context(), Request{}, &out)
			want := int64(2)
			if status == 400 {
				want = 1
			}
			if err == nil || calls.Load() != want {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestTimeoutAndHostTimeout(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		answer(w, `{"topics":[]}`)
	}))
	defer server.Close()
	c := testClient(server.URL, &http.Client{Timeout: time.Millisecond})
	var out testOutput
	if _, err := c.ChatJSON(t.Context(), Request{}, &out); err != nil {
		t.Fatalf("继承了宿主超时：%v", err)
	}
	c.cfg.Timeout = time.Millisecond
	if _, err := c.ChatJSON(t.Context(), Request{}, &out); err == nil || calls.Load() != 3 {
		t.Fatalf("超时未重试 calls=%d err=%v", calls.Load(), err)
	}
}

func TestInvalidOutputReturnsUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { answer(w, `{"topics":null}`) }))
	defer server.Close()
	c := testClient(server.URL, nil)
	var out testOutput
	usage, err := c.ChatJSON(t.Context(), Request{}, &out)
	if !errors.Is(err, ErrInvalidOutput) || usage != (Usage{6, 4}) {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = c.ChatJSON(ctx, Request{}, &out); err == nil {
		t.Fatal("取消未生效")
	}
}

func TestRepairDoesNotKeepPartialFields(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			answer(w, `{"overview":"stale","topics":1}`)
		} else {
			answer(w, `{"topics":[]}`)
		}
	}))
	defer server.Close()
	c := testClient(server.URL, nil)
	var out testOutput
	if _, err := c.ChatJSON(t.Context(), Request{}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Overview != "" {
		t.Fatal("修复响应继承了前次非法输出")
	}
}
