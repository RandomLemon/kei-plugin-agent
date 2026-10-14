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
	failNextPage    bool
	listEntered     chan struct{}
	listRelease     chan struct{}
	listCanceled    chan struct{}
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
	failNextPage, listEntered, listRelease, listCanceled := s.failNextPage, s.listEntered, s.listRelease, s.listCanceled
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
		if params.Cursor == "" && listEntered != nil {
			select {
			case listEntered <- struct{}{}:
			default:
			}
			select {
			case <-listRelease:
			case <-r.Context().Done():
				if listCanceled != nil {
					close(listCanceled)
				}
				return
			}
		}
		if params.Cursor != "" && failNextPage {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
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

func (s *mcpStub) setDirectory(defs []mcpToolDef, failNextPage bool) {
	s.mu.Lock()
	s.defs, s.failNextPage = defs, failNextPage
	s.mu.Unlock()
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

// waitMCPRefresh 等完成通知及刷新标记清除，不依赖估算网络耗时。
func waitMCPRefresh(t *testing.T, env *testEnv, sends int) {
	t.Helper()
	if !env.waitSends(sends, 2*time.Second) {
		t.Fatal("未收到刷新完成通知")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		env.p.writeMu.Lock()
		refreshing := env.p.mcpRefreshing
		env.p.writeMu.Unlock()
		if !refreshing {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("刷新任务未退出")
}

func TestMCPDirectoryReload(t *testing.T) {
	s := newMCPStub(t, false)
	comp := &stubCompleter{fn: func(n int) (completionResult, error) {
		switch n {
		case 0:
			return completionResult{ToolCalls: callsOf(stubToolName)}, nil
		case 1:
			return completionResult{Content: "原工具回复"}, nil
		case 2:
			return completionResult{ToolCalls: callsOf("mcp__get_weather")}, nil
		case 3:
			return completionResult{Content: "新工具回复"}, nil
		default:
			return completionResult{Content: "普通回复"}, nil
		}
	}}
	env := newMCPEnv(t, s, comp)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "先查询"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("初始工具未回复")
	}
	env.p.wg.Wait()
	beforeList := len(s.recorded())
	if got := env.command("tools"); got != "persona: tools=[mcp__extra mcp__search]" || len(s.recorded()) != beforeList {
		t.Fatalf("目录展示应只读本地: %q", got)
	}

	changed := testMCPDef("extra")
	changed.InputSchema = json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	s.setDirectory([]mcpToolDef{testMCPDef("get_weather"), changed}, false)
	if got := env.privateCommand("u9", "tools", "reload"); got != "persona: 开始刷新 MCP 工具" {
		t.Fatal(got)
	}
	waitMCPRefresh(t, env, 2)
	if sent := env.fake.at(1); sent.Target.Kind != "private" || sent.Target.UserID != "u9" || sent.Target.BotID != "bot1" || sent.Msg.Kind != "private" {
		t.Fatalf("私聊通知目标 = %+v", sent)
	}
	if env.fake.texts()[1] != "persona: MCP 工具刷新成功，工具 2 个" {
		t.Fatal(env.fake.texts())
	}
	_ = env.deliver(atEvent("g1", "u1", "张三", "再查询"))
	if !env.waitSends(3, 2*time.Second) {
		t.Fatal("新工具未回复")
	}
	env.p.wg.Wait()
	reqs := comp.requests()
	if defs := reqs[2].Tools; len(defs) != 2 || defs[1].Function.Name != "mcp__get_weather" || string(defs[0].Function.Parameters) != string(changed.InputSchema) {
		t.Fatalf("更新后的定义 = %+v", defs)
	}
	last := s.recorded()
	var call struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(last[len(last)-1].message.Params, &call)
	if call.Name != "get_weather" || env.fake.texts()[2] != "新工具回复" {
		t.Fatal("未实际调用新增工具并发送回复")
	}

	old := env.p.toolsFor("default")
	s.setDirectory([]mcpToolDef{testMCPDef("future"), changed}, true)
	if got := env.command("tools", "reload"); got != "persona: 开始刷新 MCP 工具" {
		t.Fatal(got)
	}
	waitMCPRefresh(t, env, 4)
	if env.fake.texts()[3] != "persona: MCP 工具刷新失败：MCP 请求失败：http_500" || !reflect.DeepEqual(old, env.p.toolsFor("default")) {
		t.Fatal("分页失败未报告或覆盖了旧目录")
	}

	s.setDirectory(nil, false)
	if got := env.command("tools", "reload"); got != "persona: 开始刷新 MCP 工具" {
		t.Fatal(got)
	}
	waitMCPRefresh(t, env, 5)
	if env.command("tools") != "persona: tools=[]" || env.fake.texts()[4] != "persona: MCP 工具刷新成功，工具 0 个" {
		t.Fatal("空目录未发布")
	}
	_ = env.deliver(atEvent("g1", "u1", "张三", "普通聊天"))
	if !env.waitSends(6, 2*time.Second) {
		t.Fatal("空目录后未回复")
	}
	if len(comp.requests()[4].Tools) != 0 || env.fake.texts()[5] != "普通回复" {
		t.Fatal("空目录后仍发送工具定义")
	}
}

func TestMCPReloadSnapshotAndStop(t *testing.T) {
	t.Run("生成固定快照", func(t *testing.T) {
		s := newMCPStub(t, false)
		s.callEntered, s.callRelease = make(chan struct{}, 1), make(chan struct{})
		comp := &stubCompleter{fn: func(n int) (completionResult, error) {
			if n%2 == 0 {
				return completionResult{ToolCalls: callsOf(stubToolName)}, nil
			}
			return completionResult{Content: fmt.Sprintf("回复%d", n)}, nil
		}}
		env := newMCPEnv(t, s, comp)
		env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(atEvent("g1", "u1", "张三", "第一次"))
		awaitMCP(t, s.callEntered)
		changed := testMCPDef("search")
		changed.InputSchema = json.RawMessage(`{"type":"object","properties":{"lang":{"type":"string"}}}`)
		s.setDirectory([]mcpToolDef{changed, testMCPDef("extra")}, false)
		if got := env.command("tools", "reload"); got != "persona: 开始刷新 MCP 工具" {
			t.Fatal(got)
		}
		waitMCPRefresh(t, env, 1)
		close(s.callRelease)
		if !env.waitSends(2, 2*time.Second) {
			t.Fatal("旧生成未完成")
		}
		env.p.wg.Wait()
		if defs := comp.requests()[1].Tools; string(defs[1].Function.Parameters) != string(testMCPDef("search").InputSchema) {
			t.Fatal("当前生成的定义被刷新改写")
		}
		_ = env.deliver(atEvent("g1", "u1", "张三", "第二次"))
		if !env.waitSends(3, 2*time.Second) {
			t.Fatal("新生成未完成")
		}
		if defs := comp.requests()[2].Tools; string(defs[1].Function.Parameters) != string(changed.InputSchema) {
			t.Fatal("下一次生成未使用新定义")
		}
	})
	t.Run("刷新准入与关闭", func(t *testing.T) {
		s := newMCPStub(t, false)
		env := newMCPEnv(t, s, &stubCompleter{})
		old := env.p.toolsFor("default")
		s.mu.Lock()
		s.defs = []mcpToolDef{testMCPDef("future")}
		s.listEntered, s.listRelease, s.listCanceled = make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
		s.mu.Unlock()
		replies := make(chan string, 1)
		go func() { replies <- env.command("tools", "reload") }()
		select {
		case got := <-replies:
			if got != "persona: 开始刷新 MCP 工具" {
				t.Fatal(got)
			}
		case <-time.After(time.Second):
			t.Fatal("reload Handler 被网络阻塞")
		}
		awaitMCP(t, s.listEntered)
		before := len(s.recorded())
		if got := env.command("tools", "reload"); got != "persona: MCP 工具正在刷新" || len(s.recorded()) != before {
			t.Fatalf("重复刷新被准入: %q", got)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := env.p.Stop(t.Context()); err != nil {
				t.Error(err)
			}
		}()
		awaitMCP(t, done)
		awaitMCP(t, s.listCanceled)
		if got := env.command("tools", "reload"); got != "persona: 插件正在关闭，无法刷新 MCP 工具" {
			t.Fatal(got)
		}
		if !reflect.DeepEqual(old, env.p.toolsFor("default")) || env.fake.count() != 0 {
			t.Fatal("关闭后发布了目录或发送了完成通知")
		}
		if _, _, deletes := s.counts(); deletes != 1 {
			t.Fatalf("session 未清理: %d", deletes)
		}
	})
}
