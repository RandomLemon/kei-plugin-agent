package memory

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

// TestRuntimeHostCleanup 用真实宿主、真实时钟与 mock 控制面验收 S3。
// PERSONA_MEMORY_HOST_BINARY 指向事先构建的 devhost；普通质量门不重复等待分钟调度。
func TestRuntimeHostCleanup(t *testing.T) {
	binary := os.Getenv("PERSONA_MEMORY_HOST_BINARY")
	if binary == "" {
		t.Skip("未指定运行验收宿主")
	}
	dsn := memtest.NewDB(t)
	db := memtest.Open(t, dsn)
	at := time.Now().UTC().Add(time.Minute).Format("15:04")
	host := startRuntimeHostFor(t, binary, dsn, 150*time.Second,
		"KEI_PLUGINS_PERSONA_GROUP_LIST=[g1]",
		"KEI_PLUGINS_PERSONA_RANDOM_TIMEZONE=UTC",
		"KEI_PLUGINS_PERSONA_MEMORY_DAILY_AT="+at,
	)
	deadline := time.Now().Add(130 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("宿主定时清理未完成")
		}
		if db.Migrator().HasTable(&store.Job{}) {
			var n int64
			if err := db.WithContext(t.Context()).Model(&store.Job{}).Where("type='cleanup' AND status='done'").Count(&n).Error; err == nil && n == 1 {
				break
			}
		}
		time.Sleep(time.Second)
	}
	host.waitReady(t, 15*time.Second)
	if !strings.Contains(host.sentBody(), "任务：待执行") {
		t.Fatal("宿主 status 未出现任务行")
	}
	t.Logf("宿主 daily_at=%s：cleanup=done，status 输出任务行", at)
}
