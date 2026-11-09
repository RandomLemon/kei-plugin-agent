package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/llm"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"gorm.io/gorm"
)

func testPipeline(t *testing.T, handler http.Handler) (Pipeline, *gorm.DB) {
	t.Helper()
	db := memtest.Open(t, memtest.NewDB(t))
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	pipeline := Pipeline{DB: db, Small: "small", Large: "large", Location: time.UTC, Now: func() time.Time { return now }, Log: log, Client: llm.New(llm.Config{BaseURL: server.URL, Timeout: time.Second, JSONMode: true}, server.Client(), log)}
	conv := store.Conversation{ConvKey: "c", Type: "group", Name: "测试群"}
	if err := db.WithContext(t.Context()).Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	return pipeline, db
}

func seedDay(t *testing.T, db *gorm.DB, date time.Time) {
	t.Helper()
	msgs := sample()
	for n := range msgs {
		msgs[n].ID = 0
		msgs[n].ConvKey = "c"
		msgs[n].DedupeKey = date.String() + msgs[n].MsgID
		msgs[n].LocalDate = date
		msgs[n].SentAt = date.Add(9*time.Hour + time.Duration(n)*time.Minute)
		msgs[n].Mentions = []string{}
	}
	if err := db.WithContext(t.Context()).Create(&msgs).Error; err != nil {
		t.Fatal(err)
	}
	d := store.ConvDay{ConvKey: "c", LocalDate: date, MsgCount: len(msgs), LastMsgID: msgs[len(msgs)-1].ID}
	if err := db.WithContext(t.Context()).Create(&d).Error; err != nil {
		t.Fatal(err)
	}
}

func mockAnswer(w http.ResponseWriter, content string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
}

const sliceAnswer = `{"topics":[{"title":"聚餐","summary":"张三和李四讨论聚餐地点","participants":["u1","u2"],"key_points":[{"type":"todo","text":"张三确认地点","speaker":"u1","refs":[1]}],"importance":0.8}]}`

func TestPipelineDailyIdempotencyContinuityAndEmpty(t *testing.T) {
	var sawKnown atomic.Bool
	pipeline, db := testPipeline(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string
			Messages []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model == "small" {
			mockAnswer(w, sliceAnswer)
		} else {
			if strings.Contains(req.Messages[1].Content, "topic:c:20261106:1") {
				sawKnown.Store(true)
			}
			mockAnswer(w, `{"overview":"聚餐，联系 x@y.com","topics":[{"from":["s1"],"title":"聚餐","summary":"联系 13812345678","key_points":["s1.0"],"continues":"k1"}]}`)
		}
	}))
	d1 := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	d2 := d1.AddDate(0, 0, 1)
	seedDay(t, db, d1)
	seedDay(t, db, d2)
	job := store.Job{Payload: store.JobPayload{ConvKey: "c"}}
	result, err := pipeline.Daily(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if result.Days != 2 || result.PromptTokens != 30 || !sawKnown.Load() {
		t.Fatalf("result=%+v known=%v", result, sawKnown.Load())
	}
	var summaries []store.Summary
	if err = db.WithContext(t.Context()).Preload("Topics").Order("local_date").Find(&summaries).Error; err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 || summaries[0].Topics[0].TopicKey != summaries[1].Topics[0].TopicKey || strings.Contains(summaries[1].Overview, "x@y.com") || strings.Contains(summaries[1].Topics[0].Summary, "13812345678") {
		t.Fatalf("摘要 %+v", summaries)
	}
	if err = db.WithContext(t.Context()).Model(&store.ConvDay{}).Where("local_date=?", d2).Update("summarized_upto_id", nil).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = pipeline.Daily(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	var summary store.Summary
	if err = db.WithContext(t.Context()).Where("local_date=?", d2).First(&summary).Error; err != nil {
		t.Fatal(err)
	}
	if summary.Version != 2 {
		t.Fatalf("version=%d", summary.Version)
	}
	if err = db.WithContext(t.Context()).Model(&store.Message{}).Where("local_date=?", d2).Update("text", "[图片]").Error; err != nil {
		t.Fatal(err)
	}
	if err = db.WithContext(t.Context()).Model(&store.ConvDay{}).Where("local_date=?", d2).Update("summarized_upto_id", nil).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = pipeline.Daily(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err = db.WithContext(t.Context()).Model(&store.Summary{}).Where("local_date=?", d2).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("空结果未清除旧摘要")
	}
	if err = db.WithContext(t.Context()).Model(&store.Topic{}).Where("local_date=?", d2).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("话题未级联删除")
	}
}

