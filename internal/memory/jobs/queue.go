// Package jobs 提供单实例部署下的持久化任务队列、调度和 worker。
package jobs

import (
	"context"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const jobMaxAttempts = 5

// Queue 用数据库事务互斥领取进程内的后台任务。
type Queue struct{ DB *gorm.DB }

// Create 按去重键创建任务。
func (q Queue) Create(ctx context.Context, job store.Job) error {
	return q.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error
}

// Reset 恢复上次进程中断的任务，保留尝试次数。
func (q Queue) Reset(ctx context.Context, now time.Time) error {
	return q.DB.WithContext(ctx).Model(&store.Job{}).Where("status = ?", "running").Updates(map[string]any{"status": "pending", "run_after": now}).Error
}

// Claim 原子地领取一个到期任务；无任务时返回 gorm.ErrRecordNotFound。
func (q Queue) Claim(ctx context.Context, now time.Time) (job store.Job, err error) {
	err = q.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("status = ? AND run_after <= ?", "pending", now).Order("run_after, id").Take(&job).Error; err != nil {
			return err
		}
		job.Attempts++
		job.Status = "running"
		return tx.Model(&job).Updates(map[string]any{"status": job.Status, "attempts": job.Attempts}).Error
	})
	return
}

// Finish 写入本次结果，并按尝试次数决定退避或终止。
func (q Queue) Finish(ctx context.Context, job store.Job, result store.JobResult, runErr error, now time.Time) error {
	result.Days += job.Result.Days
	result.FailedDays += job.Result.FailedDays
	result.PromptTokens += job.Result.PromptTokens
	result.CompletionTokens += job.Result.CompletionTokens
	result.Slices += job.Result.Slices
	result.DroppedSlices += job.Result.DroppedSlices
	result.FailedSlices += job.Result.FailedSlices
	result.MergeFallbacks += job.Result.MergeFallbacks
	job.Result = result
	job.Status = "done"
	job.LastError = ""
	if runErr != nil {
		job.LastError = runErr.Error()
		job.Status = "dead"
		if job.Attempts < jobMaxAttempts {
			job.Status = "pending"
			job.RunAfter = now.Add(time.Duration(min(1<<uint(min(job.Attempts, 6)), 60)) * time.Minute)
		}
	}
	return q.DB.WithContext(ctx).Save(&job).Error
}

// Counts 是调度 tick 刷新的任务状态计数。
type Counts struct{ Pending, Running, Dead int64 }

// Count 返回任务状态快照。
func (q Queue) Count(ctx context.Context) (Counts, error) {
	var rows []struct {
		Status string
		N      int64
	}
	err := q.DB.WithContext(ctx).Model(&store.Job{}).Select("status, count(*) AS n").Group("status").Scan(&rows).Error
	var c Counts
	for _, r := range rows {
		switch r.Status {
		case "pending":
			c.Pending = r.N
		case "running":
			c.Running = r.N
		case "dead":
			c.Dead = r.N
		}
	}
	return c, err
}
