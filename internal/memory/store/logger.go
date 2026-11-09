package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// slowSQLThreshold 是记慢查询日志的耗时阈值。
const slowSQLThreshold = time.Second

// gormLogger 把 gorm 日志转到 slog，只记出错与慢查询，不输出 SQL 文本与参数。
type gormLogger struct {
	log *slog.Logger
}

// NewLogger 返回写入 log 的 gorm 日志适配器。
func NewLogger(log *slog.Logger) logger.Interface {
	return gormLogger{log: log}
}

// LogMode 实现 logger.Interface；级别固定，忽略设置。
func (l gormLogger) LogMode(logger.LogLevel) logger.Interface { return l }

// Info 实现 logger.Interface；不记录。
func (l gormLogger) Info(context.Context, string, ...any) {}

// Warn 实现 logger.Interface。
func (l gormLogger) Warn(ctx context.Context, msg string, data ...any) {
	l.log.WarnContext(ctx, "persona memory: gorm 警告", "operation", "memory_sql", "err", fmt.Sprintf(msg, data...))
}

// Error 实现 logger.Interface。
func (l gormLogger) Error(ctx context.Context, msg string, data ...any) {
	l.log.WarnContext(ctx, "persona memory: gorm 错误", "operation", "memory_sql", "err", fmt.Sprintf(msg, data...))
}

// Trace 实现 logger.Interface：出错或耗时超过阈值时记 Warn。
// ErrRecordNotFound 与 context.Canceled（停止时打断在途语句）不算出错。
func (l gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)
	failed := err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && !errors.Is(err, context.Canceled)
	if !failed && elapsed <= slowSQLThreshold {
		return
	}
	_, rows := fc()
	attrs := []any{"operation", "memory_sql", "duration_ms", elapsed.Milliseconds(), "rows", rows}
	if failed {
		attrs = append(attrs, "err", err)
	}
	l.log.WarnContext(ctx, "persona memory: SQL 执行异常", attrs...)
}
