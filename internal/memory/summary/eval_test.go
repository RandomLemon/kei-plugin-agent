package summary

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// evalMessage 是评测 case 中的一行消息，字段取 store.Message 的子集（§16.3.9）。
type evalMessage struct {
	MsgID       string    `json:"msg_id"`
	SenderID    string    `json:"sender_id"`
	SenderName  string    `json:"sender_name"`
	SenderIsBot bool      `json:"sender_is_bot"`
	IsSelf      bool      `json:"is_self"`
	Persona     string    `json:"persona"`
	Text        string    `json:"text"`
	ReplyTo     string    `json:"reply_to"`
	Mentions    []string  `json:"mentions"`
	SentAt      time.Time `json:"sent_at"`
}

// evalEnv 读取评测环境变量；任一缺失即跳过，保证普通质量门无需评测设施。
func evalEnv(t *testing.T, keys ...string) map[string]string {
	t.Helper()
	values := make(map[string]string, len(keys))
	var missing []string
	for _, key := range keys {
		value := os.Getenv(key)
		if value == "" {
			missing = append(missing, key)
		}
		values[key] = value
	}
	if len(missing) > 0 {
		t.Skip("未设置评测环境变量: " + strings.Join(missing, ", "))
	}
	return values
}

// readEvalMessages 读取一份 messages.jsonl。
func readEvalMessages(t *testing.T, path string) []evalMessage {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开 %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var messages []evalMessage
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message evalMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("解析 %s: %v", path, err)
		}
		messages = append(messages, message)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return messages
}
