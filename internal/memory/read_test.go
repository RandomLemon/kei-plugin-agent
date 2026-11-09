package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

func TestContextScopeWindowAndLifecycle(t *testing.T) {
	dsn := memtest.NewDB(t)
	cfg := testConfig(t, dsn)
	now := time.Date(2026, 11, 8, 17, 0, 0, 0, time.UTC) // 上海11月9日01:00，未过04:00日界，仍属11月8日摘要日。
	today := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	s := startService(t, cfg, func(s *Service) { s.now = func() time.Time { return now } })
	db := memtest.Open(t, dsn)
	for _, spec := range []struct {
		conv, title, kind string
		days              int
	}{
		{"group:qq:g1", "昨日", "daily", 1}, {"group:qq:g1", "大前天", "daily", 3},
		{"group:qq:g1", "超出窗口", "daily", 4},
		{"group:qq:g1", "滚动", "rolling", 1}, {"group:qq:g1", "当日滚动", "rolling", 0}, {"group:qq:g1", "前日滚动", "rolling", 2}, {"group:qq:g2", "其他群", "daily", 1},
		{"private:qq:b1:u1", "私聊", "daily", 1}, {"private:qq:b2:u1", "其他机器人私聊", "daily", 1},
	} {
		date := today.AddDate(0, 0, -spec.days)
		summary := store.Summary{ConvKey: spec.conv, LocalDate: date, Kind: spec.kind, Overview: "日概况", Topics: []store.Topic{{ConvKey: spec.conv, LocalDate: date, TopicKey: spec.title, Title: spec.title, Summary: "对应摘要", Importance: 1, KeyPoints: []store.KeyPoint{{Type: "todo", Text: "确认安排", Speaker: "u1", MsgIDs: []string{"m1"}}}, Participants: []string{"u1"}}}}
		if err := db.WithContext(t.Context()).Create(&summary).Error; err != nil {
			t.Fatal(err)
		}
	}
	req := ContextRequest{Scene: Scene{Conv: Conv{Type: ConvTypeGroup, Platform: "qq", ChannelID: "g1"}}, BudgetChars: 1200}
	result, err := s.Context(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Blocks) != 1 {
		t.Fatal(result)
	}
	joined := strings.Join(result.Blocks[0].Lines, " ")
	for _, word := range []string{"昨日", "大前天", "待办：确认安排", "[今天] 当日滚动", "前日滚动"} {
		if !strings.Contains(joined, word) {
			t.Fatalf("漏掉 %s: %s", word, joined)
		}
	}
	for _, word := range []string{"超出窗口", "[11-07] 滚动", "其他群", "私聊"} {
		if strings.Contains(joined, word) {
			t.Fatalf("越界 %s: %s", word, joined)
		}
	}
	req.Scene.Conv = Conv{Type: ConvTypePrivate, Platform: "qq", BotID: "b1", PeerUserID: "u1"}
	result, err = s.Context(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(result.Blocks[0].Lines, " ")
	if !strings.Contains(joined, "私聊") || strings.Contains(joined, "昨日") || strings.Contains(joined, "其他机器人") {
		t.Fatal(joined)
	}
	req.Scene.Conv = Conv{Type: ConvTypeGroup, Platform: "qq", ChannelID: "empty"}
	result, err = s.Context(t.Context(), req)
	if err != nil || len(result.Blocks) != 0 {
		t.Fatalf("空会话 %+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.Context(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx错误 %v", err)
	}
	stopNow(t, s)
	if _, err = s.Context(t.Context(), req); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	fresh := New(cfg, discardLogger(), nil)
	if _, err = fresh.Context(t.Context(), req); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}

func TestContextStopDuringQuery(t *testing.T) {
	dsn := memtest.NewDB(t)
	s := startService(t, testConfig(t, dsn), nil)
	db := memtest.Open(t, dsn)
	tx := db.WithContext(t.Context()).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("LOCK TABLE mem_topic IN ACCESS EXCLUSIVE MODE").Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Context(ctx, ContextRequest{Scene: Scene{Conv: Conv{Type: ConvTypeGroup, Platform: "qq", ChannelID: "g1"}}, BudgetChars: 1200})
		done <- err
	}()
	// 等服务连接在数据库锁上等待后触发 Stop，读取不能延长其生命周期。
	waitFor(t, func() bool {
		var n int64
		db.WithContext(t.Context()).Raw("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%mem_topic AS t%'").Scan(&n)
		return n > 0
	}, "读取等待数据库锁")
	stopNow(t, s)
	if err := <-done; !errors.Is(err, ErrNotReady) {
		t.Fatalf("Stop并发查询应返回ErrNotReady，得到 %v", err)
	}
}

