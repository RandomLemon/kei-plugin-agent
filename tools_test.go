package persona

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubCompleter 按调用序号返回脚本化结果，并记录每次请求。
type stubCompleter struct {
	mu   sync.Mutex
	reqs []completionRequest
	fn   func(n int) (completionResult, error) // n 为第几次调用，从 0 起
}

func (s *stubCompleter) Complete(_ context.Context, req completionRequest) (completionResult, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	n := len(s.reqs) - 1
	s.mu.Unlock()
	return s.fn(n)
}

func (s *stubCompleter) requests() []completionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]completionRequest(nil), s.reqs...)
}

// stubTool 是可注入的桩工具；call 为空时返回 "资料"。
type stubTool struct {
	name string
	call func(ctx context.Context, args string) toolResult

	mu   sync.Mutex
	args []string
}

func (s *stubTool) Def() toolDef {
	return toolDef{Type: "function", Function: toolFunction{Name: s.name, Parameters: []byte(`{"type":"object"}`)}}
}

func (s *stubTool) Call(ctx context.Context, args string) toolResult {
	s.mu.Lock()
	s.args = append(s.args, args)
	s.mu.Unlock()
	if s.call != nil {
		return s.call(ctx, args)
	}
	return toolResult{Content: "资料", Status: "ok"}
}

func (s *stubTool) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.args)
}

// callsOf 构造一组工具调用，ID 依次为 call_1、call_2……
func callsOf(names ...string) []toolCall {
	out := make([]toolCall, len(names))
	for i, name := range names {
		out[i] = toolCall{ID: fmt.Sprintf("call_%d", i+1), Type: "function",
			Function: toolCallFunction{Name: name, Arguments: `{"query":"q"}`}}
	}
	return out
}

// newToolEnv 构造启用 knowledge_search 的测试环境，并把 completer 与 p.tools 换成桩。
func newToolEnv(t *testing.T, comp *stubCompleter, tools []tool, mutate func(map[string]any)) *testEnv {
	t.Helper()
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["tools_enabled"] = []any{ragToolName}
		c["rag_base_url"] = "http://rag.test/api/v1"
		c["rag_api_token"] = "secret-token"
		if mutate != nil {
			mutate(c)
		}
	})
	env.p.completer = comp
	env.p.tools = tools
	return env
}

// runLoop 在 g1 会话上直接执行一次 completeWithTools。
func runLoop(env *testEnv, personaName string) (string, error) {
	st := env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	st.mu.Lock()
	epoch := st.epoch
	st.mu.Unlock()
	return env.p.completeWithTools(st, epoch, personaName, completionRequest{System: "s", User: textParts("u")})
}

func TestBuiltinToolsNamesMatch(t *testing.T) {
	cfg := &config{ragBaseURL: "http://rag.test/api/v1", ragTopK: 3, ragContextMaxChars: 600, toolsResultMaxChars: 2000}
	for _, spec := range builtinTools() {
		if got := spec.build(cfg, http.DefaultClient, nil).Def().Function.Name; got != spec.name {
			t.Errorf("登记名 %q 与 Def 名 %q 不一致", spec.name, got)
		}
	}
	if got := buildTools(cfg, http.DefaultClient, nil); len(got) != 0 {
		t.Fatalf("tools_enabled 为空时不应构造工具: %d", len(got))
	}
}

