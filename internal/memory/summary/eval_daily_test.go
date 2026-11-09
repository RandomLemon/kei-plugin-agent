package summary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/llm"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"gorm.io/gorm"
)

const (
	evalLLMBaseURLEnv = "PERSONA_MEMORY_EVAL_LLM_BASE_URL"
	evalLLMAPIKeyEnv  = "PERSONA_MEMORY_EVAL_LLM_API_KEY"
	evalLLMSmallEnv   = "PERSONA_MEMORY_EVAL_LLM_MODEL_SMALL"
	evalLLMLargeEnv   = "PERSONA_MEMORY_EVAL_LLM_MODEL_LARGE"
	evalRunsEnv       = "PERSONA_MEMORY_EVAL_RUNS"
	evalTimezone      = "Asia/Shanghai"
	evalDayStart      = 4 * time.Hour
)

// evalLabels 是人工标注；key_points 只用于人工复核，不参与自动指标。
type evalLabels struct {
	Topics    []evalLabel      `json:"topics"`
	KeyPoints []evalLabelPoint `json:"key_points"`
}

type evalLabel struct {
	Title     string   `json:"title"`
	Keywords  []string `json:"keywords"`
	Continues string   `json:"continues"`
}

type evalLabelPoint struct {
	Text    string `json:"text"`
	Speaker string `json:"speaker"`
}

// evalCase 是一个会话日的 case。
type evalCase struct {
	Name     string
	Conv     string
	Date     time.Time
	Messages []evalMessage
	Labels   evalLabels
}

// TestEvalDaily 用真实摘要流水线回放 case，输出自动指标；只记录，不修改生产代码。
func TestEvalDaily(t *testing.T) {
	env := evalEnv(t, evalCasesDirEnv, "PERSONA_MEMORY_TEST_DSN", evalLLMBaseURLEnv, evalLLMAPIKeyEnv, evalLLMSmallEnv, evalLLMLargeEnv)
	runs := evalRuns(t)
	cases := readEvalCases(t, env[evalCasesDirEnv])
	location, err := time.LoadLocation(evalTimezone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", evalTimezone, err)
	}
	client := llm.New(llm.Config{BaseURL: env[evalLLMBaseURLEnv], APIKey: env[evalLLMAPIKeyEnv], Timeout: 120 * time.Second, JSONMode: true}, &http.Client{}, slog.Default())
	runMetrics := make([]evalMetrics, 0, runs)
	for run := 1; run <= runs; run++ {
		runMetrics = append(runMetrics, evaluateRun(t, run, cases, env, client, location))
	}
	logEvalAverages(t, runMetrics)
}

func evalRuns(t *testing.T) int {
	t.Helper()
	raw := os.Getenv(evalRunsEnv)
	if raw == "" {
		return 3
	}
	runs, err := strconv.Atoi(raw)
	if err != nil || runs < 1 {
		t.Fatalf("%s 必须 >= 1: %q", evalRunsEnv, raw)
	}
	return runs
}

func readEvalCases(t *testing.T, dir string) []evalCase {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 case 目录: %v", err)
	}
	var cases []evalCase
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) < 12 || name[len(name)-11] != '-' {
			t.Fatalf("case 名应为 <会话代号>-<YYYY-MM-DD>: %s", name)
		}
		date, err := time.Parse("2006-01-02", name[len(name)-10:])
		if err != nil {
			t.Fatalf("case %s 的日期: %v", name, err)
		}
		caseDir := filepath.Join(dir, name)
		var labels evalLabels
		data, err := os.ReadFile(filepath.Join(caseDir, "labels.json"))
		if err != nil {
			t.Fatalf("读取 %s/labels.json: %v", name, err)
		}
		if err = json.Unmarshal(data, &labels); err != nil {
			t.Fatalf("解析 %s/labels.json: %v", name, err)
		}
		cases = append(cases, evalCase{Name: name, Conv: name[:len(name)-11], Date: date, Messages: readEvalMessages(t, filepath.Join(caseDir, "messages.jsonl")), Labels: labels})
	}
	if len(cases) == 0 {
		t.Fatalf("case 目录为空: %s", dir)
	}
	slices.SortFunc(cases, func(a, b evalCase) int {
		if a.Conv != b.Conv {
			return strings.Compare(a.Conv, b.Conv)
		}
		return a.Date.Compare(b.Date)
	})
	return cases
}

