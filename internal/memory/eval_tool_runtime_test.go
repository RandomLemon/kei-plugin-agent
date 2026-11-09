package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"gorm.io/gorm"
)

const (
	evalToolTimezone = "Asia/Shanghai"
	evalToolDayStart = 4 * time.Hour
)

// TestEvalSummaryTool 只观察 memory__summary 的调用时机，记录指标而不设门槛。
func TestEvalSummaryTool(t *testing.T) {
	base := os.Getenv("PERSONA_MEMORY_TEST_DSN")
	binary := os.Getenv("PERSONA_MEMORY_EVAL_HOST_BINARY")
	if base == "" || binary == "" {
		t.Skip("未配置 S7 工具调用评测环境")
	}
	dsn := memtest.NewDB(t)
	location, err := time.LoadLocation(evalToolTimezone)
	if err != nil {
		t.Fatal(err)
	}
	// 00:00–04:00 摘要日仍是前一天，而模型从 {{now}} 看到日历日，相对日期期望会差一天；
	// 23 点后开始则可能在运行中跨过零点。只在两者一致且留有余量时运行。
	if hour := time.Now().In(location).Hour(); hour < 4 || hour >= 23 {
		t.Skipf("请在 %s 04:00–23:00 之间运行（当前 %d 点）", evalToolTimezone, hour)
	}
	today := store.LocalDate(time.Now(), location, evalToolDayStart)
	queries := readEvalToolQueries(t, filepath.Join("testdata", "eval", "tool_queries.json"), today)
	db := memtest.Open(t, dsn)
	if err = store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	groups := make([]string, len(queries))
	for i := range groups {
		groups[i] = fmt.Sprintf("g%d", i+1)
	}
	seedEvalToolSummaries(t, db, today, groups)

	host := startRuntimeHostFor(t, binary, dsn, time.Duration(len(queries))*90*time.Second+2*time.Minute,
		"KEI_LOG_LEVEL=debug",
		"KEI_PLUGINS_PERSONA_DEBUG_PROMPTS=true",
		"KEI_PLUGINS_PERSONA_GROUP_LIST=["+strings.Join(groups, ",")+"]",
		"KEI_PLUGINS_PERSONA_TRIGGER_KEYWORDS=[bot]",
		"KEI_PLUGINS_PERSONA_MENTION_MIN_INTERVAL=0s",
		"KEI_PLUGINS_PERSONA_REPLY_DEDUPE=false",
		"KEI_PLUGINS_PERSONA_RANDOM_TIMEZONE="+evalToolTimezone,
		"KEI_PLUGINS_PERSONA_MEMORY_DAILY_AT=04:00",
		"KEI_PLUGINS_PERSONA_MEMORY_TOOLS_ENABLED=true",
	)
	host.waitReady(t, 15*time.Second)

	var metrics evalToolMetrics
	for i, query := range queries {
		channel := groups[i]
		offset := len(host.logs.String())
		before := len(host.sentFor(t, channel))
		host.inject(t, "u1", "bot "+renderEvalDate(t, query.Message, today), channel)
		waitEvalReply(t, host, channel, before, offset, 90*time.Second)
		calls := parseEvalToolCalls(host.logsSince(offset))
		t.Logf("问题：%s 调用：%+v", query.Message, calls)
		metrics.record(query, calls)
	}
	percent := func(value, total int) string {
		if total == 0 {
			return "N/A"
		}
		return fmt.Sprintf("%.1f%%", float64(value)*100/float64(total))
	}
	t.Logf("B 指标：queries=%d 窗口外召回=%s 日期正确=%s query正确=%s 误调用=%s 冗余调用=%s",
		metrics.Queries,
		percent(metrics.OutsideHit, metrics.OutsideExpected),
		percent(metrics.DateOK, metrics.DateExpected),
		percent(metrics.QueryOK, metrics.QueryExpected),
		percent(metrics.FalseCalls, metrics.NoCallExpected),
		percent(metrics.Redundant, metrics.AnyExpected),
	)
}

type evalToolQuery struct {
	Message   string `json:"message"`
	Expected  string `json:"expected"`
	Query     string `json:"query"`
	DateFromN int    `json:"date_from_n"` // 相对今天的天数，给出即为窗口外按日期问的问题
	DateToN   int    `json:"date_to_n"`
	DateFrom  string `json:"-"` // 由 DateFromN 按当天算出
	DateTo    string `json:"-"`
}

type evalToolArgs struct {
	DateFrom string `json:"date_from"`
	DateTo   string `json:"date_to"`
	Query    string `json:"query"`
}

type evalToolCall struct {
	Args evalToolArgs `json:"args"`
}

type evalToolMetrics struct {
	Queries         int
	OutsideExpected int
	OutsideHit      int
	DateExpected    int
	DateOK          int
	QueryExpected   int
	QueryOK         int
	NoCallExpected  int
	FalseCalls      int
	AnyExpected     int
	Redundant       int
}

