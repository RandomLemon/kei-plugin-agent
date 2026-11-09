package persona

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory"
	"github.com/RandomLemon/kei/pkg/bot"
)

func TestMemorySummaryDefinitionAndResults(t *testing.T) {
	want := `{"type":"function","function":{"name":"memory__summary","description":"查询本会话过去某段时间聊过的话题摘要。当有人问到之前、上周、某天聊了什么时使用。按日期问时不要填 query，query 只写具体话题。","parameters":{"type":"object","properties":{"date_from":{"type":"string","description":"起始日期 YYYY-MM-DD，可省略"},"date_to":{"type":"string","description":"结束日期 YYYY-MM-DD，可省略"},"query":{"type":"string","description":"具体话题关键词，如「聚餐」；按日期问时不填"}}}}}`
	tool := memorySummaryTool{}
	data, err := json.Marshal(tool.Def())
	if err != nil {
		t.Fatal(err)
	}
	var gotJSON, wantJSON any
	if json.Unmarshal(data, &gotJSON) != nil || json.Unmarshal([]byte(want), &wantJSON) != nil || !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatal(string(data))
	}
	scene := memory.Scene{Conv: memory.Conv{Type: memory.ConvTypeGroup, Platform: "mock", ChannelID: "g1"}}
	for _, tc := range []struct {
		name, args string
		items      []memory.SummaryItem
		err        error
		want       toolResult
	}{
		{name: "result", items: []memory.SummaryItem{{Date: "2026-11-04", Title: "聚餐", Summary: "周末安排", OpenItems: []string{"未决：几点", "待办：订桌"}}}, want: toolResult{Content: "[2026-11-04] 聚餐：周末安排（未决：几点；待办：订桌）", Status: "ok"}},
		{name: "empty", want: toolResult{Content: "没有找到相关的聊天摘要。", Status: "ok", Detail: "no_result"}},
		{name: "date", err: memory.ErrInvalidDateRange, want: toolResult{Content: "日期格式应为 YYYY-MM-DD，且跨度不超过 31 天。", Status: "error", Detail: "bad_args"}},
		{name: "not_ready", err: memory.ErrNotReady, want: toolResult{Content: "记忆暂时不可用。", Status: "error", Detail: "not_ready"}},
		{name: "failed", err: errors.New("db"), want: toolResult{Content: "记忆暂时不可用。", Status: "error", Detail: "failed"}},
		{name: "timeout", err: context.DeadlineExceeded, want: toolResult{Content: "记忆暂时不可用。", Status: "error", Detail: "failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := &fakeMemory{queryFn: func(ctx context.Context, req memory.SummaryQuery) ([]memory.SummaryItem, error) {
				if ctx != t.Context() || req.Scene != scene || req.DateFrom != "2026-11-01" || req.DateTo != "2026-11-04" || req.Query != "聚餐" {
					t.Errorf("req=%+v", req)
				}
				return tc.items, tc.err
			}}
			got := (memorySummaryTool{backend: mem, scene: scene}).Call(t.Context(), `{"date_from":"2026-11-01","date_to":"2026-11-04","query":"聚餐","scene":{"conv":"other"}}`)
			if got != tc.want {
				t.Fatalf("%+v want %+v", got, tc.want)
			}
		})
	}
	for _, args := range []string{`null`, `[]`, `{"date_from":1}`, `{} garbage`} {
		got := tool.Call(t.Context(), args)
		if got.Detail != "bad_args" {
			t.Fatal(args, got)
		}
	}
	for _, args := range []string{"", "  "} {
		var got memory.SummaryQuery
		mem := &fakeMemory{queryFn: func(_ context.Context, req memory.SummaryQuery) ([]memory.SummaryItem, error) {
			got = req
			return nil, nil
		}}
		if r := (memorySummaryTool{backend: mem, scene: scene}).Call(t.Context(), args); r.Detail != "no_result" || got.Scene != scene || got.DateFrom != "" || got.Query != "" {
			t.Fatalf("空参数 %q: %+v %+v", args, r, got)
		}
	}
}

func TestMemoryToolsCatalogAndPersona(t *testing.T) {
	env := newTestEnvMem(t, nil, nil, nil, nil, &fakeMemory{})
	st := &channelState{platform: "mock", channelID: "g1", kind: bot.MessageGroup}
	if len(env.p.toolsFor("default", st)) != 0 {
		t.Fatal("默认不应启用")
	}
	env.p.cfg.memoryToolsEnabled = true
	env.p.replaceMCPTools([]tool{&stubTool{name: "mcp__search"}})
	tools := env.p.toolsFor("default", st)
	if len(tools) != 2 || tools[0].Def().Function.Name != "mcp__search" || tools[1].Def().Function.Name != memorySummaryName {
		t.Fatal(tools)
	}
	if tools[1].(memorySummaryTool).scene.Conv.ChannelID != "g1" {
		t.Fatal("会话未绑定")
	}
	pf := env.p.cfg.personas["default"]
	pf.hasTools = true
	pf.Tools = []string{memorySummaryName, "mcp__missing"}
	env.p.cfg.personas["default"] = pf
	if got := env.p.toolsFor("default", st); len(got) != 1 || got[0].Def().Function.Name != memorySummaryName {
		t.Fatal(got)
	}
	pf.Tools = nil
	env.p.cfg.personas["default"] = pf
	if len(env.p.toolsFor("default", st)) != 0 {
		t.Fatal("空名单未禁用工具")
	}
	if got := env.command("tools"); got != "persona: tools=[mcp__search memory__summary]" {
		t.Fatal(got)
	}
	// MCP 刷新发布空目录，已装配的记忆工具仍然可用。
	env.p.replaceMCPTools(nil)
	pf.hasTools = false
	env.p.cfg.personas["default"] = pf
	if got := env.p.toolsFor("default", st); len(got) != 1 || got[0].Def().Function.Name != memorySummaryName {
		t.Fatal(got)
	}
	if got := env.command("tools", "reload"); got != "persona: 未启用 MCP" {
		t.Fatal(got)
	}
	if got := env.command("tools"); got != "persona: tools=[memory__summary]" {
		t.Fatal(got)
	}
}

func TestMemoryToolSurvivesMCPReload(t *testing.T) {
	stub := newMCPStub(t, false)
	env := newMCPEnv(t, stub, &stubCompleter{fn: func(int) (completionResult, error) { return completionResult{Content: "回复"}, nil }})
	env.p.mem = &fakeMemory{}
	env.p.cfg.memoryToolsEnabled = true
	if got := env.command("tools"); got != "persona: tools=[mcp__extra mcp__search memory__summary]" {
		t.Fatal(got)
	}
	stub.setDirectory(nil, false)
	if got := env.command("tools", "reload"); got != "persona: 开始刷新 MCP 工具" {
		t.Fatal(got)
	}
	waitMCPRefresh(t, env, 1)
	if got := env.command("tools"); got != "persona: tools=[memory__summary]" {
		t.Fatal(got)
	}
}
