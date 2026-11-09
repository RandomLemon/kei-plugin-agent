package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/jobs"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/summary"
)

// ingestBatch 是已校验、规范化，待在一个事务内写入的一批记录。
type ingestBatch struct {
	convs    []store.Conversation // 按 conv_key 去重并排序
	messages []store.Message
}

// Enqueue 把记录放入写队列，永不阻塞。队列满时丢弃并计入 Dropped；已停止时返回 false。
// Mentions 会被复制，调用方入队后可复用原切片。
func (s *Service) Enqueue(r Record) bool {
	r.Mentions = slices.Clone(r.Mentions)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stopped {
		return false
	}
	select {
	case s.queue <- r:
		return true
	default:
		s.dropped.Add(1)
		return false
	}
}

// writeLoop 攒批写入：满 IngestBatch 条立即写，IngestInterval 到期写未满的批。
// ctx 取消（Stop）后写完剩余记录再返回。
func (s *Service) writeLoop(ctx context.Context, db *gorm.DB) {
	// writeCtx 在 ctx 取消（Stop）后再过 flushTimeout 才取消：正在写的批次不会被 Stop 立即打断
	// （否则服务端已提交、客户端却报错，重写时整批被记成重复），并与随后的 flush 共用这一收尾时限。
	writeCtx, cancelWrite := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWrite()
	stopAfter := context.AfterFunc(ctx, func() { time.AfterFunc(s.flushTimeout, cancelWrite) })
	defer stopAfter()

	batch := make([]Record, 0, s.cfg.IngestBatch)
	ticker := time.NewTicker(s.cfg.IngestInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush(writeCtx, db, s.prepare(batch))
			return
		case r := <-s.queue:
			batch = append(batch, r)
			if len(batch) < s.cfg.IngestBatch {
				continue
			}
		case <-ticker.C:
			if len(batch) == 0 {
				continue
			}
		}
		b := s.prepare(batch)
		batch = batch[:0]
		if !s.writeWithRetry(ctx, writeCtx, db, b) {
			s.flush(writeCtx, db, b) // 停止打断了本批，交给 flush 再写一次
			return
		}
	}
}

// prepare 推导会话键与去重键、规范化时间与字段；不合法的记录计入 Rejected 并丢弃。
func (s *Service) prepare(records []Record) ingestBatch {
	now := s.now().UTC()
	convs := make(map[string]*store.Conversation)
	var b ingestBatch
	for _, r := range records {
		key, okConv := convKey(r.Conv)
		dedupe, okDedupe := dedupeKey(r)
		if !okConv || !okDedupe {
			s.rejected.Add(1)
			continue
		}
		at := r.At.UTC()
		if at.IsZero() || at.Sub(now) > maxFutureSkew {
			at = now
		}
		mentions := r.Mentions
		if mentions == nil {
			mentions = []string{}
		}
		b.messages = append(b.messages, store.Message{
			ConvKey:     key,
			DedupeKey:   dedupe,
			MsgID:       r.MsgID,
			EventID:     r.EventID,
			SenderID:    r.Sender.ID,
			SenderName:  cleanText(r.Sender.Name),
			SenderIsBot: r.Sender.IsBot,
			IsSelf:      r.Self,
			Persona:     r.Persona,
			Text:        cleanText(r.Text),
			ReplyTo:     r.ReplyTo,
			Mentions:    mentions,
			SentAt:      at,
			LocalDate:   store.LocalDate(at, s.cfg.Location, s.dayStart),
			ReceivedAt:  now,
		})

		c, seen := convs[key]
		if !seen {
			c = newConversation(key, r.Conv)
			convs[key] = c
		}
		if name := cleanText(r.Conv.Name); name != "" {
			c.Name = name
		}
		if c.LastMsgAt == nil || at.After(*c.LastMsgAt) {
			c.LastMsgAt = &at
		}
	}
	for _, c := range convs {
		b.convs = append(b.convs, *c)
	}
	slices.SortFunc(b.convs, func(x, y store.Conversation) int { return cmp.Compare(x.ConvKey, y.ConvKey) })
	return b
}

// cleanText 替换非法 UTF-8、去掉 \x00：PostgreSQL 的 text 不接受它们，一条坏记录会让整批写入失败。
func cleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

// newConversation 只填写构成 conv_key 的字段：群会话由多个机器人共享，不记 BotID。
func newConversation(key string, c Conv) *store.Conversation {
	conv := &store.Conversation{ConvKey: key, Type: c.Type, Platform: c.Platform}
	if c.Type == ConvTypeGroup {
		conv.ChannelID = c.ChannelID
	} else {
		conv.BotID = c.BotID
		conv.PeerUserID = c.PeerUserID
	}
	return conv
}

// writeWithRetry 写入一批，失败后按 retryBackoff 重试；最终失败时计入 FailedBatches 并丢弃。
// ctx 取消（Stop）后不再重试；单次写入用 writeCtx，Stop 后仍可在 flushTimeout 内写完。
// 返回 false 表示被停止打断、本批尚未写入。
func (s *Service) writeWithRetry(ctx, writeCtx context.Context, db *gorm.DB, b ingestBatch) bool {
	if len(b.messages) == 0 {
		return true
	}
	for attempt := 0; ; attempt++ {
		wctx, cancel := context.WithTimeout(writeCtx, s.ingestWriteTimeout)
		err := s.writeBatch(wctx, db, b)
		cancel()
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if attempt == len(s.retryBackoff) {
			s.failedBatches.Add(1)
			s.logIngestFailure(len(b.messages), err)
			return true
		}
		if !sleepCtx(ctx, s.retryBackoff[attempt]) {
			return false
		}
	}
}