var (
	evalToolLineRE        = regexp.MustCompile(`(?i)"persona 工具调用".*tool=memory__summary`)
	evalToolArgsRE        = regexp.MustCompile(`\bargs=(?:"((?:\\.|[^"\\])*)"|(\S+))`)
	evalDatePlaceholderRE = regexp.MustCompile(`\{date:(-?\d+)\}`)
)

func renderEvalDate(t *testing.T, message string, today time.Time) string {
	t.Helper()
	return evalDatePlaceholderRE.ReplaceAllStringFunc(message, func(placeholder string) string {
		match := evalDatePlaceholderRE.FindStringSubmatch(placeholder)
		days, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("日期占位符 %q: %v", placeholder, err)
		}
		return today.AddDate(0, 0, days).Format("2006-01-02")
	})
}

func readEvalToolQueries(t *testing.T, path string, today time.Time) []evalToolQuery {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var queries []evalToolQuery
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&queries); err != nil {
		t.Fatal(err)
	}
	for i, query := range queries {
		field := fmt.Sprintf("问题[%d]", i)
		if query.Message == "" {
			t.Fatalf("%s: message 为空", field)
		}
		switch query.Expected {
		case "call", "no_call", "any":
		default:
			t.Fatalf("%s: expected=%q 非法", field, query.Expected)
		}
		if (query.DateFromN == 0) != (query.DateToN == 0) {
			t.Fatalf("%s: date_from_n/date_to_n 必须成对给出", field)
		}
		if query.DateFromN != 0 {
			query.DateFrom = today.AddDate(0, 0, query.DateFromN).Format("2006-01-02")
			query.DateTo = today.AddDate(0, 0, query.DateToN).Format("2006-01-02")
		}
		if query.Query != "" && query.Expected != "call" {
			t.Fatalf("%s: 带 query 的问题必须期望调用", field)
		}
		queries[i] = query
	}
	return queries
}

// evalToolTopics 是合成摘要，每天一个不同话题，便于核对日期：-1 天在 WindowDays 窗口内会被自动注入；
// -4～-6 天在窗口外、但在不带日期调用的默认范围（近 7 天）内，供按话题问；-7 及更早供按日期问。
var evalToolTopics = []struct {
	days                int
	key, title, summary string
}{
	{-1, "爬山", "周末爬山", "大家约周六去紫金山爬山，早上 8 点在地铁站集合。"},
	{-4, "聚餐", "聚餐安排", "讨论聚餐，决定在松果餐厅聚餐，人均预算100元。"},
	{-5, "篮球", "篮球赛", "班级篮球赛定在下周四下午 4 点，在东区球场。"},
	{-6, "招新", "社团招新", "摄影社团招新报名月底截止，由小林负责收表。"},
	{-7, "读书会", "读书会", "读书会这期读《百年孤独》，周日晚上线上讨论。"},
	{-10, "考试", "考试安排", "期中考试安排在第 10 周周三上午，闭卷。"},
	{-20, "志愿者", "运动会志愿者", "校运动会志愿者名单已定，周六早上 7 点在体育馆签到。"},
	{-21, "宿舍", "搬宿舍", "下学期调整宿舍，月底前搬到 3 号楼。"},
}

func seedEvalToolSummaries(t *testing.T, db *gorm.DB, today time.Time, groups []string) {
	t.Helper()
	for _, topic := range evalToolTopics {
		date := today.AddDate(0, 0, topic.days)
		for _, group := range groups {
			convKey := "group:mock:" + group
			row := store.Summary{
				ConvKey:   convKey,
				LocalDate: date,
				Kind:      "daily",
				Version:   1,
				Topics: []store.Topic{{
					ConvKey:    convKey,
					LocalDate:  date,
					TopicKey:   topic.key,
					Title:      topic.title,
					Summary:    topic.summary,
					Importance: 0.9,
				}},
			}
			if err := db.WithContext(t.Context()).Create(&row).Error; err != nil {
				t.Fatalf("%s %s: %v", convKey, date.Format("2006-01-02"), err)
			}
		}
	}
}

// waitEvalReply 等到该频道发出回复，或模型返回空回复被跳过（不该调用工具的问题可能如此）。
func waitEvalReply(t *testing.T, host *runtimeHost, channel string, before, offset int, timeout time.Duration) {
	t.Helper()
	skipped := "reason=empty_reply channel=mock:mock-main:" + channel + " "
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(host.sentFor(t, channel)) > before || strings.Contains(host.logsSince(offset), skipped) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("问题未在 %s 内收到回复：\n%s", timeout, host.logs.String())
}

