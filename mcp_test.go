package persona

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

type mcpRecorded struct {
	message mcpMessage
	headers http.Header
}

// mcpStub 是可变目录的单端点桩；阻塞请求随 HTTP ctx 退出。
type mcpStub struct {
	t               *testing.T
	srv             *httptest.Server
	mu              sync.Mutex
	defs            []mcpToolDef
	records         []mcpRecorded
	initializations int
	deletes         int
	sse             bool
	call404         bool
	callEntered     chan struct{}
	callRelease     chan struct{}
	callCanceled    chan struct{}
}

func testMCPDef(name string) mcpToolDef {
	return mcpToolDef{Name: name, Description: "查询资料", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}
}

func newMCPStub(t *testing.T, sse bool) *mcpStub {
	t.Helper()
	s := &mcpStub{t: t, sse: sse, defs: []mcpToolDef{testMCPDef("search"), testMCPDef("extra")}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *mcpStub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		s.deletes++
		s.mu.Unlock()
		if r.Header.Get("MCP-Session-Id") == "" {
			s.t.Error("DELETE 缺少 session")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var msg mcpMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		s.t.Error(err)
		w.WriteHeader(400)
		return
	}
	s.mu.Lock()
	s.records = append(s.records, mcpRecorded{message: msg, headers: r.Header.Clone()})
	defs := append([]mcpToolDef{}, s.defs...)
	call404, entered, release, canceled := s.call404, s.callEntered, s.callRelease, s.callCanceled
	if msg.Method == "initialize" {
		s.initializations++
	}
	session := fmt.Sprintf("session-%d", s.initializations)
	s.mu.Unlock()
	if r.Header.Get("MCP-Protocol-Version") != mcpVersion || r.Header.Get("Accept") != "application/json, text/event-stream" || r.Header.Get("Content-Type") != "application/json" {
		s.t.Error("MCP 协议请求头不正确")
	}
	if msg.Method != "initialize" && r.Header.Get("MCP-Session-Id") != session {
		s.t.Error("后续请求未携带 session")
	}
	if msg.Method == "notifications/initialized" || msg.Method == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch msg.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(msg.Params, &params) != nil || params.ProtocolVersion != mcpVersion {
			s.t.Error("初始化版本不正确")
		}
		w.Header().Set("MCP-Session-Id", session)
		result = map[string]any{"protocolVersion": mcpVersion, "capabilities": map[string]any{"tools": struct{}{}}}
	case "tools/list":
		var params struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		if params.Cursor == "" && len(defs) > 1 {
			result = map[string]any{"tools": defs[:1], "nextCursor": "next"}
		} else if params.Cursor != "" {
			result = map[string]any{"tools": defs[1:]}
		} else {
			result = map[string]any{"tools": defs}
		}
	case "tools/call":
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				if canceled != nil {
					close(canceled)
				}
				return
			}
		}
		if call404 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if s.sse {
			result = map[string]any{"content": []any{}, "structuredContent": map[string]string{"answer": "资料"}}
		} else {
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "资料"}}, "structuredContent": map[string]string{"answer": "不重复展示"}}
		}
	default:
		s.t.Errorf("未知方法 %q", msg.Method)
		w.WriteHeader(400)
		return
	}
	response := mcpMessage{JSONRPC: "2.0", ID: msg.ID}
	response.Result, _ = json.Marshal(result)
	if !s.sse {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	// 顺便覆盖注释、通知、多行 data 与服务端 ping；响应后不主动关流。
	_, _ = fmt.Fprint(w, ": comment\n\ndata: {"+`"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`+"\n\n")
	if msg.Method == "initialize" {
		_, _ = fmt.Fprint(w, "data: "+`{"jsonrpc":"2.0","id":-1,"method":"ping"}`+"\n\n")
	}
	data, _ := json.Marshal(response)
	_, _ = fmt.Fprintf(w, "data: \ndata: %s\n\n", data)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

func (s *mcpStub) recorded() []mcpRecorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mcpRecorded(nil), s.records...)
}

func (s *mcpStub) counts() (initializations, calls, deletes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.message.Method == "tools/call" {
			calls++
		}
	}
	return s.initializations, calls, s.deletes
}

func newMCPEnv(t *testing.T, s *mcpStub, comp *stubCompleter) *testEnv {
	t.Helper()
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["mcp_url"] = s.srv.URL
		c["mcp_timeout"] = "2s"
		c["mcp_extra_headers"] = map[string]any{"Authorization": "Bearer test-token"}
	})
	env.p.completer = comp
	return env
}

