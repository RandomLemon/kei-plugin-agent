package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/llm"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestRuntimeDailySummary 回放已采集的真实会话日，在隔离 schema 上使用真实 LLM。
// 运行变量只属于测试设施；源库只读，原始消息不写入日志或仓库。
func TestRuntimeDailySummary(t *testing.T) {
	endpoint := os.Getenv("PERSONA_MEMORY_RUNTIME_LLM_BASE_URL")
	if endpoint == "" {
		t.Skip("未配置真实 LLM 运行验收")
	}
	source, err := gorm.Open(postgres.Open(os.Getenv("PERSONA_MEMORY_TEST_DSN")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("连接源库失败")
	}
	sourceSQL, err := source.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceSQL.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	var candidates []store.ConvDay
	if err = source.WithContext(ctx).Where("msg_count >= 3").Order("local_date DESC, msg_count DESC").Limit(30).Find(&candidates).Error; err != nil {
		t.Fatal(err)
	}
	var raw []store.Message
	var day store.ConvDay
	for _, candidate := range candidates {
		var messages []store.Message
		if err = source.WithContext(ctx).Where("conv_key=? AND local_date=? AND recalled=false", candidate.ConvKey, candidate.LocalDate).Order("sent_at,id").Limit(400).Find(&messages).Error; err != nil {
			t.Fatal(err)
		}
		chunks, _ := Slice(messages)
		useful := false
		for _, chunk := range chunks {
			if len(Prefilter(chunk)) > 0 {
				useful = true
				break
			}
		}
		if useful {
			raw = messages
			day = candidate
			break
		}
	}
	if len(raw) == 0 {
		t.Fatal("源库没有满足真人信息阈值的会话日，无法完成真实数据运行验收")
	}
	db, err := gorm.Open(postgres.Open(memtest.NewDB(t)), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var conv store.Conversation
	if err = source.WithContext(ctx).Where("conv_key=?", day.ConvKey).First(&conv).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.WithContext(ctx).Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	for n := range raw {
		raw[n].ID = 0
	}
	if err = db.WithContext(ctx).Create(&raw).Error; err != nil {
		t.Fatal(err)
	}
	day.LastMsgID = raw[len(raw)-1].ID
	day.SummarizedUptoID = nil
	if err = db.WithContext(ctx).Create(&day).Error; err != nil {
		t.Fatal(err)
	}
	log := slog.Default()
	headers := map[string]string{}
	if s := os.Getenv("PERSONA_MEMORY_RUNTIME_LLM_HEADERS"); s != "" {
		if err = json.Unmarshal([]byte(s), &headers); err != nil {
			t.Fatal("运行验收请求头不是 JSON 对象")
		}
	}
	client := llm.New(llm.Config{BaseURL: endpoint, APIKey: os.Getenv("PERSONA_MEMORY_RUNTIME_LLM_API_KEY"), Headers: headers, Timeout: 120 * time.Second, JSONMode: true}, &http.Client{}, log)
	pipeline := Pipeline{DB: db, Client: client, Small: os.Getenv("PERSONA_MEMORY_RUNTIME_LLM_MODEL"), Large: os.Getenv("PERSONA_MEMORY_RUNTIME_LLM_MODEL"), Location: time.UTC, Now: func() time.Time { return day.LocalDate.AddDate(0, 0, 1).Add(12 * time.Hour) }, Log: log}
	result, err := pipeline.Daily(ctx, store.Job{Payload: store.JobPayload{ConvKey: day.ConvKey}})
	if err != nil {
		t.Fatal(err)
	}
	var summary store.Summary
	if err = db.WithContext(ctx).Preload("Topics").First(&summary).Error; err != nil {
		t.Fatal(err)
	}
	// 只打印脱敏后的生成结果，供与本次来源匹配结果一起人工抽读。
	t.Logf("真实会话日回放：messages=%d topics=%d prompt_tokens=%d completion_tokens=%d", len(raw), len(summary.Topics), result.PromptTokens, result.CompletionTokens)
	for _, topic := range summary.Topics {
		t.Logf("%s：%s", topic.Title, topic.Summary)
		for _, point := range topic.KeyPoints {
			valid := false
			for _, m := range raw {
				if m.SenderID == point.Speaker {
					for _, id := range point.MsgIDs {
						expected := m.MsgID
						if expected == "" {
							expected = fmt.Sprintf("mem:%d", m.ID)
						}
						if id == expected {
							valid = true
						}
					}
				}
			}
			if !valid {
				t.Fatal("要点来源归属不匹配")
			}
		}
	}
}
