package store

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// indexStatements 是 AutoMigrate 之后幂等创建的索引。
var indexStatements = []string{
	`CREATE UNIQUE INDEX IF NOT EXISTS uq_mem_summary_day ON mem_summary (conv_key, local_date, kind)`,
	`CREATE INDEX IF NOT EXISTS idx_mem_topic_day ON mem_topic (conv_key, local_date)`,
	`CREATE INDEX IF NOT EXISTS idx_mem_topic_key ON mem_topic (conv_key, topic_key)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS uq_mem_job_dedupe ON mem_job (dedupe_key)`,
	`CREATE INDEX IF NOT EXISTS idx_mem_job_pending ON mem_job (status, run_after)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS uq_mem_message_dedupe ON mem_message (conv_key, dedupe_key)`,
	`CREATE INDEX IF NOT EXISTS idx_mem_message_day ON mem_message (conv_key, local_date, sent_at, id)`,
	`CREATE INDEX IF NOT EXISTS idx_mem_message_msg_id ON mem_message (conv_key, msg_id) WHERE msg_id <> ''`,
}

// Migrate 在一个事务内建表与索引，可重复执行。
// 插件单实例部署，不加跨进程锁。
func Migrate(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(&Conversation{}, &Message{}, &ConvDay{}, &Job{}, &Summary{}, &Topic{}); err != nil {
			return fmt.Errorf("auto migrate: %w", err)
		}
		for _, stmt := range indexStatements {
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("create index: %w", err)
			}
		}
		return nil
	})
}