func awaitMCP(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("等待 MCP 请求超时")
	}
}

func TestMCPConversation(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%t", sse), func(t *testing.T) {
			s := newMCPStub(t, sse)
			comp := &stubCompleter{fn: func(n int) (completionResult, error) {
				if n == 0 {
					return completionResult{ToolCalls: callsOf(stubToolName)}, nil
				}
				return completionResult{Content: "查到了"}, nil
			}}
			env := newMCPEnv(t, s, comp)
			env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
			if err := env.deliver(atEvent("g1", "u1", "张三", "查询资料")); err != nil {
				t.Fatal(err)
			}
			if !env.waitSends(1, 2*time.Second) {
				t.Fatal("MCP 调用后未发送回复")
			}
			if got := env.fake.texts(); !reflect.DeepEqual(got, []string{"查到了"}) {
				t.Fatalf("回复 = %v", got)
			}
			reqs := comp.requests()
			if len(reqs) != 2 || len(reqs[0].Tools) != 2 {
				t.Fatalf("LLM 请求 = %+v", reqs)
			}
			def := reqs[0].Tools[1].Function
			if def.Name != stubToolName || def.Description != "查询资料" || string(def.Parameters) != string(s.defs[0].InputSchema) {
				t.Fatalf("工具定义 = %+v", def)
			}
			want := "资料"
			if sse {
				want = `{"answer":"资料"}`
			}
			if f := reqs[1].Followups; len(f) != 2 || *f[1].Content != want {
				t.Fatalf("工具结果 = %+v", f)
			}
			var methods []string
			for _, r := range s.recorded() {
				if r.message.Method != "" {
					methods = append(methods, r.message.Method)
				}
				if r.headers.Get("Authorization") != "Bearer test-token" {
					t.Fatal("自定义请求头未传递")
				}
				if r.message.Method == "tools/call" {
					var args struct {
						Name      string            `json:"name"`
						Arguments map[string]string `json:"arguments"`
					}
					if json.Unmarshal(r.message.Params, &args) != nil || args.Name != "search" || args.Arguments["query"] != "q" {
						t.Fatalf("远端调用 = %s", r.message.Params)
					}
				}
			}
			if !reflect.DeepEqual(methods, []string{"initialize", "notifications/initialized", "tools/list", "tools/list", "tools/call"}) {
				t.Fatalf("协议顺序 = %v", methods)
			}
			if err := env.p.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, calls, deletes := s.counts(); calls != 1 || deletes != 1 {
				t.Fatalf("calls=%d deletes=%d", calls, deletes)
			}
			if sse {
				for _, r := range s.recorded() {
					if string(r.message.ID) == "-1" && r.message.Method == "" && string(r.message.Result) == "{}" {
						return
					}
				}
				t.Fatal("未响应服务端 ping")
			}
		})
	}
}

func TestMCPSessionRecoveryDoesNotReplay(t *testing.T) {
	s := newMCPStub(t, false)
	s.call404 = true
	env := newMCPEnv(t, s, &stubCompleter{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := env.p.toolsFor("default")[1].Call(ctx, `{"query":"q"}`)
	if result.Status != "error" || result.Detail != "http_404" {
		t.Fatalf("原调用应失败: %+v", result)
	}
	if inits, calls, _ := s.counts(); inits != 2 || calls != 1 {
		t.Fatalf("初始化=%d 工具调用=%d", inits, calls)
	}
}

func TestMCPStopCancelsCall(t *testing.T) {
	s := newMCPStub(t, false)
	s.callEntered, s.callRelease, s.callCanceled = make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
	comp := &stubCompleter{fn: func(int) (completionResult, error) { return completionResult{ToolCalls: callsOf(stubToolName)}, nil }}
	env := newMCPEnv(t, s, comp)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	if err := env.deliver(atEvent("g1", "u1", "张三", "查询资料")); err != nil {
		t.Fatal(err)
	}
	awaitMCP(t, s.callEntered)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := env.p.Stop(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	awaitMCP(t, done)
	awaitMCP(t, s.callCanceled)
	if _, calls, deletes := s.counts(); calls != 1 || deletes != 1 {
		t.Fatalf("calls=%d deletes=%d", calls, deletes)
	}
	if len(comp.requests()) != 1 || env.fake.count() != 0 {
		t.Fatal("取消后仍继续生成或发送")
	}
}
