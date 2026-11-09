package summary

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

func TestMergeRollingContinuity(t *testing.T) {
	date := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	oldPoint := store.KeyPoint{Type: "todo", Text: "订桌", Speaker: "u1"}
	old := store.Topic{TopicKey: "stable", ContinuesFrom: "yesterday", Title: "旧标题", Participants: []string{"u1"}, Entities: []string{"旧实体"}, KeyPoints: []store.KeyPoint{oldPoint}, FirstMsgID: 1, FirstAt: date, LastMsgID: 3}
	source := []store.Topic{{Title: "新增", Summary: "新进展", Participants: []string{"u2"}, Entities: []string{"新实体"}, KeyPoints: []store.KeyPoint{oldPoint, {Type: "open_question", Text: "时间", Speaker: "u2"}}, FirstMsgID: 4, LastMsgID: 6, FirstAt: date.Add(time.Hour), LastAt: date.Add(2 * time.Hour), Importance: 0.8}}
	source = append(source, source[0], source[0], source[0])
	output := MergeOutput{Topics: []MergedTopic{
		{From: []string{"s1"}, Title: "新标题", Summary: "全部进展", Continues: "t1", KeyPoints: []string{"s1.0", "s1.1"}},
		{From: []string{"s2"}, Continues: "t1"},
		{From: []string{"s3"}, Continues: "k1"},
		{From: []string{"s4"}, Continues: "k2"},
	}}
	got, _ := MergeRolling(output, source, []store.Topic{{TopicKey: "stable"}, {TopicKey: "other"}}, []store.Topic{old, {TopicKey: "untouched", Title: "保留"}}, "c", date, 2, false)
	if len(got) != 5 || got[0].TopicKey != "stable" || got[0].ContinuesFrom != "yesterday" || got[0].FirstMsgID != 1 || !got[0].FirstAt.Equal(date) || got[0].LastMsgID != 6 || got[0].Title != "新标题" || len(got[0].Participants) != 2 || len(got[0].Entities) != 2 || len(got[0].KeyPoints) != 2 || !got[0].HasOpen || got[1].TopicKey != "topic:c:20261108:r2.1" || got[2].TopicKey != "topic:c:20261108:r2.2" || got[3].TopicKey != "other" || got[4].Title != "保留" {
		t.Fatalf("%+v", got)
	}
	input := renderMerge(source, nil, old)
	if !strings.Contains(input, `"Today":[{"ID":"t1","Title":"旧标题","Summary":""}]`) {
		t.Fatal(input)
	}
}

func TestMergeRollingEmptyOutputKeepsOldText(t *testing.T) {
	date := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	old := store.Topic{TopicKey: "stable", Title: "聚餐", Summary: "上午定了地点，下午定了预算"}
	source := []store.Topic{{Title: "预算", Summary: "下午定了预算", Importance: 0.8}}
	output := MergeOutput{Topics: []MergedTopic{{From: []string{"s1"}, Continues: "t1"}, {From: []string{"s1"}}}}
	got, _ := MergeRolling(output, source, nil, []store.Topic{old}, "c", date, 2, false)
	if len(got) != 1 || got[0].TopicKey != "stable" || got[0].Title != old.Title || got[0].Summary != old.Summary {
		t.Fatalf("%+v", got)
	}
	// 新话题仍按 S4 用切片兜底。
	got, _ = MergeRolling(MergeOutput{Topics: []MergedTopic{{From: []string{"s1"}}}}, source, nil, []store.Topic{old}, "c", date, 2, false)
	if len(got) != 2 || got[0].Title != "预算" || got[0].Summary != "下午定了预算" {
		t.Fatalf("%+v", got)
	}
}

