package summary

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

// Candidate 是读取日摘要时的一条话题及所属日概况。
type Candidate struct {
	store.Topic `gorm:"embedded"`
	Overview    string
	Kind        string
}

func bigrams(text string) map[string]struct{} {
	text = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, StripPlaceholders(text))
	runes := []rune(text)
	out := map[string]struct{}{}
	if len(runes) == 1 {
		out[text] = struct{}{}
	}
	for n := 1; n < len(runes); n++ {
		out[string(runes[n-1:n+1])] = struct{}{}
	}
	return out
}

// KeywordRelevance 按规范化正文的 bigram 重叠计算关键词相关度。
func KeywordRelevance(query, text string) float64 {
	q, t := bigrams(query), bigrams(text)
	if len(q) == 0 {
		return 0
	}
	matches := 0
	for unit := range q {
		if _, ok := t[unit]; ok {
			matches++
		}
	}
	return float64(matches) / float64(len(q))
}

func topicScore(topic store.Topic, queries []string, today time.Time) float64 {
	relevance := 0.0
	for _, q := range queries {
		relevance = max(relevance, KeywordRelevance(q, topic.Title+topic.Summary))
	}
	age := today.Sub(topic.LocalDate).Hours() / 24
	return topic.Importance * math.Pow(0.5, age/tSummary) * (1 + scoreAlpha*relevance)
}

// OpenItems 提取第一条未决问题与待办，供上下文和显式查询共用。
func OpenItems(points []store.KeyPoint) []string {
	var question, todo string
	for _, p := range points {
		if p.Type == "open_question" && question == "" {
			question = "未决：" + p.Text
		}
		if p.Type == "todo" && todo == "" {
			todo = "待办：" + p.Text
		}
	}
	var items []string
	if question != "" {
		items = append(items, question)
	}
	if todo != "" {
		items = append(items, todo)
	}
	return items
}

func openItems(points []store.KeyPoint) string {
	items := OpenItems(points)
	if len(items) == 0 {
		return ""
	}
	return "（" + strings.Join(items, "；") + "）"
}

// SelectLines 去重、打分并按正文预算选取记忆行；预算含每行的列表前缀与换行。
func SelectLines(candidates []Candidate, queries []string, today time.Time, budget int, historySince time.Time) []string {
	if len(candidates) == 0 || budget <= 0 {
		return nil
	}
	// 最新日期由全部候选决定，而非去重后剩余话题的排序。
	var latest time.Time
	var latestOverview string
	newest := map[string]Candidate{}
	for _, candidate := range candidates {
		if candidate.Kind == "rolling" && !historySince.IsZero() && !candidate.FirstAt.Before(historySince) {
			continue
		}
		if candidate.Kind != "rolling" && candidate.LocalDate.After(latest) {
			latest = candidate.LocalDate
			latestOverview = candidate.Overview
		}
		old, ok := newest[candidate.TopicKey]
		if !ok || candidate.LocalDate.After(old.LocalDate) || candidate.LocalDate.Equal(old.LocalDate) && candidate.ID < old.ID {
			newest[candidate.TopicKey] = candidate
		}
	}
	type ranked struct {
		candidate Candidate
		score     float64
	}
	rankedTopics := make([]ranked, 0, len(newest))
	for _, candidate := range newest {
		rankedTopics = append(rankedTopics, ranked{candidate, topicScore(candidate.Topic, queries, today)})
	}
	slices.SortFunc(rankedTopics, func(a, b ranked) int {
		if a.score > b.score {
			return -1
		}
		if a.score < b.score {
			return 1
		}
		if c := b.candidate.LocalDate.Compare(a.candidate.LocalDate); c != 0 {
			return c
		}
		if a.candidate.ID < b.candidate.ID {
			return -1
		}
		if a.candidate.ID > b.candidate.ID {
			return 1
		}
		return strings.Compare(a.candidate.TopicKey, b.candidate.TopicKey)
	})
	var pending []string
	for _, item := range rankedTopics {
		c := item.candidate
		if c.Kind == "rolling" {
			label := c.LocalDate.Format("01-02")
			if c.LocalDate.Equal(today) {
				label = "今天"
			}
			pending = append(pending, fmt.Sprintf("[%s] %s：%s%s", label, c.Title, c.Summary, openItems(c.KeyPoints)))
		}
	}
	if !latest.IsZero() {
		pending = append(pending, fmt.Sprintf("[%s 概况] %s", latest.Format("01-02"), latestOverview))
	}
	for _, item := range rankedTopics {
		c := item.candidate
		if c.Kind != "rolling" && c.LocalDate.Equal(latest) {
			pending = append(pending, fmt.Sprintf("[%s] %s：%s%s", c.LocalDate.Format("01-02"), c.Title, c.Summary, openItems(c.KeyPoints)))
		}
	}
	for _, item := range rankedTopics {
		c := item.candidate
		if c.Kind != "rolling" && !c.LocalDate.Equal(latest) && item.score >= scoreTheta {
			pending = append(pending, fmt.Sprintf("[%s] %s：%s", c.LocalDate.Format("01-02"), c.Title, ShortSummary(c.Summary)))
		}
	}
	var lines []string
	used := 0
	for _, line := range pending {
		line = strings.Join(strings.Fields(line), " ")
		size := utf8.RuneCountInString(line) + 3 // "- " 与一个换行。
		if used+size > budget {
			break
		}
		lines = append(lines, line)
		used += size
	}
	return lines
}
