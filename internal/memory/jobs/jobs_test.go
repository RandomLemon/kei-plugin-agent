package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"gorm.io/gorm"
)

func testQueue(t *testing.T) Queue {
	t.Helper()
	db := memtest.Open(t, memtest.NewDB(t))
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	return Queue{DB: db}
}

func TestQueueLifecycle(t *testing.T) {
	q := testQueue(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, key := range []string{"a", "a", "b"} {
		if err := q.Create(ctx, store.Job{Type: "cleanup", DedupeKey: key, Status: "pending", RunAfter: now}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	claimed := make(chan store.Job, 2)
	for range 2 {
		wg.Go(func() {
			j, err := q.Claim(ctx, now)
			if err != nil {
				t.Error(err)
				return
			}
			claimed <- j
		})
	}
	wg.Wait()
	close(claimed)
	ids := map[int64]bool{}
	for j := range claimed {
		if ids[j.ID] {
			t.Fatal("重复领取")
		}
		ids[j.ID] = true
	}
	if len(ids) != 2 {
		t.Fatalf("领取 %d 个", len(ids))
	}
	if err := q.Reset(ctx, now); err != nil {
		t.Fatal(err)
	}
	j, err := q.Claim(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if j.Attempts != 2 {
		t.Fatalf("attempts=%d", j.Attempts)
	}
	result := store.JobResult{Deleted: map[string]int64{"x": 2}}
	if err = q.Finish(ctx, j, result, errors.New("failed"), now); err != nil {
		t.Fatal(err)
	}
	var saved store.Job
	if err = q.DB.WithContext(ctx).First(&saved, j.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Status != "pending" || saved.Result.Deleted["x"] != 2 || !saved.RunAfter.Equal(now.Add(4*time.Minute)) {
		t.Fatalf("退避结果 %+v", saved)
	}
	j.Attempts = 5
	if err = q.Finish(ctx, j, result, errors.New("failed"), now); err != nil {
		t.Fatal(err)
	}
	counts, err := q.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Dead != 1 {
		t.Fatalf("counts=%+v", counts)
	}
}

func TestScheduler(t *testing.T) {
	q := testQueue(t)
	loc := time.FixedZone("UTC+8", 8*3600)
	s := Scheduler{Queue: q, Location: loc, DailyAt: "04:00", DayStart: 4 * time.Hour}
	now := time.Date(2026, 11, 7, 19, 59, 0, 0, time.UTC)
	if _, err := s.Tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	c, err := q.Count(t.Context())
	if err != nil || c.Pending != 0 {
		t.Fatalf("提前建任务 %+v %v", c, err)
	}
	for range 2 {
		if _, err = s.Tick(t.Context(), now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	c, err = s.Tick(t.Context(), now.Add(2*time.Minute))
	if err != nil || c.Pending != 1 {
		t.Fatalf("去重/计数 %+v %v", c, err)
	}
}

func TestWorkersCancellationAndTimeout(t *testing.T) {
	for _, stopping := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "stop"}[stopping], func(t *testing.T) {
			q := testQueue(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if err := q.Create(ctx, store.Job{Type: "test", DedupeKey: "test", Status: "pending", RunAfter: time.Now()}); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{}, 1)
			w := Workers{Queue: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Timeout: 30 * time.Millisecond, IdleWait: time.Millisecond, Handlers: map[string]Handler{"test": func(ctx context.Context, _ store.Job) (store.JobResult, error) {
				entered <- struct{}{}
				<-ctx.Done()
				return store.JobResult{}, ctx.Err()
			}}}
			done := make(chan struct{})
			go func() { defer close(done); w.Run(ctx) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("未执行")
			}
			if stopping {
				cancel()
			} else {
				deadline := time.Now().Add(time.Second)
				for {
					c, err := q.Count(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if c.Pending == 1 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("超时任务未退避")
					}
					time.Sleep(time.Millisecond)
				}
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("worker 未退出")
			}
			var j store.Job
			if err := q.DB.WithContext(t.Context()).First(&j).Error; err != nil {
				t.Fatal(err)
			}
			want := "pending"
			if stopping {
				want = "running"
			}
			if j.Status != want {
				t.Fatalf("status=%s want=%s", j.Status, want)
			}
		})
	}
}

func TestUnknownJobFails(t *testing.T) {
	q := testQueue(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := q.Create(ctx, store.Job{Type: "unknown", DedupeKey: "u", Status: "pending", RunAfter: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w := Workers{Queue: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), IdleWait: time.Millisecond}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		var j store.Job
		if err := q.DB.WithContext(ctx).First(&j).Error; err != nil {
			t.Fatal(err)
		}
		if j.LastError == "未知任务类型" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("未知任务未失败")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestFinishRetryDoesNotRepeatHandler(t *testing.T) {
	q := testQueue(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var writes, calls atomic.Int64
	if err := q.DB.Callback().Update().Before("gorm:update").Register("test_finish_failure", func(db *gorm.DB) {
		if db.Statement.Table == "mem_job" {
			if j, ok := db.Statement.Dest.(*store.Job); ok && j.Status == "done" && writes.Add(1) == 1 {
				_ = db.AddError(errors.New("temporary outage"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.Create(ctx, store.Job{Type: "test", DedupeKey: "retry", Status: "pending", RunAfter: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w := Workers{Queue: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), IdleWait: time.Millisecond, Handlers: map[string]Handler{"test": func(context.Context, store.Job) (store.JobResult, error) { calls.Add(1); return store.JobResult{}, nil }}}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		var j store.Job
		if err := q.DB.WithContext(ctx).First(&j).Error; err != nil {
			t.Fatal(err)
		}
		if j.Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("状态未恢复")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if calls.Load() != 1 || writes.Load() < 2 {
		t.Fatalf("calls=%d writes=%d", calls.Load(), writes.Load())
	}
}

func TestDailyScheduling(t *testing.T) {
	q := testQueue(t)
	now := time.Date(2026, 11, 8, 5, 0, 0, 0, time.UTC)
	checkpoint := int64(1)
	for _, day := range []store.ConvDay{
		{ConvKey: "pending", LocalDate: now.AddDate(0, 0, -1), MsgCount: 4},
		{ConvKey: "pending", LocalDate: now.AddDate(0, 0, -2), MsgCount: 4},
		{ConvKey: "done", LocalDate: now.AddDate(0, 0, -1), MsgCount: 4, SummarizedUptoID: &checkpoint},
		{ConvKey: "old", LocalDate: now.AddDate(0, 0, -8), MsgCount: 4},
	} {
		if err := q.DB.WithContext(t.Context()).Create(&day).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := Scheduler{Queue: q, Location: time.UTC, DailyAt: "04:00", DayStart: 4 * time.Hour}
	for range 2 {
		if _, err := s.Tick(t.Context(), now); err != nil {
			t.Fatal(err)
		}
	}
	var jobs []store.Job
	if err := q.DB.WithContext(t.Context()).Where("type='daily_summary'").Find(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Payload.ConvKey != "pending" {
		t.Fatalf("jobs=%+v", jobs)
	}
}

func TestRetryUsageAccumulates(t *testing.T) {
	q := testQueue(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := q.Create(ctx, store.Job{Type: "daily_summary", DedupeKey: "usage", Status: "pending", RunAfter: now}); err != nil {
		t.Fatal(err)
	}
	first, err := q.Claim(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Finish(ctx, first, store.JobResult{PromptTokens: 5, CompletionTokens: 2, FailedSlices: 1}, errors.New("retry"), now); err != nil {
		t.Fatal(err)
	}
	second, err := q.Claim(ctx, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Finish(ctx, second, store.JobResult{PromptTokens: 7, CompletionTokens: 3, Days: 1}, nil, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var saved store.Job
	if err = q.DB.WithContext(ctx).First(&saved, second.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Result.PromptTokens != 12 || saved.Result.CompletionTokens != 5 || saved.Result.FailedSlices != 1 || saved.Status != "done" {
		t.Fatalf("累计结果 %+v", saved)
	}
}
