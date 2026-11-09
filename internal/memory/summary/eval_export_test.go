package summary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	evalSourceDSNEnv = "PERSONA_MEMORY_EVAL_SOURCE_DSN"
	evalConvEnv      = "PERSONA_MEMORY_EVAL_CONV"
	evalDateEnv      = "PERSONA_MEMORY_EVAL_DATE"
	evalCodeEnv      = "PERSONA_MEMORY_EVAL_CODE"
	evalIDMapEnv     = "PERSONA_MEMORY_EVAL_ID_MAP"
	evalCasesDirEnv  = "PERSONA_MEMORY_EVAL_CASES_DIR"
)

// assertEvalPrivatePath 拒绝把含真实数据的文件写进仓库的未忽略路径。
func assertEvalPrivatePath(t *testing.T, key, path string) {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	repo, err := filepath.Abs("../../..") // 包源码目录 internal/memory/summary
	if err != nil {
		t.Fatalf("仓库根目录: %v", err)
	}
	rel, err := filepath.Rel(repo, abs)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return // 仓库外不受限
	}
	if err := exec.Command("git", "-C", repo, "check-ignore", "-q", abs).Run(); err != nil {
		t.Fatalf("%s 在仓库内且未被 gitignore 忽略（或无法校验）: %s: %v", key, abs, err)
	}
}

// validateEvalCode 要求会话代号非空，且不把路径分隔符带进 Join。
func validateEvalCode(t *testing.T, code string) {
	t.Helper()
	if code == "." || code == ".." || strings.ContainsAny(code, `/\`) {
		t.Fatalf("%s 的会话代号不合法: %s", evalCodeEnv, code)
	}
}

// TestEvalExport 从只读源库导出一个会话日，按会话映射匿名化后写入 case 目录。
func TestEvalExport(t *testing.T) {
	env := evalEnv(t, evalSourceDSNEnv, evalConvEnv, evalDateEnv, evalCodeEnv, evalIDMapEnv, evalCasesDirEnv)
	assertEvalPrivatePath(t, evalCasesDirEnv, env[evalCasesDirEnv])
	assertEvalPrivatePath(t, evalIDMapEnv, env[evalIDMapEnv])
	date, err := time.Parse("2006-01-02", env[evalDateEnv])
	if err != nil {
		t.Fatalf("%s 应为 YYYY-MM-DD: %v", evalDateEnv, err)
	}
	validateEvalCode(t, env[evalCodeEnv])
	// case 名为 <会话代号>-<YYYY-MM-DD>，日期取自 DATE，只填一次。
	messagesPath := filepath.Join(env[evalCasesDirEnv], env[evalCodeEnv]+"-"+date.Format("2006-01-02"), "messages.jsonl")
	assertEvalPrivatePath(t, evalCasesDirEnv, messagesPath)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	db, err := gorm.Open(postgres.Open(env[evalSourceDSNEnv]), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("连接源库失败")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()

	conv := env[evalConvEnv]
	var rows []store.Message
	if err = db.WithContext(ctx).Where("conv_key = ? AND local_date = ? AND recalled = false", conv, date).Order("sent_at, id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatalf("源库没有 %s 在 %s 的消息", conv, date.Format("2006-01-02"))
	}

	mapping := loadEvalIDMap(t, env[evalIDMapEnv])
	// 先遍历整个 case 预分配编号，使早先消息正文里出现的 ID 也能被替换。
	for _, row := range rows {
		mapping.code(conv, row.SenderID)
		for _, mention := range row.Mentions {
			mapping.code(conv, mention)
		}
	}
	messages := make([]evalMessage, 0, len(rows))
	for _, row := range rows {
		sender := mapping.code(conv, row.SenderID)
		mentions := make([]string, 0, len(row.Mentions))
		for _, mention := range row.Mentions {
			mentions = append(mentions, mapping.code(conv, mention))
		}
		// 文档要求正文先 Redact；只替换映射表中已知的 ID（来自 sender / mentions）。
		text := mapping.replace(conv, Redact(row.Text))
		messages = append(messages, evalMessage{
			MsgID:       row.MsgID,
			SenderID:    sender,
			SenderName:  sender,
			SenderIsBot: row.SenderIsBot,
			IsSelf:      row.IsSelf,
			Persona:     row.Persona,
			Text:        text,
			ReplyTo:     row.ReplyTo,
			Mentions:    mentions,
			SentAt:      row.SentAt.UTC(),
		})
	}

	writeEvalJSONL(t, messagesPath, messages)
	saveEvalIDMap(t, env[evalIDMapEnv], mapping)
	t.Logf("导出 %s/%s: messages=%d ids=%d", conv, date.Format("2006-01-02"), len(messages), len(mapping[conv]))
}

// evalIDMap 按会话记录「真实 ID → 匿名编号」；同一会话的 case 共用一份。
type evalIDMap map[string]map[string]string

func loadEvalIDMap(t *testing.T, path string) evalIDMap {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return evalIDMap{}
	}
	if err != nil {
		t.Fatalf("读取映射表: %v", err)
	}
	var mapping evalIDMap
	if err := json.Unmarshal(data, &mapping); err != nil {
		t.Fatalf("解析映射表: %v", err)
	}
	return mapping
}

func saveEvalIDMap(t *testing.T, path string, mapping evalIDMap) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建映射表目录: %v", err)
	}
	data, err := json.MarshalIndent(mapping, "", "  ")
	if err != nil {
		t.Fatalf("编码映射表: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("写映射表: %v", err)
	}
}

// code 返回真实 ID 的匿名编号，首次出现时按顺序分配 u1、u2……
func (m evalIDMap) code(conv, real string) string {
	if real == "" {
		return ""
	}
	known := m[conv]
	if known == nil {
		known = make(map[string]string)
		m[conv] = known
	}
	if code, ok := known[real]; ok {
		return code
	}
	code := fmt.Sprintf("u%d", len(known)+1)
	known[real] = code
	return code
}

// replace 把正文中已映射的真实 ID 换成编号；长 ID 优先，避免前缀互相覆盖。
func (m evalIDMap) replace(conv, text string) string {
	known := m[conv]
	if len(known) == 0 || text == "" {
		return text
	}
	reals := make([]string, 0, len(known))
	for real := range known {
		reals = append(reals, real)
	}
	slices.SortFunc(reals, func(a, b string) int { return len(b) - len(a) })
	for _, real := range reals {
		text = strings.ReplaceAll(text, real, known[real])
	}
	return text
}

func writeEvalJSONL(t *testing.T, path string, messages []evalMessage) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建 case 目录: %v", err)
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, message := range messages {
		if err := encoder.Encode(message); err != nil {
			t.Fatalf("编码消息: %v", err)
		}
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}