func TestPipelineRollingIncrementalAndDailyReplacement(t *testing.T) {
	var calls atomic.Int64
	pipeline, db := testPipeline(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Model    string
			Messages []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model == "large" {
			mockAnswer(w, `{"overview":"聚餐进展","topics":[{"from":["s1"],"summary":"已确认地点","continues":"t1","key_points":["s1.0"]}]}`)
		} else {
			mockAnswer(w, sliceAnswer)
		}
	}))
	date := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC) // 故意不是今天，待执行任务仍需写入。
	seedDay(t, db, date)
	job := store.Job{Payload: store.JobPayload{Run: 1, ConvKey: "c", Date: "2026-11-07"}}
	if result, err := pipeline.Rolling(t.Context(), job); err != nil || result.Days != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	var first store.Summary
	if err := db.WithContext(t.Context()).Preload("Topics").First(&first).Error; err != nil {
		t.Fatal(err)
	}
	if first.Topics[0].TopicKey != "topic:c:20261107:r1.1" {
		t.Fatal(first.Topics)
	}
	// 摘要已提交而任务状态尚未落库时，重放固定 run 必须幂等。
	if _, err := pipeline.Rolling(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	var replay store.Summary
	if err := db.WithContext(t.Context()).First(&replay).Error; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || replay.ID != first.ID || replay.Version != first.Version {
		t.Fatalf("重复执行：%+v calls=%d", replay, calls.Load())
	}

	var messages []store.Message
	if err := db.WithContext(t.Context()).Order("id").Find(&messages).Error; err != nil {
		t.Fatal(err)
	}
	for i := range messages {
		messages[i].ID = 0
		messages[i].DedupeKey += "new"
		messages[i].MsgID += "new"
		messages[i].SentAt = messages[i].SentAt.Add(time.Hour)
	}
	if err := db.WithContext(t.Context()).Create(&messages).Error; err != nil {
		t.Fatal(err)
	}
	upto := messages[len(messages)-1].ID
	if err := db.WithContext(t.Context()).Model(&store.ConvDay{}).Where("conv_key='c'").Updates(map[string]any{"msg_count": 2 * len(messages), "last_msg_id": upto}).Error; err != nil {
		t.Fatal(err)
	}
	job.Payload.Run = 2
	if _, err := pipeline.Rolling(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	var second store.Summary
	if err := db.WithContext(t.Context()).Preload("Topics").First(&second).Error; err != nil {
		t.Fatal(err)
	}
	var day store.ConvDay
	if err := db.WithContext(t.Context()).First(&day).Error; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || second.Version != 2 || second.FromMsgID != first.FromMsgID || second.ToMsgID != upto || second.MsgCount != 2*len(messages) || second.Topics[0].TopicKey != first.Topics[0].TopicKey || second.Topics[0].FirstMsgID != first.Topics[0].FirstMsgID || day.RollingRuns != 2 || day.RolledMsgCount != 2*len(messages) || day.RolledUptoMsgID != upto {
		t.Fatalf("calls=%d summary=%+v day=%+v", calls.Load(), second, day)
	}
	if _, err := pipeline.Daily(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	if _, err := pipeline.Rolling(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.WithContext(t.Context()).Model(&store.Summary{}).Where("kind='rolling'").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 || calls.Load() != before {
		t.Fatal("日摘要后仍滚动")
	}
}

func TestRollingDailyWinsDuringModelCall(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "topics", true: "empty"}[empty], func(t *testing.T) {
			var commitDaily func()
			pipeline, db := testPipeline(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { commitDaily(); mockAnswer(w, sliceAnswer) }))
			date := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
			seedDay(t, db, date)
			old := store.Summary{ConvKey: "c", LocalDate: date, Kind: "rolling", Topics: []store.Topic{{ConvKey: "c", LocalDate: date, TopicKey: "old", Title: "旧"}}}
			if err := db.WithContext(t.Context()).Create(&old).Error; err != nil {
				t.Fatal(err)
			}
			commitDaily = func() {
				var day store.ConvDay
				if err := db.WithContext(t.Context()).First(&day).Error; err != nil {
					t.Error(err)
					return
				}
				var topics []store.Topic
				if !empty {
					topics = []store.Topic{{ConvKey: "c", LocalDate: date, TopicKey: "daily", Title: "日摘要"}}
				}
				if err := pipeline.write(t.Context(), day, nil, topics, "概况", "group", "small", false); err != nil {
					t.Error(err)
				}
			}
			if _, err := pipeline.Rolling(t.Context(), store.Job{Payload: store.JobPayload{Run: 1, ConvKey: "c", Date: "2026-11-07"}}); err != nil {
				t.Fatal(err)
			}
			var n int64
			if err := db.WithContext(t.Context()).Model(&store.Summary{}).Where("kind='rolling'").Count(&n).Error; err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatal("滚动覆盖了已完成的日摘要")
			}
		})
	}
}

func TestSelectRollingLines(t *testing.T) {
	today := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	since := today.Add(9 * time.Hour)
	cs := []Candidate{
		{Topic: store.Topic{ID: 1, TopicKey: "stable", LocalDate: today, FirstAt: since, Title: "短期已有", Importance: 1}, Kind: "rolling"},
		{Topic: store.Topic{ID: 2, TopicKey: "stable", LocalDate: today.AddDate(0, 0, -1), Title: "昨日重点", Summary: "完整摘要", Importance: 1}, Kind: "daily", Overview: "日概况"},
		{Topic: store.Topic{ID: 3, TopicKey: "rolling", LocalDate: today, FirstAt: since.Add(-time.Hour), Title: "今日低分", Importance: 0.01}, Kind: "rolling"},
		{Topic: store.Topic{ID: 4, TopicKey: "older", LocalDate: today.AddDate(0, 0, -2), Title: "早前滚动", Importance: 0.01}, Kind: "rolling"},
	}
	lines := SelectLines(cs, nil, today, 1200, since)
	if len(lines) != 4 || !strings.Contains(lines[0], "[今天] 今日低分") || !strings.Contains(lines[1], "早前滚动") || lines[2] != "[11-07 概况] 日概况" || !strings.Contains(lines[3], "昨日重点") {
		t.Fatal(lines)
	}
	if strings.Contains(strings.Join(SelectLines(cs, nil, today, 1200, time.Time{}), " "), "昨日重点") {
		t.Fatal("零值未保留今日延续话题")
	}
}

func TestRollingKeepsLatestDailyOverview(t *testing.T) {
	today := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	cs := []Candidate{{Topic: store.Topic{TopicKey: "stable", LocalDate: today, Title: "今天进展"}, Kind: "rolling"}, {Topic: store.Topic{TopicKey: "stable", LocalDate: today.AddDate(0, 0, -1), Title: "昨天话题"}, Kind: "daily", Overview: "昨天概况"}}
	lines := SelectLines(cs, nil, today, 1200, time.Time{})
	if len(lines) != 2 || lines[1] != "[11-07 概况] 昨天概况" {
		t.Fatal(lines)
	}
}
