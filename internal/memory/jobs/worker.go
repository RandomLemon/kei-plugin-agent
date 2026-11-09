package jobs

import (
	"context"
	"errors"
	"fmt"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"gorm.io/gorm"
	"log/slog"
	"sync"
	"time"
)

const (
	workerConcurrency = 2
	jobTimeout        = 30 * time.Minute
	workerIdleWait    = 5 * time.Second
)

// Handler 执行一个任务；失败时也返回已产生的结果。
type Handler func(context.Context, store.Job) (store.JobResult, error)

// Workers 并发执行已注册的任务；注册表在启动后只读。
type Workers struct {
	Queue    Queue
	Log      *slog.Logger
	Handlers map[string]Handler
	Timeout  time.Duration
	IdleWait time.Duration
}

// Run 启动固定数量的 worker，并等待它们随 ctx 退出。
func (w Workers) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range workerConcurrency {
		wg.Go(func() { w.run(ctx) })
	}
	wg.Wait()
}

func (w Workers) run(ctx context.Context) {
	timeout, idle := w.Timeout, w.IdleWait
	if timeout <= 0 {
		timeout = jobTimeout
	}
	if idle <= 0 {
		idle = workerIdleWait
	}
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		job, err := w.Queue.Claim(opCtx, time.Now().UTC())
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				w.Log.Warn("persona memory: 领取任务失败", "operation", "memory_job", "err", err)
			}
			timer := time.NewTimer(idle)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		started := time.Now()
		taskCtx, taskCancel := context.WithTimeout(ctx, timeout)
		var result store.JobResult
		handler, ok := w.Handlers[job.Type]
		if !ok {
			err = fmt.Errorf("未知任务类型")
		} else {
			result, err = handler(taskCtx, job)
		}
		if err == nil {
			err = taskCtx.Err()
		}
		taskCancel()
		if ctx.Err() != nil {
			return
		}

		args := []any{"operation", "memory_job", "job_id", job.ID, "job_type", job.Type, "attempts", job.Attempts, "duration_ms", time.Since(started).Milliseconds()}
		for ctx.Err() == nil {
			opCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
			saveErr := w.Queue.Finish(opCtx, job, result, err, time.Now().UTC())
			cancel()
			if saveErr == nil {
				break
			}
			w.Log.Warn("persona memory: 保存任务状态失败，稍后重试", append(args, "err", saveErr)...)
			timer := time.NewTimer(idle)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if ctx.Err() == nil && err != nil && job.Attempts >= jobMaxAttempts {
			w.Log.Error("persona memory: 任务失败", append(args, "err", err)...)
		}

	}
}
