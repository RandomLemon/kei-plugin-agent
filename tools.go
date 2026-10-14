package persona

import (
	"context"
	"errors"
	"slices"
	"time"
	"unicode/utf8"
)

// maxToolCallsPerRound 是单轮最多执行的工具调用数；多出的调用只回忽略文本。
const maxToolCallsPerRound = 3

// errStale 表示工具轮之间会话 epoch 已变化（/persona off|switch|reset），本轮生成作废。
var errStale = errors.New("persona: 会话已变化，放弃本轮生成")

// tool 是一个可供 LLM 调用的工具。
// Call 必须尊重 ctx：ctx 带有循环施加的 tools_call_timeout，取消后应尽快返回。
// 工具自身的失败（网络、上游错误、参数非法）不返回 error，
// 而是返回交给 LLM 的失败文本。
type tool interface {
	Def() toolDef
	Call(ctx context.Context, args string) toolResult // args 为 tool_calls[].function.arguments 原文
}

// toolResult 是一次工具调用的结果。
type toolResult struct {
	Content string // 作为 role=tool 消息的 content（循环会再按 tools_result_max_chars 截断）
	Status  string // 工具只返回 "ok" 或 "error"；其余取值由循环设置（mcp.md §13.6）
	Detail  string // 工具自定义的细分原因，只进日志，可为空（如 "no_result"、"http_503"）
}

// toolsFor 返回人格可用的工具：不写 tools 时继承全部已装配工具，否则按名单过滤，保持装配顺序。
func (p *Plugin) toolsFor(personaName string) []tool {
	pf := p.cfg.personas[personaName]
	if !pf.hasTools {
		return append([]tool(nil), p.tools...)
	}
	var out []tool
	for _, t := range p.tools {
		if slices.Contains(pf.Tools, t.Def().Function.Name) {
			out = append(out, t)
		}
	}
	return out
}

// completeWithTools 执行一轮生成：人格无可用工具时只调一次 Complete；
// 否则按 mcp.md §13.4 循环执行工具调用，直到模型给出文本或轮数用尽。
func (p *Plugin) completeWithTools(st *channelState, epoch uint64, personaName string, req completionRequest) (string, error) {
	tools := p.toolsFor(personaName)
	if len(tools) == 0 {
		res, err := p.completer.Complete(p.ctx, req)
		return res.Content, err
	}

	for _, t := range tools {
		req.Tools = append(req.Tools, t.Def())
	}
	for round := 0; ; round++ {
		req.ToolChoice = ""
		if round >= p.cfg.toolsMaxRounds {
			req.ToolChoice = "none" // 轮数用尽：仍带 tools，但禁止再调用
		}
		res, err := p.completer.Complete(p.ctx, req)
		if err != nil {
			return "", err
		}
		if len(res.ToolCalls) == 0 || req.ToolChoice == "none" {
			return res.Content, nil
		}

		st.mu.Lock()
		stale := st.epoch != epoch
		st.mu.Unlock()
		if stale {
			return "", errStale
		}

		var text *string // assistant 无文本时发 null
		if res.Content != "" {
			text = &res.Content
		}
		req.Followups = append(req.Followups, chatMessage{Role: "assistant", Content: text, ToolCalls: res.ToolCalls})
		for i, call := range res.ToolCalls {
			r := p.runTool(st.key, tools, i, call)
			req.Followups = append(req.Followups, chatMessage{Role: "tool", Content: &r.Content, ToolCallID: call.ID})
		}
		if err := p.ctx.Err(); err != nil {
			return "", err
		}
	}
}

// runTool 执行一次工具调用，并统一施加调用数、未知工具、超时与长度兜底。
func (p *Plugin) runTool(channel string, tools []tool, i int, call toolCall) toolResult {
	start := time.Now()
	name := call.Function.Name
	idx := slices.IndexFunc(tools, func(t tool) bool { return t.Def().Function.Name == name })

	var r toolResult
	switch {
	case i >= maxToolCallsPerRound:
		r = toolResult{Content: "[工具调用被忽略] 本轮工具调用过多。", Status: "ignored"}
	case idx < 0:
		r = toolResult{Content: "[工具调用失败] 未知工具 " + name + "。", Status: "unknown_tool"}
	default:
		r = p.callTool(tools[idx], name, call.Function.Arguments)
	}
	r.Content = truncateWithEllipsis(r.Content, p.cfg.toolsResultMaxChars)

	attrs := []any{"channel", channel, "tool", name, "status", r.Status, "detail", r.Detail,
		"duration_ms", time.Since(start).Milliseconds()}
	if p.cfg.debugPrompts {
		attrs = append(attrs, "args", truncateRunes(call.Function.Arguments, 256))
	}
	p.log.Debug("persona 工具调用", attrs...)
	return r
}

// callTool 以 tools_call_timeout 调用工具；超时改写为超时结果，非 "ok" 的 status 一律记为 "error"。
func (p *Plugin) callTool(t tool, name, args string) toolResult {
	ctx, cancel := context.WithTimeout(p.ctx, p.cfg.toolsCallTimeout)
	defer cancel()
	r := t.Call(ctx, args)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return toolResult{Content: "[工具调用失败] 工具 " + name + " 超时。", Status: "timeout", Detail: r.Detail}
	}
	if r.Status != "ok" {
		r.Status = "error"
	}
	return r
}

// truncateWithEllipsis 按 rune 截断到 max，被截断时末尾追加 "…"。
func truncateWithEllipsis(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return truncateRunes(s, max) + "…"
}
