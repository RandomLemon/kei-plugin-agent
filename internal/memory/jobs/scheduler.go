package jobs

import (
	"context"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

// Scheduler 每日按本地时间建立去重任务。
type Scheduler struct {
	Queue    Queue
	Location *time.Location
	DailyAt  string
	DayStart time.Duration // 摘要日日界，与 DailyAt 一致
}

// Tick 刷新计数，并在每日指定时间后创建任务。
func (s Scheduler) Tick(ctx context.Context, now time.Time) (Counts, error) {
	counts, err := s.Queue.Count(ctx)
	if err != nil {
		return counts, err
	}
	local := now.In(s.Location)
	if local.Format("15:04") < s.DailyAt {
		return counts, nil
	}
	date := local.Format("2006-01-02")
	err = s.Queue.Create(ctx, store.Job{Type: "cleanup", DedupeKey: "cleanup:" + date, Payload: store.JobPayload{Date: date}, Status: "pending", RunAfter: now.UTC()})
	if err != nil {
		return counts, err
	}
	today := store.LocalDate(now, s.Location, s.DayStart)
	var convs []string
	if err = s.Queue.DB.WithContext(ctx).Model(&store.ConvDay{}).Distinct("conv_key").Where("local_date >= ? AND local_date < ? AND msg_count > 0 AND summarized_upto_id IS NULL", today.AddDate(0, 0, -7), today).Pluck("conv_key", &convs).Error; err != nil {
		return counts, err
	}
	for _, conv := range convs {
		if err = s.Queue.Create(ctx, store.Job{Type: "daily_summary", DedupeKey: "daily:" + conv + ":" + date, Payload: store.JobPayload{ConvKey: conv}, Status: "pending", RunAfter: now.UTC()}); err != nil {
			return counts, err
		}
	}
	return counts, nil
}
