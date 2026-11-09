package memory

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

// TestRuntimeHostSummaryTool 用真实宿主和已配置聊天模型验收显式查询，只发送固定合成摘要。
func TestRuntimeHostSummaryTool(t *testing.T) {
	binary := os.Getenv("PERSONA_MEMORY_SUMMARY_HOST_BINARY")
	if binary == "" {
		t.Skip("未指定 S6 运行验收宿主")
	}
	dsn := memtest.NewDB(t)
	db := memtest.Open(t, dsn)
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	today := store.LocalDate(time.Now(), time.UTC, 4*time.Hour)
	// 上周三，在自动注入的三日窗口之外，必须经工具查询。
	weekday := (int(today.Weekday()) + 6) % 7
	date := today.AddDate(0, 0, -weekday-7+2).Format("2006-01-02")
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		t.Fatal(err)
	}
	row := store.Summary{ConvKey: "group:mock:g1", LocalDate: day, Kind: "daily", Version: 1, Topics: []store.Topic{{ConvKey: "group:mock:g1", LocalDate: day, TopicKey: "runtime:summary", Title: "聚餐安排", Summary: "上周三讨论聚餐，决定周六18点在松果餐厅聚餐，人均预算100元。", Importance: 0.9}}}
	if err = db.WithContext(t.Context()).Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	host := startRuntimeHostFor(t, binary, dsn, 2*time.Minute,
		"KEI_LOG_LEVEL=debug",
		"KEI_PLUGINS_PERSONA_DEBUG_PROMPTS=true",
		"KEI_PLUGINS_PERSONA_GROUP_LIST=[g1]",
		"KEI_PLUGINS_PERSONA_TRIGGER_KEYWORDS=[bot]",
		"KEI_PLUGINS_PERSONA_RANDOM_TIMEZONE=UTC",
		"KEI_PLUGINS_PERSONA_MEMORY_DAILY_AT=04:00",
		"KEI_PLUGINS_PERSONA_MEMORY_TOOLS_ENABLED=true",
		"KEI_PLUGINS_PERSONA_MCP_URL=",
		"KEI_PLUGINS_PERSONA_BATCH_WINDOW=20ms",
		"KEI_PLUGINS_PERSONA_BATCH_MAX_WINDOW=30ms",
		"KEI_PLUGINS_PERSONA_MENTION_MIN_INTERVAL=0s",
		"KEI_PLUGINS_PERSONA_REPLY_DEDUPE=false",
		"KEI_PLUGINS_PERSONA_LLM_MAX_TOKENS=2048",
		`KEI_PLUGINS_PERSONA_PERSONAS={"default":{"prompt":"用户询问过去聊天内容时，先用 memory__summary 按日期查询，再据工具结果简短回答。"}}`,
	)
	host.waitReady(t, 15*time.Second)
	host.inject(t, "u1", "bot 上周三（"+date+"）聊了什么？请按该日期查聊天摘要。", "g1")
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(host.sentBody(), "松果餐厅") || !strings.Contains(host.logs.String(), "tool=memory__summary") || !strings.Contains(host.logs.String(), "status=ok") {
		if time.Now().After(deadline) {
			t.Fatal("未观察到成功的 memory__summary 调用及包含合成摘要的回复")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Log("真实宿主与聊天模型：询问上周三，成功调用 memory__summary，回复包含工具返回的松果餐厅安排；仅使用固定合成数据")
}