func evaluateRun(t *testing.T, run int, cases []evalCase, env map[string]string, client *llm.Client, location *time.Location) evalMetrics {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	db := memtest.Open(t, memtest.NewDB(t))
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	seedEvalCases(t, ctx, db, cases)

	results := make([]evalCaseResult, 0, len(cases))
	for _, c := range cases {
		// 摘要日次日 05:00：Daily 的七日窗口包含该 case，且当天尚未摘要。
		nextDay := time.Date(c.Date.Year(), c.Date.Month(), c.Date.Day(), 5, 0, 0, 0, location).AddDate(0, 0, 1)
		pipeline := Pipeline{DB: db, Client: client, Small: env[evalLLMSmallEnv], Large: env[evalLLMLargeEnv], Location: location, DayStart: evalDayStart, Now: func() time.Time { return nextDay }, Log: slog.Default()}
		convKey := evalConvKey(c.Conv)
		result, err := pipeline.Daily(ctx, store.Job{Payload: store.JobPayload{ConvKey: convKey}})
		if err != nil {
			t.Fatalf("run=%d case=%s: %v", run, c.Name, err)
		}
		var summary store.Summary
		err = db.WithContext(ctx).Preload("Topics", func(db *gorm.DB) *gorm.DB { return db.Order("id") }).Where("conv_key = ? AND local_date = ? AND kind = 'daily'", convKey, c.Date).First(&summary).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatal(err)
		}
		results = append(results, evalCaseResult{Case: c, Topics: summary.Topics, Tokens: result.PromptTokens + result.CompletionTokens})
	}
	metrics := computeEvalMetrics(results)
	for _, result := range results {
		logEvalCase(t, run, result)
	}
	t.Logf("run=%d: cases=%d coverage=%s fragmentation=%s coarseness=%s continuity=%s tokens/case=%.1f",
		run, metrics.Cases, evalPercent(metrics.Covered, metrics.Labels), evalPercent(metrics.Fragmented, metrics.Labels),
		evalPercent(metrics.Coarse, metrics.Generated), evalPercent(metrics.ContinuityOK, metrics.ContinuityTotal), evalTokenAverage(metrics))
	return metrics
}

func seedEvalCases(t *testing.T, ctx context.Context, db *gorm.DB, cases []evalCase) {
	t.Helper()
	seen := map[string]bool{}
	for _, c := range cases {
		convKey := evalConvKey(c.Conv)
		if !seen[convKey] {
			conv := store.Conversation{ConvKey: convKey, Type: "group", Platform: "eval", ChannelID: c.Conv, Name: c.Conv}
			if err := db.WithContext(ctx).Create(&conv).Error; err != nil {
				t.Fatalf("建会话 %s: %v", convKey, err)
			}
			seen[convKey] = true
		}
		rows := make([]store.Message, 0, len(c.Messages))
		for i, message := range c.Messages {
			sentAt := message.SentAt
			if sentAt.IsZero() {
				sentAt = c.Date.Add(time.Duration(i) * time.Minute)
			}
			rows = append(rows, store.Message{
				ConvKey: convKey, DedupeKey: fmt.Sprintf("%s:%d", c.Name, i), MsgID: message.MsgID,
				SenderID: message.SenderID, SenderName: message.SenderName, SenderIsBot: message.SenderIsBot,
				IsSelf: message.IsSelf, Persona: message.Persona, Text: message.Text, ReplyTo: message.ReplyTo,
				Mentions: append([]string{}, message.Mentions...), SentAt: sentAt, LocalDate: c.Date, ReceivedAt: sentAt,
			})
		}
		if err := db.WithContext(ctx).Create(&rows).Error; err != nil {
			t.Fatalf("写入 case %s: %v", c.Name, err)
		}
		day := store.ConvDay{ConvKey: convKey, LocalDate: c.Date, MsgCount: len(rows)}
		if len(rows) > 0 {
			day.LastMsgID = rows[len(rows)-1].ID
		}
		if err := db.WithContext(ctx).Create(&day).Error; err != nil {
			t.Fatalf("写会话日 %s: %v", c.Name, err)
		}
	}
}

