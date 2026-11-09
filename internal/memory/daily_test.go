package memory

import (
	"encoding/json"
	"fmt"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestScheduledDailySummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"topics":[{"title":"聚餐","summary":"商量周末聚餐与预算","importance":0.8}]}`}}}, "usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 3}})
	}))
	defer server.Close()
	dsn := memtest.NewDB(t)
	db := memtest.Open(t, dsn)
	cfg := testConfig(t, dsn)
	cfg.LLMBaseURL = server.URL
	cfg.LLMModelSmall = "test"
	cfg.LLMModelLarge = "test"
	cfg.LLMTimeout = time.Second
	cfg.LLMJSONMode = true
	cfg.DailyAt = "00:00"
	s := startService(t, cfg, func(s *Service) { s.tickInterval = 10 * time.Millisecond; s.workerIdleWait = time.Millisecond })
	for n := range 4 {
		record := groupRecord(fmt.Sprint(n))
		record.At = time.Now().Add(-24 * time.Hour)
		record.Text = fmt.Sprintf("消息%d：我们周末讨论聚餐地点和具体时间，预算人均一百元，可以坐地铁过去。", n)
		s.Enqueue(record)
	}
	waitFor(t, func() bool {
		var n int64
		db.WithContext(t.Context()).Model(&store.Summary{}).Where("kind='daily'").Count(&n)
		return n == 1
	}, "调度日摘要生成")
	var job store.Job
	waitFor(t, func() bool {
		err := db.WithContext(t.Context()).Where("type='daily_summary' AND status='done'").First(&job).Error
		return err == nil
	}, "摘要任务完成")
	if job.Result.PromptTokens != 4 || job.Result.Days != 1 {
		t.Fatalf("job=%+v", job)
	}
	stopNow(t, s)
}