// flush 在 fctx 到期（writeLoop 的 writeCtx：Stop 后 flushTimeout）前写完 pending 与队列中剩余的记录，不重试；
// 到期放弃剩余并记 Warn。
func (s *Service) flush(fctx context.Context, db *gorm.DB, pending ingestBatch) {
	b := pending
	for {
		if len(b.messages) > 0 {
			if err := s.writeBatch(fctx, db, b); err != nil {
				if fctx.Err() != nil {
					s.log.Warn("persona memory: 停止前未能写完队列", "operation", "memory_flush",
						"dropped", len(b.messages)+len(s.queue))
					return
				}
				s.failedBatches.Add(1)
				s.logIngestFailure(len(b.messages), err)
			}
		}
		records := s.drain(s.cfg.IngestBatch)
		if len(records) == 0 {
			return
		}
		b = s.prepare(records)
	}
}

// drain 非阻塞地从队列取出至多 n 条记录。
func (s *Service) drain(n int) []Record {
	var out []Record
	for len(out) < n {
		select {
		case r := <-s.queue:
			out = append(out, r)
		default:
			return out
		}
	}
	return out
}

// dayKey 标识 mem_conv_day 的一行。
type dayKey struct {
	convKey string
	date    int64 // LocalDate 的 Unix 秒
}

// writeBatch 在一个事务内 upsert 会话、逐条插入消息、累加每日计数，提交后更新计数器。
// 逐条插入是为了拿到准确的消息 ID 与重复计数；写入按去重键幂等，重试安全。
func (s *Service) writeBatch(ctx context.Context, db *gorm.DB, b ingestBatch) error {
	var written, duplicates int64
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		written, duplicates = 0, 0
		convs := slices.Clone(b.convs)
		if err := tx.Clauses(upsertConversation()).Create(&convs).Error; err != nil {
			return fmt.Errorf("upsert conversation: %w", err)
		}

		days := make(map[dayKey]*store.ConvDay)
		for _, msg := range b.messages {
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&msg)
			if res.Error != nil {
				return fmt.Errorf("insert message: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				duplicates++
				continue
			}
			written++
			k := dayKey{convKey: msg.ConvKey, date: msg.LocalDate.Unix()}
			d, ok := days[k]
			if !ok {
				d = &store.ConvDay{ConvKey: msg.ConvKey, LocalDate: msg.LocalDate}
				days[k] = d
			}
			d.MsgCount++
			d.LastMsgID = max(d.LastMsgID, msg.ID)
		}
		if len(days) == 0 {
			return nil
		}

		rows := make([]store.ConvDay, 0, len(days))
		for _, d := range days {
			rows = append(rows, *d)
		}
		slices.SortFunc(rows, func(x, y store.ConvDay) int {
			return cmp.Or(cmp.Compare(x.ConvKey, y.ConvKey), x.LocalDate.Compare(y.LocalDate))
		})
		if err := tx.Clauses(upsertConvDay(), clause.Returning{}).Create(&rows).Error; err != nil {
			return fmt.Errorf("upsert conv day: %w", err)
		}
		today := store.LocalDate(s.now(), s.cfg.Location, s.dayStart)
		for _, day := range rows {
			if day.LocalDate.Equal(today) && day.MsgCount-day.RolledMsgCount >= summary.RollingEvery && day.RollingRuns < summary.RollingMaxPerDay {
				job := store.Job{Type: "rolling_summary", DedupeKey: fmt.Sprintf("rolling:%s:%s:%d", day.ConvKey, day.LocalDate.Format("2006-01-02"), day.RollingRuns+1), Payload: store.JobPayload{Run: day.RollingRuns + 1, ConvKey: day.ConvKey, Date: day.LocalDate.Format("2006-01-02")}, Status: "pending", RunAfter: s.now()}
				if err := (jobs.Queue{DB: tx}).Create(ctx, job); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.written.Add(written)
	s.duplicates.Add(duplicates)
	s.log.Debug("persona memory: 批量写入完成", "operation", "memory_ingest", "success", true,
		"count", len(b.messages), "duplicates", duplicates)
	return nil
}

// upsertConversation 冲突时只更新名字（不让空名覆盖已有群名）与最后消息时间（取较大值）。
func upsertConversation() clause.OnConflict {
	return clause.OnConflict{
		Columns: []clause.Column{{Name: "conv_key"}},
		DoUpdates: clause.Set{
			{Column: clause.Column{Name: "name"}, Value: gorm.Expr("COALESCE(NULLIF(EXCLUDED.name, ''), mem_conversation.name)")},
			{Column: clause.Column{Name: "last_msg_at"}, Value: gorm.Expr("GREATEST(mem_conversation.last_msg_at, EXCLUDED.last_msg_at)")},
		},
	}
}

// upsertConvDay 冲突时累加消息数，最后消息 ID 取较大值。
func upsertConvDay() clause.OnConflict {
	return clause.OnConflict{
		Columns: []clause.Column{{Name: "conv_key"}, {Name: "local_date"}},
		DoUpdates: clause.Set{
			{Column: clause.Column{Name: "msg_count"}, Value: gorm.Expr("mem_conv_day.msg_count + EXCLUDED.msg_count")},
			{Column: clause.Column{Name: "last_msg_id"}, Value: gorm.Expr("GREATEST(mem_conv_day.last_msg_id, EXCLUDED.last_msg_id)")},
		},
	}
}

func (s *Service) logIngestFailure(count int, err error) {
	s.log.Warn("persona memory: 批量写入失败", "operation", "memory_ingest", "success", false,
		"count", count, "err", err)
}