func evalConvKey(conv string) string { return "group:eval:" + conv }

type evalCaseResult struct {
	Case   evalCase
	Topics []store.Topic
	Tokens int64
}

type evalMetrics struct {
	Cases                         int
	Labels, Covered, Fragmented   int
	Generated, Coarse             int
	ContinuityTotal, ContinuityOK int
	Tokens                        int64
}

// computeEvalMetrics 按 case 顺序累计自动指标；延续以标注标题关联前一个 case。
func computeEvalMetrics(results []evalCaseResult) evalMetrics {
	var metrics evalMetrics
	previous := map[string]map[string][]string{}
	for _, result := range results {
		metrics.Cases++
		metrics.Tokens += result.Tokens
		metrics.Generated += len(result.Topics)
		labels := result.Case.Labels.Topics
		matched := make([][]int, len(labels))
		labelKeys := map[string][]string{}
		for i, label := range labels {
			metrics.Labels++
			for j := range result.Topics {
				if evalTopicMatches(label, result.Topics[j]) {
					matched[i] = append(matched[i], j)
				}
			}
			if len(matched[i]) > 0 {
				metrics.Covered++
				for _, j := range matched[i] {
					labelKeys[label.Title] = append(labelKeys[label.Title], result.Topics[j].TopicKey)
				}
			}
			if len(matched[i]) > 1 {
				metrics.Fragmented++
			}
			if label.Continues != "" {
				metrics.ContinuityTotal++
				// 过碎时一个标注会命中多个话题：与前一 case 的匹配集合有交集即视作延续。
				if evalKeyIntersects(previous[result.Case.Conv][label.Continues], labelKeys[label.Title]) {
					metrics.ContinuityOK++
				}
			}
		}
		for j := range result.Topics {
			count := 0
			for i := range labels {
				if slices.Contains(matched[i], j) {
					count++
				}
			}
			if count > 1 {
				metrics.Coarse++
			}
		}
		previous[result.Case.Conv] = labelKeys
	}
	return metrics
}

