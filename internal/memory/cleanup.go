package memory

import (
	"context"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/jobs"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/llm"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/summary"
	"gorm.io/gorm"
	"time"
)

const (
	jobRetentionDays  = 14
	tickInterval      = 60 * time.Second
	workerStopTimeout = 3 * time.Second
)

func (s *Service) cleanup(ctx context.Context, db *gorm.DB, _ store.Job) (store.JobResult, error) {
	result := store.JobResult{Deleted: map[string]int64{}}
	now := s.now().UTC()
	queries := []struct {
		model        any
		table, where string
		cutoff       time.Time
	}{
		{&store.Message{}, "mem_message", "sent_at < ?", now.AddDate(0, 0, -s.cfg.MessageRetentionDays)},
		{&store.Summary{}, "mem_summary", "local_date < ?", store.LocalDate(now, s.cfg.Location, s.dayStart).AddDate(0, 0, -s.cfg.SummaryRetentionDays)},
		{&store.ConvDay{}, "mem_conv_day", "local_date < ?", store.LocalDate(now, s.cfg.Location, s.dayStart).AddDate(0, 0, -s.cfg.SummaryRetentionDays)},
		{&store.Job{}, "mem_job", "status IN ('done','dead') AND updated_at < ?", now.AddDate(0, 0, -jobRetentionDays)},
	}
	for _, q := range queries {
		r := db.WithContext(ctx).Where(q.where, q.cutoff).Delete(q.model)
		result.Deleted[q.table] = r.RowsAffected
		if r.Error != nil {
			return result, r.Error
		}
	}
	return result, nil
}

// startJobs 派生独立子 ctx，保证 Stop 在写入 flush 后再取消任务。
func (s *Service) startJobs(ctx context.Context, db *gorm.DB) func() {
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	q := jobs.Queue{DB: db}
	workers := jobs.Workers{Queue: q, Log: s.log, IdleWait: s.workerIdleWait, Handlers: map[string]jobs.Handler{"cleanup": func(ctx context.Context, j store.Job) (store.JobResult, error) { return s.cleanup(ctx, db, j) }}}
	pipeline := summary.Pipeline{DB: db, Small: s.cfg.LLMModelSmall, Large: s.cfg.LLMModelLarge, Location: s.cfg.Location, DayStart: s.dayStart, Now: s.now, Log: s.log, Client: llm.New(llm.Config{BaseURL: s.cfg.LLMBaseURL, APIKey: s.cfg.LLMAPIKey, Headers: s.cfg.LLMHeaders, Timeout: s.cfg.LLMTimeout, JSONMode: s.cfg.LLMJSONMode}, s.httpClient, s.log)}
	workers.Handlers["daily_summary"] = pipeline.Daily
	workers.Handlers["rolling_summary"] = pipeline.Rolling
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); workers.Run(jobCtx) }()
	go func() {
		defer close(done)
		scheduler := jobs.Scheduler{Queue: q, Location: s.cfg.Location, DailyAt: s.cfg.DailyAt, DayStart: s.dayStart}
		ticker := time.NewTicker(s.tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				opCtx, opCancel := context.WithTimeout(jobCtx, ingestWriteTimeout)
				counts, err := scheduler.Tick(opCtx, s.now())
				opCancel()
				if err != nil {
					if jobCtx.Err() == nil {
						s.log.Warn("persona memory: 调度失败", "operation", "memory_schedule", "err", err)
					}
					continue
				}
				s.jobsPending.Store(counts.Pending)
				s.jobsRunning.Store(counts.Running)
				s.jobsDead.Store(counts.Dead)
			}
		}
	}()
	return func() {
		cancel()
		timer := time.NewTimer(s.workerStopTimeout)
		defer timer.Stop()
		for _, ch := range []chan struct{}{done, workerDone} {
			select {
			case <-ch:
			case <-timer.C:
				s.log.Warn("persona memory: 等待任务协程退出超时", "operation", "memory_stop")
				return
			}
		}
	}
}