func TestSummaryDateRange(t *testing.T) {
	today := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		from, to, wantFrom, wantTo string
		invalid                    bool
	}{
		{"", "", "2026-11-02", "2026-11-08", false},
		{"2026-10-01", "", "2026-10-01", "2026-10-31", false},
		{"2026-11-01", "", "2026-11-01", "2026-11-08", false},
		{"", "2026-10-15", "2026-10-09", "2026-10-15", false},
		{"2026-10-01", "2026-10-31", "2026-10-01", "2026-10-31", false},
		{"2026-1-01", "", "", "", true}, {"", "2026-02-30", "", "", true},
		{"2026-11-09", "", "", "", true}, {"2026-11-08", "2026-11-07", "", "", true},
		{"2026-10-01", "2026-11-01", "", "", true},
	} {
		from, to, err := summaryDateRange(tc.from, tc.to, today)
		if tc.invalid {
			if !errors.Is(err, ErrInvalidDateRange) {
				t.Errorf("%+v: %v", tc, err)
			}
			continue
		}
		if err != nil || from.Format("2006-01-02") != tc.wantFrom || to.Format("2006-01-02") != tc.wantTo {
			t.Errorf("%+v: %s %s %v", tc, from, to, err)
		}
	}
}

func TestQuerySummariesScopeRankingAndLimits(t *testing.T) {
	dsn := memtest.NewDB(t)
	cfg := testConfig(t, dsn)
	now := time.Date(2026, 11, 8, 17, 0, 0, 0, time.UTC) // 上海9日01点，仍是8日摘要日
	s := startService(t, cfg, func(s *Service) { s.now = func() time.Time { return now } })
	db := memtest.Open(t, dsn)
	today := store.LocalDate(now, cfg.Location, s.dayStart)
	seed := func(conv, kind, title, text string, days int, importance float64) {
		t.Helper()
		date := today.AddDate(0, 0, -days)
		row := store.Summary{ConvKey: conv, LocalDate: date, Kind: kind, Topics: []store.Topic{{ConvKey: conv, LocalDate: date, TopicKey: title, Title: title, Summary: text, Importance: importance, KeyPoints: []store.KeyPoint{{Type: "todo", Text: "确认"}}}}}
		if err := db.WithContext(t.Context()).Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	seed("group:qq:g1", "rolling", "今日", "今天讨论服务器", 0, 0.9)
	seed("group:qq:g1", "daily", "昨日", "聚餐时间与预算", 1, 0.3)
	seed("group:qq:g1", "rolling", "昨日滚动", "聚餐时间与预算", 1, 1)
	seed("group:qq:g1", "daily", "很久以前", "旧安排", 10, 1)
	seed("group:qq:g2", "daily", "其他群", "聚餐时间与预算", 1, 1)
	seed("private:qq:b1:u1", "daily", "私聊", "私事", 1, 1)
	// 同日多话题测试重要性排序与 Limit。
	var row store.Summary
	if err := db.WithContext(t.Context()).Where("conv_key='group:qq:g1' AND kind='rolling' AND local_date=?", today).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	for i := range 11 {
		topic := store.Topic{SummaryID: row.ID, ConvKey: row.ConvKey, LocalDate: today, TopicKey: fmt.Sprint(i), Title: fmt.Sprintf("当日%d", i), Summary: "技术话题", Importance: float64(i) / 20}
		if err := db.WithContext(t.Context()).Create(&topic).Error; err != nil {
			t.Fatal(err)
		}
	}
	req := SummaryQuery{Scene: Scene{Conv: Conv{Type: ConvTypeGroup, Platform: "qq", ChannelID: "g1"}}}
	items, err := s.QuerySummaries(t.Context(), req)
	if err != nil || len(items) != 8 || items[0].Title != "今日" || items[1].Title != "当日10" || items[0].Date != "2026-11-08" || items[0].Kind != "rolling" {
		t.Fatalf("%+v %v", items, err)
	}
	req.Query = "聚餐时间"
	items, err = s.QuerySummaries(t.Context(), req)
	if err != nil || len(items) != 1 || items[0].Title != "昨日" || items[0].Kind != "daily" || len(items[0].OpenItems) != 1 {
		t.Fatalf("%+v %v", items, err)
	}
	req.Query = "没有相关关键词"
	items, err = s.QuerySummaries(t.Context(), req)
	if err != nil || len(items) != 8 || items[0].Title != "今日" {
		t.Fatalf("零相关回落：%+v %v", items, err)
	}
	req.Query = ""
	req.Limit = 99
	items, err = s.QuerySummaries(t.Context(), req)
	if err != nil || len(items) != 10 {
		t.Fatalf("%+v %v", items, err)
	}
	req.Limit = -5
	items, err = s.QuerySummaries(t.Context(), req)
	if err != nil || len(items) != 1 {
		t.Fatalf("%+v %v", items, err)
	}
	req.Limit = 10
	req.DateFrom = "2026-10-01"
	req.DateTo = "2026-10-31"
	items, err = s.QuerySummaries(t.Context(), req)
	if err != nil || len(items) != 1 || items[0].Title != "很久以前" {
		t.Fatalf("窗口外显式查询：%+v %v", items, err)
	}
	req.DateFrom = "bad"
	if _, err = s.QuerySummaries(t.Context(), req); !errors.Is(err, ErrInvalidDateRange) {
		t.Fatal(err)
	}
	req.DateFrom = ""
	req.DateTo = ""
	req.Scene.Conv = Conv{Type: ConvTypePrivate, Platform: "qq", BotID: "b1", PeerUserID: "u1"}
	items, err = s.QuerySummaries(t.Context(), req)
	if err != nil || len(items) != 1 || items[0].Title != "私聊" {
		t.Fatalf("%+v %v", items, err)
	}
	req.Scene.Conv.BotID = "b2"
	items, err = s.QuerySummaries(t.Context(), req)
	if err != nil || len(items) != 0 {
		t.Fatalf("%+v %v", items, err)
	}
	// 相关度优先于日期，日期优先于重要性；同日同相关度再按重要性。
	seed("group:qq:g1", "daily", "更早全匹配", "聚餐时间", 2, 1)
	for _, spec := range []struct {
		days              int
		kind, title, text string
		importance        float64
	}{
		{0, "rolling", "当日部分匹配", "聚餐", 1},
		{1, "daily", "昨日高重要性", "聚餐时间", 0.9},
	} {
		var parent store.Summary
		if err := db.WithContext(t.Context()).Where("conv_key='group:qq:g1' AND local_date=? AND kind=?", today.AddDate(0, 0, -spec.days), spec.kind).First(&parent).Error; err != nil {
			t.Fatal(err)
		}
		topic := store.Topic{SummaryID: parent.ID, ConvKey: parent.ConvKey, LocalDate: parent.LocalDate, TopicKey: spec.title, Title: spec.title, Summary: spec.text, Importance: spec.importance}
		if err := db.WithContext(t.Context()).Create(&topic).Error; err != nil {
			t.Fatal(err)
		}
	}
	req = SummaryQuery{Scene: Scene{Conv: Conv{Type: ConvTypeGroup, Platform: "qq", ChannelID: "g1"}}, Query: "聚餐时间", Limit: 10}
	items, err = s.QuerySummaries(t.Context(), req)
	var titles []string
	for _, item := range items {
		titles = append(titles, item.Title)
	}
	if err != nil || strings.Join(titles, ",") != "昨日高重要性,昨日,更早全匹配,当日部分匹配" {
		t.Fatalf("关键词排序：%v %v", titles, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.QuerySummaries(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	stopNow(t, s)
	if _, err = s.QuerySummaries(t.Context(), req); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if _, err = New(cfg, discardLogger(), nil).QuerySummaries(t.Context(), req); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}