func evalTopicMatches(label evalLabel, topic store.Topic) bool {
	haystack := strings.ToLower(topic.Title + "\n" + topic.Summary)
	for _, keyword := range label.Keywords {
		if keyword != "" && strings.Contains(haystack, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

func evalKeyIntersects(left, right []string) bool {
	for _, a := range left {
		if slices.Contains(right, a) {
			return true
		}
	}
	return false
}

func logEvalCase(t *testing.T, run int, result evalCaseResult) {
	t.Helper()
	t.Logf("run=%d case=%s labels=%d generated=%d tokens=%d", run, result.Case.Name, len(result.Case.Labels.Topics), len(result.Topics), result.Tokens)
	for _, topic := range result.Topics {
		t.Logf("  生成 [%s] %s：%s", topic.TopicKey, topic.Title, topic.Summary)
		for _, point := range topic.KeyPoints {
			t.Logf("    要点 type=%s speaker=%s owner=%s text=%q msg_ids=%v", point.Type, point.Speaker, point.Owner, point.Text, point.MsgIDs)
		}
	}
	for _, label := range result.Case.Labels.Topics {
		var hits []string
		for _, topic := range result.Topics {
			if evalTopicMatches(label, topic) {
				hits = append(hits, topic.Title)
			}
		}
		t.Logf("  标注 %q keywords=%v continues=%q 命中 %v", label.Title, label.Keywords, label.Continues, hits)
	}
	// 归属正确率与编造没有自动指标，报告必须保留标注要点供人工逐条核对。
	for _, point := range result.Case.Labels.KeyPoints {
		t.Logf("  标注要点 text=%q speaker=%q", point.Text, point.Speaker)
	}
}

// logEvalAverages 把各次运行的分子分母分别累加后求比例：以标注数为分母的指标与逐次平均一致，过粗率按生成话题数加权。
func logEvalAverages(t *testing.T, runs []evalMetrics) {
	t.Helper()
	var total evalMetrics
	for _, metrics := range runs {
		total.Cases += metrics.Cases
		total.Labels += metrics.Labels
		total.Covered += metrics.Covered
		total.Fragmented += metrics.Fragmented
		total.Generated += metrics.Generated
		total.Coarse += metrics.Coarse
		total.ContinuityTotal += metrics.ContinuityTotal
		total.ContinuityOK += metrics.ContinuityOK
		total.Tokens += metrics.Tokens
	}
	t.Logf("评测平均：runs=%d coverage=%s fragmentation=%s coarseness=%s continuity=%s tokens/case=%.1f",
		len(runs), evalPercent(total.Covered, total.Labels), evalPercent(total.Fragmented, total.Labels),
		evalPercent(total.Coarse, total.Generated), evalPercent(total.ContinuityOK, total.ContinuityTotal), evalTokenAverage(total))
}

// evalPercent 在分母为 0（如没有带 continues 的标注）时输出 N/A，避免与真实的 0% 混淆。
func evalPercent(numerator, denominator int) string {
	if denominator == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.1f%%", float64(numerator)*100/float64(denominator))
}

func evalTokenAverage(metrics evalMetrics) float64 {
	if metrics.Cases == 0 {
		return 0
	}
	return float64(metrics.Tokens) / float64(metrics.Cases)
}

func TestComputeEvalMetrics(t *testing.T) {
	day1 := evalCaseResult{
		Case: evalCase{Conv: "c", Labels: evalLabels{Topics: []evalLabel{
			{Title: "聚餐", Keywords: []string{"聚餐"}},
			{Title: "考试", Keywords: []string{"考试"}},
		}}},
		Topics: []store.Topic{
			{TopicKey: "k1", Title: "聚餐与考试", Summary: "先聊聚餐再聊考试"},
			{TopicKey: "k2", Title: "聚餐地点"},
		},
		Tokens: 10,
	}
	day2 := evalCaseResult{
		Case: evalCase{Conv: "c", Labels: evalLabels{Topics: []evalLabel{
			{Title: "聚餐续", Keywords: []string{"聚餐"}, Continues: "聚餐"},
			{Title: "考试续", Keywords: []string{"考试"}, Continues: "考试"},
			{Title: "篮球", Keywords: []string{"篮球"}},
		}}},
		Topics: []store.Topic{
			{TopicKey: "k2", Title: "聚餐结账"},
			{TopicKey: "k3", Title: "考试复习"},
		},
		Tokens: 30,
	}
	got := computeEvalMetrics([]evalCaseResult{day1, day2})
	want := evalMetrics{
		Cases: 2, Labels: 5, Covered: 4, Fragmented: 1,
		Generated: 4, Coarse: 1,
		ContinuityTotal: 2, ContinuityOK: 1,
		Tokens: 40,
	}
	if got != want {
		t.Fatalf("指标 = %+v，期望 %+v", got, want)
	}
	if p := evalPercent(0, 0); p != "N/A" {
		t.Fatalf("分母为 0 = %q，期望 N/A", p)
	}
	if p := evalPercent(1, 4); p != "25.0%" {
		t.Fatalf("1/4 = %q", p)
	}
}
