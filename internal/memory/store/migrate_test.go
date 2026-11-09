package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
)

func TestMigrateIdempotent(t *testing.T) {
	db := memtest.Open(t, memtest.NewDB(t))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for i := range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("第 %d 次迁移: %v", i+1, err)
		}
	}

	for _, table := range []string{"mem_conversation", "mem_message", "mem_conv_day", "mem_job", "mem_summary", "mem_topic"} {
		if !db.Migrator().HasTable(table) {
			t.Errorf("缺少表 %s", table)
		}
	}
	var indexes []string
	if err := db.WithContext(ctx).Raw(
		`SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() `,
	).Scan(&indexes).Error; err != nil {
		t.Fatal(err)
	}
	for _, idx := range []string{"uq_mem_message_dedupe", "idx_mem_message_day", "idx_mem_message_msg_id", "uq_mem_job_dedupe", "idx_mem_job_pending", "uq_mem_summary_day", "idx_mem_topic_day", "idx_mem_topic_key"} {
		if !slices.Contains(indexes, idx) {
			t.Errorf("缺少索引 %s，现有 %v", idx, indexes)
		}
	}
}
