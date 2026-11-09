package memory

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

func TestIngestRollingTrigger(t *testing.T) {
	dsn := memtest.NewDB(t)
	db := memtest.Open(t, dsn)
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, dsn)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s := New(cfg, discardLogger(), nil)
	s.now = func() time.Time { return now }
	write := func(n, start int, at time.Time) {
		t.Helper()
		var rs []Record
		for i := range n {
			r := groupRecord(fmt.Sprint(start + i))
			r.At = at
			rs = append(rs, r)
		}
		if err := s.writeBatch(t.Context(), db, s.prepare(rs)); err != nil {
			t.Fatal(err)
		}
	}
	count := func(want int64) {
		t.Helper()
		var n int64
		if err := db.WithContext(t.Context()).Model(&store.Job{}).Where("type='rolling_summary'").Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("jobs=%d want=%d", n, want)
		}
	}
	write(199, 0, now)
	count(0)
	write(1, 199, now)
	count(1)
	write(10, 200, now)
	count(1)
	// 任务完成才推进序号；六次之后停止触发。
	date := store.LocalDate(now, cfg.Location, s.dayStart)
	for run := 1; run <= 6; run++ {
		if err := db.WithContext(t.Context()).Model(&store.ConvDay{}).Where("local_date=?", date).Updates(map[string]any{"rolling_runs": run, "rolled_msg_count": 210 + (run-1)*200}).Error; err != nil {
			t.Fatal(err)
		}
		write(200, 210+(run-1)*200, now)
		count(int64(min(run+1, 6)))
	}
	write(200, 5000, now.AddDate(0, 0, -1))
	count(6)
}

// 真实 Service 写入→worker→滚动→推进时钟→调度日摘要，不调用真实聊天服务。
func TestRuntimeRollingLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"topics":[{"title":"聚餐","summary":"商量聚餐地点与预算","importance":0.8}]}`}}}})
	}))
	defer server.Close()
	dsn := memtest.NewDB(t)
	db := memtest.Open(t, dsn)
	cfg := testConfig(t, dsn)
	cfg.LLMBaseURL = server.URL
	cfg.LLMModelSmall = "test"
	cfg.LLMModelLarge = "test"
	cfg.LLMTimeout = time.Second
	local := time.Now().In(cfg.Location).AddDate(0, 0, -2)
	initial := time.Date(local.Year(), local.Month(), local.Day(), 1, 0, 0, 0, cfg.Location) // 凌晨消息归前一摘要日
	var clock atomic.Int64
	clock.Store(initial.Unix())
	s := startService(t, cfg, func(s *Service) {
		s.now = func() time.Time { return time.Unix(clock.Load(), 0) }
		s.workerIdleWait = time.Millisecond
		s.tickInterval = 10 * time.Millisecond
	})
	for i := range 200 {
		r := groupRecord(fmt.Sprint(i))
		r.At = initial.Add(-time.Duration(200-i) * time.Second)
		r.Text = fmt.Sprintf("聚餐讨论%d：我们确认聚餐地点和人均预算，周末一起坐地铁过去。", i)
		if !s.Enqueue(r) {
			t.Fatal("queue full")
		}
		if i%50 == 49 {
			want := int64(i + 1)
			waitFor(t, func() bool { return s.Stats().Written >= want }, "写入")
		}
	}
	waitFor(t, func() bool {
		var n int64
		db.WithContext(t.Context()).Model(&store.Job{}).Where("type='rolling_summary' AND status='done'").Count(&n)
		return n == 1
	}, "滚动完成")
	var rolling store.Summary
	if err := db.WithContext(t.Context()).Where("kind='rolling'").First(&rolling).Error; err != nil {
		t.Fatal(err)
	}
	if rolling.LocalDate.Format("2006-01-02") != store.LocalDate(initial, cfg.Location, 4*time.Hour).Format("2006-01-02") {
		t.Fatal(rolling)
	}
	clock.Store(initial.Add(3*time.Hour + time.Minute).Unix()) // 04:01
	waitFor(t, func() bool {
		var n int64
		db.WithContext(t.Context()).Model(&store.Job{}).Where("type='daily_summary' AND status='done'").Count(&n)
		return n == 1
	}, "日摘要替换")
	var n int64
	if err := db.WithContext(t.Context()).Model(&store.Summary{}).Where("kind='rolling'").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("滚动未删除")
	}
	t.Log("200 条凌晨消息归入前一摘要日，生成 rolling，04:00 日界后调度 daily 并替换 rolling")
}
