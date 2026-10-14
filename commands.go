package persona

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/pkg/message"
)

func personaUsage() string {
	return "用法: /persona status | switch [name] | on | off | reset | policy [group|private mode] | list [group|private [add|del id]] | tools [reload]"
}

// handleCommand 处理 /persona 管理命令。
func (p *Plugin) handleCommand(ctx context.Context, ev *bot.Event, r bot.Reply) error {
	if ev.Command == nil {
		return nil
	}
	// 工具命令不需要会话状态，尤其不能在关闭后触发新的懒加载任务。
	if args := ev.Command.Args; len(args) > 0 && args[0] == "tools" {
		return p.handleToolsCommand(ctx, bot.TargetFromEvent(ev), r, args[1:])
	}
	st := p.stateFor(ev)
	args := ev.Command.Args
	if len(args) == 0 {
		return r.Text(personaUsage()).Send(ctx)
	}
	switch args[0] {
	case "status":
		return r.Text(p.statusLine(st)).Send(ctx)
	case "switch":
		if len(args) == 1 {
			name, src := p.resolvePersona(st)
			return r.Text(fmt.Sprintf("persona: persona=%s 来源=%s", name, src)).Send(ctx)
		}
		name := args[1]
		if _, ok := p.cfg.personas[name]; !ok {
			return r.Text("persona: 未找到人格 " + name).Send(ctx)
		}
		st.mu.Lock()
		st.persona = name
		st.epoch++
		st.mu.Unlock()
		p.saveOverride(st)
		return r.Text("persona: persona=" + name).Send(ctx)
	case "on":
		st.mu.Lock()
		st.disabled = false
		st.mu.Unlock()
		p.saveOverride(st)
		return r.Text("persona: 已开启").Send(ctx)
	case "off":
		st.mu.Lock()
		st.disabled = true
		st.epoch++
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		st.mu.Unlock()
		p.saveOverride(st)
		return r.Text("persona: 已关闭").Send(ctx)
	case "reset":
		st.reset()
		p.saveOverride(st)
		return r.Text("persona: 已重置").Send(ctx)
	case "policy":
		if len(args) == 1 {
			return r.Text(p.policyReport()).Send(ctx)
		}
		if len(args) != 3 || !p.setPolicyMode(args[1], args[2]) {
			return r.Text(personaUsage()).Send(ctx)
		}
		return r.Text(p.policyScopeReport(args[1])).Send(ctx)
	case "list":
		switch {
		case len(args) == 1:
			return r.Text(p.listReport()).Send(ctx)
		case len(args) == 2 && (args[1] == scopeGroup || args[1] == scopePrivate):
			return r.Text(p.listScopeReport(args[1])).Send(ctx)
		case len(args) == 4 && (args[1] == scopeGroup || args[1] == scopePrivate) && args[3] != "":
			if args[2] == "add" {
				p.addPolicyID(args[1], args[3])
			} else if args[2] == "del" {
				p.delPolicyID(args[1], args[3])
			} else {
				return r.Text(personaUsage()).Send(ctx)
			}
			return r.Text(p.listScopeReport(args[1])).Send(ctx)
		default:
			return r.Text(personaUsage()).Send(ctx)
		}
	default:
		return r.Text(personaUsage()).Send(ctx)
	}
}

func (p *Plugin) handleToolsCommand(ctx context.Context, target bot.Target, r bot.Reply, args []string) error {
	if len(args) == 0 {
		p.toolsMu.RLock()
		names := make([]string, len(p.tools))
		for i, t := range p.tools {
			names[i] = t.Def().Function.Name
		}
		p.toolsMu.RUnlock()
		return r.Text("persona: tools=[" + strings.Join(names, " ") + "]").Send(ctx)
	}
	if len(args) != 1 || args[0] != "reload" {
		return r.Text(personaUsage()).Send(ctx)
	}

	p.writeMu.Lock()
	var reply string
	switch {
	case p.closing:
		reply = "persona: 插件正在关闭，无法刷新 MCP 工具"
	case p.mcp == nil:
		reply = "persona: 未启用 MCP"
	case p.mcpRefreshing:
		reply = "persona: MCP 工具正在刷新"
	default:
		p.mcpRefreshing = true
		p.wg.Add(1)
	}
	p.writeMu.Unlock()
	if reply != "" {
		return r.Text(reply).Send(ctx)
	}
	go p.reloadMCPTools(target)
	return r.Text("persona: 开始刷新 MCP 工具").Send(ctx)
}

// reloadMCPTools 只捕获发送目标的值；加载与通知分别使用插件级 ctx 的独立预算。
func (p *Plugin) reloadMCPTools(target bot.Target) {
	defer p.wg.Done()
	defer func() {
		p.writeMu.Lock()
		p.mcpRefreshing = false
		p.writeMu.Unlock()
	}()
	tools, err := p.loadMCPTools(p.ctx)
	p.writeMu.Lock()
	if p.closing {
		p.writeMu.Unlock()
		return
	}
	if err == nil {
		p.replaceMCPTools(tools)
	}
	p.writeMu.Unlock()

	text := fmt.Sprintf("persona: MCP 工具刷新成功，工具 %d 个", len(tools))
	if err != nil {
		text = "persona: MCP 工具刷新失败：" + err.Error()
		p.log.Warn("persona: MCP 工具刷新失败", "operation", "mcp_reload", "success", false, "tool_count", len(tools), "err", err)
	} else {
		p.log.Info("persona: MCP 工具刷新成功", "operation", "mcp_reload", "success", true, "tool_count", len(tools))
	}
	if p.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, p.cfg.mcpTimeout)
	defer cancel()
	msg := message.Group(message.Text(text))
	if target.Kind == bot.MessagePrivate {
		msg = message.Private(message.Text(text))
	}
	if _, err := p.api.Send(ctx, target, msg); err != nil {
		p.log.Warn("persona: MCP 刷新通知发送失败", "operation", "mcp_reload", "err", err)
	}
}

// statusLine 生成 /persona status 的固定格式一行。
func (p *Plugin) statusLine(st *channelState) string {
	now := p.now()
	st.mu.Lock()
	disabled := st.disabled
	hist := len(st.history)
	last := st.lastReplyAt
	st.mu.Unlock()

	name, src := p.resolvePersona(st)
	window := st.repliesInWindow(now, time.Hour)

	state := "开"
	if disabled {
		state = "关"
	}
	lastStr := "从未"
	if !last.IsZero() {
		d := now.Sub(last)
		if d < 0 {
			d = 0
		}
		lastStr = d.Round(time.Second).String() + " 前"
	}
	return fmt.Sprintf(
		"persona: %s · persona=%s(%s) · 历史 %d 条 · 近 1 小时回复 %d/%d · 上次回复 %s · llm 错误 %d · 已跳 %d",
		state, name, src, hist, window, p.cfg.randomMaxPerHour, lastStr,
		p.statLLMErrors.Load(), p.statSkips.Load(),
	)
}