func parseEvalToolCalls(logs string) []evalToolCall {
	var calls []evalToolCall
	for _, line := range strings.Split(logs, "\n") {
		if !evalToolLineRE.MatchString(line) {
			continue
		}
		var args evalToolArgs
		if match := evalToolArgsRE.FindStringSubmatch(line); match != nil {
			raw := match[2]
			if raw == "" {
				raw = match[1]
				if unquoted, err := strconv.Unquote(`"` + raw + `"`); err == nil {
					raw = unquoted
				} else {
					raw = strings.ReplaceAll(raw, `\"`, `"`)
				}
			}
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				// 截断或非法参数仍计为一次调用，参数按全部不匹配处理。
				args = evalToolArgs{}
			}
		}
		calls = append(calls, evalToolCall{Args: args})
	}
	return calls
}

func TestParseEvalToolCalls(t *testing.T) {
	logs := strings.Join([]string{
		`time=2026-11-09 level=DEBUG msg="persona 工具调用" channel=g1 tool=memory__summary status=ok args="{\"date_from\":\"2026-10-29\",\"date_to\":\"2026-10-29\",\"query\":\"聚餐\"}"`,
		`time=2026-11-09 level=DEBUG msg="persona 工具调用" channel=g1 tool=memory__summary status=ok args={}`,
		`time=2026-11-09 level=DEBUG msg="persona 工具调用" channel=g1 tool=memory__summary status=ok args=""`,
		`time=2026-11-09 level=DEBUG msg="persona 工具调用" channel=g1 tool=memory__summary status=ok args="{\"date_from\":\"2026-10-`,
		`time=2026-11-09 level=DEBUG msg="persona 工具调用" channel=g1 tool=memory__list status=ok args="{\"query\":\"无关\"}"`,
	}, "\n")
	calls := parseEvalToolCalls(logs)
	if len(calls) != 4 {
		t.Fatalf("调用数 = %d，期望 4：%+v", len(calls), calls)
	}
	if calls[0].Args.DateFrom != "2026-10-29" || calls[0].Args.Query != "聚餐" {
		t.Fatalf("带引号参数解析错误：%+v", calls[0].Args)
	}
	for i := 1; i < len(calls); i++ {
		if calls[i].Args != (evalToolArgs{}) {
			t.Fatalf("调用[%d] 参数应为空：%+v", i, calls[i].Args)
		}
	}
}

func (m *evalToolMetrics) record(query evalToolQuery, calls []evalToolCall) {
	m.Queries++
	queryOK := false
	outsideOK := false
	dateExpected, dateOK := 0, 0
	for _, call := range calls {
		if query.DateFrom != "" {
			ok := 0
			for _, match := range []bool{call.Args.DateFrom == query.DateFrom, call.Args.DateTo == query.DateTo} {
				dateExpected++
				if match {
					ok++
				}
			}
			dateOK += ok
			if ok == 2 {
				outsideOK = true
			}
		}
		if query.Query != "" && strings.Contains(strings.ToLower(call.Args.Query), strings.ToLower(query.Query)) {
			queryOK = true
		}
	}
	m.DateExpected += dateExpected
	m.DateOK += dateOK
	// 窗口内内容已注入上下文，any 问题仍调用即记为冗余。
	if query.Expected == "any" {
		m.AnyExpected++
		if len(calls) > 0 {
			m.Redundant++
		}
	}
	if query.DateFrom != "" {
		m.OutsideExpected++
		if outsideOK {
			m.OutsideHit++
		}
	}
	if query.Query != "" {
		m.QueryExpected++
		if queryOK {
			m.QueryOK++
		}
	}
	if query.Expected == "no_call" {
		m.NoCallExpected++
		if len(calls) > 0 {
			m.FalseCalls++
		}
	}
}

func TestEvalToolMetricsRecord(t *testing.T) {
	var m evalToolMetrics
	outside := evalToolQuery{Expected: "call", DateFrom: "2026-10-30", DateTo: "2026-10-30"}
	m.record(outside, []evalToolCall{
		{Args: evalToolArgs{DateFrom: "2026-10-29", DateTo: "2026-10-30"}},
		{Args: evalToolArgs{DateFrom: "2026-10-30", DateTo: "2026-10-30"}},
	})
	m.record(outside, nil)
	m.record(evalToolQuery{Expected: "call", Query: "聚餐"}, []evalToolCall{{Args: evalToolArgs{Query: "聚餐 餐厅"}}})
	m.record(evalToolQuery{Expected: "any"}, []evalToolCall{{}})
	m.record(evalToolQuery{Expected: "any"}, nil)
	m.record(evalToolQuery{Expected: "no_call"}, []evalToolCall{{}})
	want := evalToolMetrics{
		Queries:         6,
		OutsideExpected: 2, OutsideHit: 1,
		DateExpected: 4, DateOK: 3,
		QueryExpected: 1, QueryOK: 1,
		NoCallExpected: 1, FalseCalls: 1,
		AnyExpected: 2, Redundant: 1,
	}
	if m != want {
		t.Fatalf("指标 = %+v，期望 %+v", m, want)
	}
}
