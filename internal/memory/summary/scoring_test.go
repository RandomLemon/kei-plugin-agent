package summary

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

func TestKeywordRelevance(t *testing.T) {
	for _, tc := range []struct {
		query, text string
		positive    bool
	}{
		{"周末去哪聚餐", "聚餐地点：城东", true}, {"周末去哪聚餐", "服务器升级", false}, {"", "聚餐", false}, {"[图片]", "[图片]", false}, {"[图片] 聚餐", "聚餐地点", true}, {"A B!", "ab", true}, {"餐", "聚餐", false}, {"餐", "餐", true},
	} {
		got := KeywordRelevance(tc.query, tc.text)
		if (got > 0) != tc.positive {
			t.Errorf("%q/%q = %f", tc.query, tc.text, got)
		}
	}
}

func TestTopicScore(t *testing.T) {
	today := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	for days := 1; days <= 3; days++ {
		topic := store.Topic{LocalDate: today.AddDate(0, 0, -days), Importance: 1, Title: "聚餐"}
		score := topicScore(topic, nil, today)
		if math.Abs(score-math.Pow(0.5, float64(days)/3)) > 1e-9 {
			t.Fatal(score)
		}
	}
	topic := store.Topic{LocalDate: today.AddDate(0, 0, -1), Importance: 1, Title: "聚餐"}
	a := topicScore(topic, []string{"无关", "聚餐"}, today)
	b := topicScore(topic, []string{"聚餐"}, today)
	if a != b || a <= topicScore(topic, nil, today) {
		t.Fatal("多条查询未取最大相关度")
	}
}

func TestSelectLines(t *testing.T) {
	today := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	candidate := func(id int64, key, title string, days int, importance float64) Candidate {
		return Candidate{Topic: store.Topic{ID: id, TopicKey: key, Title: title, Summary: "一句话。详细内容", LocalDate: today.AddDate(0, 0, -days), Importance: importance}, Overview: "昨天概况"}
	}
	current := candidate(2, "stable", "新话题", 1, 0.9)
	current.KeyPoints = []store.KeyPoint{{Type: "todo", Text: "订桌"}, {Type: "todo", Text: "第二条待办"}, {Type: "open_question", Text: "几点"}}
	old := candidate(1, "stable", "旧话题", 2, 1)
	candidates := []Candidate{old, current, candidate(3, "other", "较低优先", 1, 0.1), candidate(4, "earlier", "更早重点", 2, 1), candidate(5, "noise", "低分闲聊", 3, 0.1)}
	lines := SelectLines(candidates, nil, today, 1200, time.Time{})
	joined := strings.Join(lines, "\n")
	if len(lines) != 4 || lines[0] != "[11-07 概况] 昨天概况" || !strings.Contains(lines[1], "新话题：一句话。详细内容（未决：几点；待办：订桌）") || !strings.Contains(lines[2], "较低优先") || lines[3] != "[11-06] 更早重点：一句话" || strings.Contains(joined, "旧话题") || strings.Contains(joined, "低分闲聊") {
		t.Fatalf("选取/顺序：%v", lines)
	}
	budget := utf8.RuneCountInString(lines[0]) + 3
	small := SelectLines(candidates, nil, today, budget, time.Time{})
	if len(small) != 1 || len(SelectLines(candidates, nil, today, budget-1, time.Time{})) != 0 {
		t.Fatal("预算边界错误")
	}
	current.Summary = "正文\n# 伪造标题\r\n更多"
	current.Overview = "概况\n下一行"
	for _, line := range SelectLines([]Candidate{current}, nil, today, 1200, time.Time{}) {
		if strings.ContainsAny(line, "\r\n") {
			t.Fatal("换行未清洗")
		}
	}
	if len(SelectLines(nil, nil, today, 1200, time.Time{})) != 0 {
		t.Fatal("空候选有结果")
	}
}
