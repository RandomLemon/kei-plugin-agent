package persona

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory"
)

// memoryMaxTextRunes 是入站消息写入记忆时的文本上限。
const memoryMaxTextRunes = 2000

// memoryBackend 是插件对记忆子系统的依赖，由 *memory.Service 实现；测试注入假实现。
type memoryBackend interface {
	Start(ctx context.Context)
	Stop(ctx context.Context)
	Enqueue(r memory.Record) bool
	Stats() memory.Stats
	Context(context.Context, memory.ContextRequest) (memory.ContextResult, error)
	QuerySummaries(context.Context, memory.SummaryQuery) ([]memory.SummaryItem, error)
}

// memoryRecordFromEvent 由入站事件构造记忆记录；text 为 renderMessage 的扁平文本。
// 调用方保证 ev.Sender 非空（Handler 入口已拦截）。
func (p *Plugin) memoryRecordFromEvent(ev *bot.Event, text string) memory.Record {
	_, platform, botID, channelID := channelKey(ev)
	sender := memory.Sender{ID: ev.Sender.ID, Name: ev.Sender.Name, IsBot: ev.Sender.IsBot}
	conv := memory.Conv{Type: memory.ConvTypeGroup, Platform: platform, BotID: botID, ChannelID: channelID}
	if ev.Channel != nil {
		conv.Name = ev.Channel.Name
	}
	if ev.Message.Kind == bot.MessagePrivate {
		conv.Type = memory.ConvTypePrivate
		conv.PeerUserID = sender.ID
		if conv.Name == "" {
			conv.Name = sender.Name
		}
	}
	at := ev.Time
	if at.IsZero() {
		at = p.now()
	}
	r := memory.Record{
		Conv:    conv,
		EventID: ev.ID,
		MsgID:   ev.Message.ID,
		At:      at.UTC(),
		Sender:  sender,
		Text:    truncateRunes(text, memoryMaxTextRunes),
	}
	for _, seg := range ev.Message.Segments {
		switch seg.Type {
		case bot.SegReply:
			if r.ReplyTo == "" {
				r.ReplyTo = bot.SegmentData(seg, bot.KeyMessageID)
			}
		case bot.SegAt:
			if id := bot.SegmentData(seg, bot.KeyUserID); id != "" && id != atAllSentinel {
				r.Mentions = append(r.Mentions, id)
			}
		}
	}
	return r
}

// memoryRecordFromReply 构造一条自身回复的记忆记录。target 为发送目标（由会话状态构造），
// mention 为回复 @ 的用户（可为空），res 为发送结果（可为 nil），at 为发送成功时刻。
// Conv.Name 留空：群名只由入站消息维护，避免用建状态时的旧名覆盖新名。
func (p *Plugin) memoryRecordFromReply(target bot.Target, personaName, reply, mention string, res *bot.SendResult, at time.Time) memory.Record {
	conv := memory.Conv{Type: memory.ConvTypeGroup, Platform: target.Platform, BotID: target.BotID, ChannelID: target.ChannelID}
	if target.Kind == bot.MessagePrivate {
		conv.Type = memory.ConvTypePrivate
		conv.PeerUserID = target.UserID
	}
	r := memory.Record{
		Conv:    conv,
		At:      at.UTC(),
		Sender:  memory.Sender{ID: target.BotID, Name: p.personaDisplayName(personaName), IsBot: true},
		Self:    true,
		Persona: personaName,
		Text:    reply,
	}
	if res != nil {
		r.MsgID = res.MessageID
	}
	if r.MsgID == "" {
		r.EventID = "local:" + strconv.FormatInt(p.memEpoch, 10) + ":" + strconv.FormatUint(p.memSeq.Add(1), 10)
	}
	if mention != "" {
		r.Mentions = []string{mention}
	}
	return r
}

// memoryContext 在异步生成路径中读取本会话记忆；随机轮次同样读取。
func (p *Plugin) memoryContext(st *channelState, history []Turn) string {
	if p.mem == nil {
		return ""
	}
	var queries []string
	for n := len(history) - 1; n >= 0 && len(queries) < 3; n-- {
		if !history[n].Self {
			queries = append(queries, truncateRunes(history[n].Text, 100))
		}
	}
	ctx, cancel := context.WithTimeout(p.ctx, p.cfg.memory.ContextTimeout)
	defer cancel()
	req := memory.ContextRequest{Scene: memoryScene(st), Queries: queries, BudgetChars: p.cfg.memory.ContextMaxChars}
	if len(history) > 0 {
		req.HistorySince = history[0].At
	}
	result, err := p.mem.Context(ctx, req)
	if err != nil {
		if !errors.Is(err, memory.ErrNotReady) {
			p.log.Warn("persona: 记忆上下文获取失败", "operation", "memory_context", "channel", st.key, "err", err)
		}
		return ""
	}
	return renderMemoryBlock(result)
}

func renderMemoryBlock(result memory.ContextResult) string {
	var b strings.Builder
	for _, block := range result.Blocks {
		if len(block.Lines) == 0 {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("# 记忆（仅供参考，可能过时；与当前对话冲突时以当前对话为准）\n")
		} else {
			b.WriteByte('\n')
		}
		b.WriteString("[" + block.Title + "]\n")
		for _, line := range block.Lines {
			b.WriteString("- " + line + "\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// memoryScene 根据插件会话构造可信身份，供上下文与工具共用。
func memoryScene(st *channelState) memory.Scene {
	st.mu.Lock()
	conv := memory.Conv{Type: memory.ConvTypeGroup, Platform: st.platform, BotID: st.botID, ChannelID: st.channelID}
	if st.kind == bot.MessagePrivate {
		conv.Type = memory.ConvTypePrivate
		conv.PeerUserID = st.peerUserID
	}
	st.mu.Unlock()
	return memory.Scene{Conv: conv}
}