func TestPipelineFailedDayContinuesAndRetry(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	pipeline, db := testPipeline(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string
			Messages []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if fail.Load() && strings.Contains(req.Messages[1].Content, "2026-11-06") {
			mockAnswer(w, `{"topics":null}`)
			return
		}
		if req.Model == "large" {
			mockAnswer(w, `{"topics":[]}`)
		} else {
			mockAnswer(w, sliceAnswer)
		}
	}))
	d1 := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	seedDay(t, db, d1)
	seedDay(t, db, d1.AddDate(0, 0, 1))
	result, err := pipeline.Daily(t.Context(), store.Job{Payload: store.JobPayload{ConvKey: "c"}})
	if err == nil || result.Days != 1 || result.FailedDays != 1 || result.FailedSlices != 1 || result.PromptTokens != 30 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	fail.Store(false)
	result, err = pipeline.Daily(t.Context(), store.Job{Payload: store.JobPayload{ConvKey: "c"}})
	if err != nil || result.Days != 1 {
		t.Fatalf("未仅重试失败日期：%+v %v", result, err)
	}
}

// 两个切片中一个失败：输出不合规时当天照常写入；HTTP 错误时当天失败、不设进度，留待重试。
func TestPipelineSliceErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fail     func(http.ResponseWriter)
		dayFails bool
	}{
		{"invalid_output", func(w http.ResponseWriter) { mockAnswer(w, `{"topics":null}`) }, false},
		{"http_error", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadRequest) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline, db := testPipeline(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model    string
					Messages []struct{ Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				switch {
				case req.Model == "large":
					mockAnswer(w, `{"overview":"聚餐","topics":[{"from":["s1"],"title":"聚餐","summary":"讨论聚餐"}]}`)
				case strings.Contains(req.Messages[1].Content, " 11:0"):
					tc.fail(w)
				default:
					mockAnswer(w, sliceAnswer)
				}
			}))
			date := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
			var messages []store.Message
			for n := range 12 {
				m := sample()[n%6]
				m.ID = 0
				m.ConvKey = "c"
				m.MsgID = fmt.Sprint(n)
				m.DedupeKey = m.MsgID
				m.Mentions = []string{}
				m.LocalDate = date
				m.SentAt = date.Add(time.Duration(9+n/6*2)*time.Hour + time.Duration(n%6)*time.Minute)
				messages = append(messages, m)
			}
			if err := db.WithContext(t.Context()).Create(&messages).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.WithContext(t.Context()).Create(&store.ConvDay{ConvKey: "c", LocalDate: date, MsgCount: len(messages), LastMsgID: messages[len(messages)-1].ID}).Error; err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Daily(t.Context(), store.Job{Payload: store.JobPayload{ConvKey: "c"}})
			if result.Slices != 2 || result.FailedSlices != 1 || (err != nil) != tc.dayFails {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			var day store.ConvDay
			if err = db.WithContext(t.Context()).Where("conv_key = ?", "c").First(&day).Error; err != nil {
				t.Fatal(err)
			}
			var n int64
			if err = db.WithContext(t.Context()).Model(&store.Summary{}).Count(&n).Error; err != nil {
				t.Fatal(err)
			}
			if tc.dayFails != (day.SummarizedUptoID == nil) || tc.dayFails != (n == 0) {
				t.Fatalf("进度=%v 摘要数=%d", day.SummarizedUptoID, n)
			}
		})
	}
}

func TestPipelineMergeFallback(t *testing.T) {
	pipeline, db := testPipeline(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model == "large" {
			mockAnswer(w, `{}`)
		} else {
			mockAnswer(w, `{"topics":[{"title":"A","summary":"信息A","importance":0.8},{"title":"B","summary":"信息B","importance":0.2}]}`)
		}
	}))
	seedDay(t, db, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC))
	result, err := pipeline.Daily(t.Context(), store.Job{Payload: store.JobPayload{ConvKey: "c"}})
	if err != nil || result.MergeFallbacks != 1 || result.PromptTokens != 30 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var n int64
	if err = db.WithContext(t.Context()).Model(&store.Topic{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatal("兜底未保留全部来源")
	}
}

func TestPipelineTimeoutKeepsUsage(t *testing.T) {
	var calls atomic.Int64
	pipeline, db := testPipeline(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			mockAnswer(w, sliceAnswer)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	date := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	var messages []store.Message
	for n := range 18 {
		m := sample()[n%6]
		m.ID = 0
		m.ConvKey = "c"
		m.MsgID = fmt.Sprint(n)
		m.DedupeKey = m.MsgID
		m.Mentions = []string{}
		m.LocalDate = date
		m.SentAt = date.Add(9*time.Hour + time.Duration(n/3)*40*time.Minute + time.Duration(n%3)*time.Minute)
		messages = append(messages, m)
	}
	if err := db.WithContext(t.Context()).Create(&messages).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(t.Context()).Create(&store.ConvDay{ConvKey: "c", LocalDate: date, MsgCount: len(messages), LastMsgID: messages[len(messages)-1].ID}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	result, err := pipeline.Daily(ctx, store.Job{Payload: store.JobPayload{ConvKey: "c"}})
	if err == nil || result.PromptTokens < 10 || result.CompletionTokens < 5 {
		t.Fatalf("usage 被丢失：%+v %v", result, err)
	}
}
