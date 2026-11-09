package persona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory"
)

const memorySummaryName = "memory__summary"

// memorySummaryTool 持有插件构造的会话身份，模型参数不能改变查询范围。
type memorySummaryTool struct {
	backend memoryBackend
	scene   memory.Scene
}

func (t memorySummaryTool) Def() toolDef {
	return toolDef{Type: "function", Function: toolFunction{
		Name:        memorySummaryName,
		Description: "查询本会话过去某段时间聊过的话题摘要。当有人问到之前、上周、某天聊了什么时使用。按日期问时不要填 query，query 只写具体话题。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"date_from":{"type":"string","description":"起始日期 YYYY-MM-DD，可省略"},"date_to":{"type":"string","description":"结束日期 YYYY-MM-DD，可省略"},"query":{"type":"string","description":"具体话题关键词，如「聚餐」；按日期问时不填"}}}`),
	}}
}

func (t memorySummaryTool) Call(ctx context.Context, args string) toolResult {
	var input struct {
		DateFrom string `json:"date_from"`
		DateTo   string `json:"date_to"`
		Query    string `json:"query"`
	}
	badArgs := toolResult{Content: "日期格式应为 YYYY-MM-DD，且跨度不超过 31 天。", Status: "error", Detail: "bad_args"}
	if strings.TrimSpace(args) == "" { // 部分模型对无必填参数的调用传空串
		args = "{}"
	}
	if !strings.HasPrefix(strings.TrimSpace(args), "{") || json.Unmarshal([]byte(args), &input) != nil {
		return badArgs
	}
	items, err := t.backend.QuerySummaries(ctx, memory.SummaryQuery{Scene: t.scene, DateFrom: input.DateFrom, DateTo: input.DateTo, Query: input.Query})
	switch {
	case errors.Is(err, memory.ErrInvalidDateRange):
		return badArgs
	case errors.Is(err, memory.ErrNotReady):
		return toolResult{Content: "记忆暂时不可用。", Status: "error", Detail: "not_ready"}
	case err != nil:
		return toolResult{Content: "记忆暂时不可用。", Status: "error", Detail: "failed"}
	case len(items) == 0:
		return toolResult{Content: "没有找到相关的聊天摘要。", Status: "ok", Detail: "no_result"}
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		line := fmt.Sprintf("[%s] %s：%s", item.Date, item.Title, item.Summary)
		if len(item.OpenItems) > 0 {
			line += "（" + strings.Join(item.OpenItems, "；") + "）"
		}
		lines = append(lines, line)
	}
	return toolResult{Content: strings.Join(lines, "\n"), Status: "ok"}
}