func TestToolsForPersona(t *testing.T) {
	search, extra := &stubTool{name: ragToolName}, &stubTool{name: "extra"}
	comp := &stubCompleter{fn: func(int) (completionResult, error) { return completionResult{Content: "ok"}, nil }}
	env := newToolEnv(t, comp, []tool{search, extra}, func(c map[string]any) {
		c["personas"] = map[string]any{
			"default": "继承",
			"quiet":   map[string]any{"prompt": "p", "tools": []any{}},
			"picked":  map[string]any{"prompt": "p", "tools": []any{ragToolName}},
		}
	})
	cases := []struct {
		persona string
		want    []string
	}{
		{"default", []string{ragToolName, "extra"}},
		{"quiet", nil},
		{"picked", []string{ragToolName}},
	}
	for _, tc := range cases {
		t.Run(tc.persona, func(t *testing.T) {
			comp.mu.Lock()
			comp.reqs = nil
			comp.mu.Unlock()
			if _, err := runLoop(env, tc.persona); err != nil {
				t.Fatalf("completeWithTools: %v", err)
			}
			reqs := comp.requests()
			if len(reqs) != 1 {
				t.Fatalf("应只调用一次 Complete, got %d", len(reqs))
			}
			var got []string
			for _, d := range reqs[0].Tools {
				got = append(got, d.Function.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestToolLoopThenText(t *testing.T) {
	search := &stubTool{name: ragToolName}
	comp := &stubCompleter{fn: func(n int) (completionResult, error) {
		if n == 0 {
			return completionResult{ToolCalls: callsOf(ragToolName)}, nil
		}
		return completionResult{Content: "先重装驱动"}, nil
	}}
	env := newToolEnv(t, comp, []tool{search}, nil)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "显卡驱动怎么装"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("应发送最终文本")
	}
	if got := env.fake.texts(); len(got) != 1 || got[0] != "先重装驱动" {
		t.Fatalf("发送 = %v", got)
	}
	if search.calls() != 1 {
		t.Fatalf("工具应被调用一次, got %d", search.calls())
	}
	reqs := comp.requests()
	if len(reqs) != 2 {
		t.Fatalf("应调用两次 Complete, got %d", len(reqs))
	}
	f := reqs[1].Followups
	if len(f) != 2 || f[0].Role != "assistant" || f[0].Content != nil || len(f[0].ToolCalls) != 1 {
		t.Fatalf("assistant 消息 = %+v", f)
	}
	if f[1].Role != "tool" || f[1].ToolCallID != "call_1" || *f[1].Content != "资料" {
		t.Fatalf("tool 消息 = %+v", f[1])
	}
	if reqs[0].ToolChoice != "" || reqs[1].ToolChoice != "" {
		t.Fatal("未用尽轮数时不应发 tool_choice")
	}
}

func TestToolLoopMaxRounds(t *testing.T) {
	search := &stubTool{name: ragToolName}
	comp := &stubCompleter{fn: func(int) (completionResult, error) {
		return completionResult{ToolCalls: callsOf(ragToolName)}, nil
	}}
	env := newToolEnv(t, comp, []tool{search}, nil)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitReason("empty_reply", 2*time.Second) {
		t.Fatal("轮数用尽仍只有 tool_calls 时应 empty_reply")
	}
	reqs := comp.requests()
	if len(reqs) != 3 {
		t.Fatalf("应调用 3 次 Complete, got %d", len(reqs))
	}
	if reqs[0].ToolChoice != "" || reqs[1].ToolChoice != "" || reqs[2].ToolChoice != "none" {
		t.Fatalf("tool_choice = %q %q %q", reqs[0].ToolChoice, reqs[1].ToolChoice, reqs[2].ToolChoice)
	}
	if len(reqs[2].Tools) != 1 {
		t.Fatal("最后一轮仍应带 tools")
	}
	if search.calls() != 2 {
		t.Fatalf("工具应执行 2 轮, got %d", search.calls())
	}
}

func TestToolLoopCallLimits(t *testing.T) {
	search, extra := &stubTool{name: ragToolName}, &stubTool{name: "extra"}
	comp := &stubCompleter{fn: func(n int) (completionResult, error) {
		if n == 0 {
			// 4 个调用：未知名、人格不可用的已装配工具、可用工具、超出上限的第 4 个。
			return completionResult{ToolCalls: callsOf("nope", "extra", ragToolName, ragToolName)}, nil
		}
		return completionResult{Content: "好"}, nil
	}}
	env := newToolEnv(t, comp, []tool{search, extra}, func(c map[string]any) {
		c["personas"] = map[string]any{"default": map[string]any{"prompt": "p", "tools": []any{ragToolName}}}
	})
	if _, err := runLoop(env, "default"); err != nil {
		t.Fatalf("completeWithTools: %v", err)
	}
	if search.calls() != 1 || extra.calls() != 0 {
		t.Fatalf("执行次数 search=%d extra=%d", search.calls(), extra.calls())
	}
	f := comp.requests()[1].Followups
	if len(f) != 5 {
		t.Fatalf("应有 assistant + 4 条 tool 消息, got %d", len(f))
	}
	want := []string{
		"[工具调用失败] 未知工具 nope。",
		"[工具调用失败] 未知工具 extra。",
		"资料",
		"[工具调用被忽略] 本轮工具调用过多。",
	}
	for i, w := range want {
		m := f[i+1]
		if m.ToolCallID != fmt.Sprintf("call_%d", i+1) || *m.Content != w {
			t.Errorf("第 %d 条 tool 消息 = %s %q, want %q", i+1, m.ToolCallID, *m.Content, w)
		}
	}
	if !env.cap.hasAttr("status", "unknown_tool") || !env.cap.hasAttr("status", "ignored") {
		t.Fatal("应记录 unknown_tool 与 ignored")
	}
}

func TestToolLoopTimeout(t *testing.T) {
	var hasDeadline bool
	slow := &stubTool{name: ragToolName, call: func(ctx context.Context, _ string) toolResult {
		_, hasDeadline = ctx.Deadline()
		<-ctx.Done()
		return toolResult{Status: "error", Detail: "network"}
	}}
	comp := &stubCompleter{fn: func(n int) (completionResult, error) {
		if n == 0 {
			return completionResult{ToolCalls: callsOf(ragToolName)}, nil
		}
		return completionResult{Content: "好"}, nil
	}}
	env := newToolEnv(t, comp, []tool{slow}, func(c map[string]any) { c["tools_call_timeout"] = "20ms" })
	content, err := runLoop(env, "default")
	if err != nil || content != "好" {
		t.Fatalf("超时后循环应继续: content=%q err=%v", content, err)
	}
	if !hasDeadline {
		t.Fatal("工具应收到带截止时间的 ctx")
	}
	if got := *comp.requests()[1].Followups[1].Content; got != "[工具调用失败] 工具 knowledge_search 超时。" {
		t.Fatalf("tool 消息 = %q", got)
	}
	if !env.cap.hasAttr("status", "timeout") {
		t.Fatal("应记录 status=timeout")
	}
}

func TestToolLoopResultLimits(t *testing.T) {
	odd := &stubTool{name: ragToolName, call: func(context.Context, string) toolResult {
		return toolResult{Content: "abcdefgh", Status: "weird"}
	}}
	comp := &stubCompleter{fn: func(n int) (completionResult, error) {
		if n == 0 {
			return completionResult{ToolCalls: callsOf(ragToolName)}, nil
		}
		return completionResult{Content: "好"}, nil
	}}
	env := newToolEnv(t, comp, []tool{odd}, func(c map[string]any) { c["tools_result_max_chars"] = 5 })
	if _, err := runLoop(env, "default"); err != nil {
		t.Fatalf("completeWithTools: %v", err)
	}
	if got := *comp.requests()[1].Followups[1].Content; got != "abcde…" {
		t.Fatalf("tool 消息 = %q", got)
	}
	if !env.cap.hasAttr("status", "error") {
		t.Fatal("非法 status 应记为 error")
	}
}

func TestToolLoopStale(t *testing.T) {
	search := &stubTool{name: ragToolName}
	var st *channelState
	comp := &stubCompleter{fn: func(int) (completionResult, error) {
		// 模拟生成期间被 /persona off|switch|reset。
		st.mu.Lock()
		st.epoch++
		st.mu.Unlock()
		return completionResult{ToolCalls: callsOf(ragToolName)}, nil
	}}
	env := newToolEnv(t, comp, []tool{search}, nil)
	st = env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	st.mu.Lock()
	epoch := st.epoch
	st.mu.Unlock()
	env.p.wg.Add(1)
	env.p.generate(st, epoch, nil)
	if !env.cap.has("stale") {
		t.Fatal("want stale")
	}
	if search.calls() != 0 || env.fake.count() != 0 {
		t.Fatalf("stale 时不应调用工具或发送: calls=%d sends=%d", search.calls(), env.fake.count())
	}
}

func TestToolLoopLaterCompleteFails(t *testing.T) {
	comp := &stubCompleter{fn: func(n int) (completionResult, error) {
		if n == 0 {
			return completionResult{ToolCalls: callsOf(ragToolName)}, nil
		}
		return completionResult{}, errors.New("http 500")
	}}
	env := newToolEnv(t, comp, []tool{&stubTool{name: ragToolName}}, nil)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitReason("llm_error", 2*time.Second) {
		t.Fatal("want llm_error")
	}
	if env.fake.count() != 0 {
		t.Fatal("LLM 失败不应发送")
	}
}

func TestToolLogArgsOnlyWithDebug(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprint(debug), func(t *testing.T) {
			comp := &stubCompleter{fn: func(n int) (completionResult, error) {
				if n == 0 {
					return completionResult{ToolCalls: callsOf(ragToolName)}, nil
				}
				return completionResult{Content: "好"}, nil
			}}
			env := newToolEnv(t, comp, []tool{&stubTool{name: ragToolName}}, func(c map[string]any) { c["debug_prompts"] = debug })
			if _, err := runLoop(env, "default"); err != nil {
				t.Fatalf("completeWithTools: %v", err)
			}
			if !env.cap.hasMsg("persona 工具调用") {
				t.Fatal("应输出工具调用日志")
			}
			if got := env.cap.attrOf("persona 工具调用", "args") != ""; got != debug {
				t.Fatalf("args 出现 = %v, want %v", got, debug)
			}
		})
	}
}
