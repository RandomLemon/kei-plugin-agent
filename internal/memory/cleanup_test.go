package memory

import (
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"testing"
	"time"
)

func TestScheduledCleanupAndRecovery(t *testing.T) {
	dsn := memtest.NewDB(t)
	db := memtest.Open(t, dsn)
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, days := range []int{-100, 0} {
		msg := store.Message{Mentions: []string{}, ConvKey: "c", DedupeKey: time.Duration(days).String(), SentAt: now.AddDate(0, 0, days), LocalDate: store.LocalDate(now.AddDate(0, 0, days), time.UTC, 0), ReceivedAt: now}
		if err := db.WithContext(t.Context()).Create(&msg).Error; err != nil {
			t.Fatal(err)
		}
		day := store.ConvDay{ConvKey: "c", LocalDate: msg.LocalDate}
		if err := db.WithContext(t.Context()).Create(&day).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, j := range []store.Job{
		{Type: "cleanup", DedupeKey: "interrupted", Status: "running", Attempts: 1, RunAfter: now},
		{Type: "cleanup", DedupeKey: "old", Status: "done", UpdatedAt: now.AddDate(0, 0, -20), RunAfter: now},
		{Type: "cleanup", DedupeKey: "recent", Status: "done", UpdatedAt: now, RunAfter: now},
		{Type: "cleanup", DedupeKey: "dead", Status: "dead", UpdatedAt: now.AddDate(0, 0, -20), RunAfter: now},
	} {
		if err := db.WithContext(t.Context()).Create(&j).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := startService(t, testConfig(t, dsn), func(s *Service) {
		s.cfg.DailyAt = "00:00"
		s.dayStart = 0
		s.tickInterval = 10 * time.Millisecond
		s.workerIdleWait = 10 * time.Millisecond
	})
	waitFor(t, func() bool {
		var n int64
		db.WithContext(t.Context()).Model(&store.Job{}).Where("dedupe_key LIKE 'cleanup:%' AND status='done'").Count(&n)
		return n == 1
	}, "定时 cleanup 完成")
	var recovered store.Job
	if err := db.WithContext(t.Context()).Where("dedupe_key='interrupted'").First(&recovered).Error; err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "done" || recovered.Attempts != 2 {
		t.Fatalf("恢复 %+v", recovered)
	}
	for _, model := range []any{&store.Message{}, &store.ConvDay{}} {
		var n int64
		if err := db.WithContext(t.Context()).Model(model).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("清理后行数 %d", n)
		}
	}
	var old, recent int64
	if err := db.WithContext(t.Context()).Model(&store.Job{}).Where("dedupe_key IN ('old','dead')").Count(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(t.Context()).Model(&store.Job{}).Where("dedupe_key='recent'").Count(&recent).Error; err != nil {
		t.Fatal(err)
	}
	if old != 0 || recent != 1 {
		t.Fatalf("old=%d recent=%d", old, recent)
	}
	if err := db.WithContext(t.Context()).Create(&store.Job{Type: "cleanup", DedupeKey: "future", Status: "pending", RunAfter: now.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Stats().JobsPending == 1 }, "任务计数刷新")
	stopNow(t, s)
}

func TestServiceStopCancelsRunningCleanup(t *testing.T) {
	dsn := memtest.NewDB(t)
	db := memtest.Open(t, dsn)
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	s := startService(t, testConfig(t, dsn), func(s *Service) { s.workerIdleWait = time.Millisecond })
	tx := db.WithContext(t.Context()).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("LOCK TABLE mem_message IN ACCESS EXCLUSIVE MODE").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(t.Context()).Create(&store.Job{Type: "cleanup", DedupeKey: "blocked", Status: "pending", RunAfter: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var n int64
		db.WithContext(t.Context()).Model(&store.Job{}).Where("status='running'").Count(&n)
		return n == 1
	}, "cleanup 执行中")
	begin := time.Now()
	stopNow(t, s)
	if time.Since(begin) > workerStopTimeout+time.Second {
		t.Fatal("Stop 超时")
	}
	var job store.Job
	if err := db.WithContext(t.Context()).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != "running" {
		t.Fatalf("status=%s", job.Status)
	}
}
