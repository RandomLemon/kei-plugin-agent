package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// runtimeHost 是测试内启动的 devhost：mock 适配器监听随机端口，日志写入缓冲。
type runtimeHost struct {
	addr string
	logs *runtimeLogBuffer
	sent *http.Client
}

type runtimeSentRecord struct {
	Request struct {
		Target struct {
			ChannelID string
		}
	}
}

// runtimeLogBuffer 收集宿主输出，供并发读取。
type runtimeLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *runtimeLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *runtimeLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// startRuntimeHostFor 启动 devhost，extra 覆盖默认环境中的同名键；调用方自行轮询就绪。
func startRuntimeHostFor(t *testing.T, binary, dsn string, timeout time.Duration, extra ...string) *runtimeHost {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配端口: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	// cmd.Dir 改变了相对路径的解析基准，先转绝对路径。
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	// t.Context() 在 Cleanup 前就会取消并强杀宿主，这里用不随测试取消的 ctx，超时只作兜底。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), timeout)
	cmd := exec.CommandContext(ctx, binary)
	cmd.Dir = filepath.Join("..", "..", "cmd/devhost")
	cmd.Env = mergeRuntimeEnv(append(os.Environ(),
		"KEI_BOTS_QQ_MAIN_ENABLED=false",
		"KEI_BOTS_MOCK_MAIN_LISTEN_ADDR="+addr,
		"KEI_AUTH_ADMIN_USERS=mock:u1",
		"KEI_LOG_FORMAT=text", // 工具调用按文本日志解析
		"KEI_PLUGINS_PERSONA_RANDOM_ENABLED=false",
		"KEI_PLUGINS_PERSONA_MEMORY_DATABASE_DSN="+dsn,
	), extra)
	logs := &runtimeLogBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("启动宿主: %v", err)
	}
	// 先发中断让宿主正常退出并检查退出状态；ctx 超时只作兜底强杀。
	t.Cleanup(func() {
		defer cancel()
		_ = cmd.Process.Signal(os.Interrupt)
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			t.Errorf("宿主退出: %v", err)
		}
	})
	return &runtimeHost{
		addr: addr,
		logs: logs,
		sent: &http.Client{Timeout: 2 * time.Second},
	}
}

// mergeRuntimeEnv 合并环境变量，extra 覆盖 base 中的同名键。
func mergeRuntimeEnv(base, extra []string) []string {
	values := make(map[string]string, len(base)+len(extra))
	order := make([]string, 0, len(base)+len(extra))
	for _, entry := range append(append([]string{}, base...), extra...) {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	merged := make([]string, 0, len(order))
	for _, key := range order {
		merged = append(merged, key+"="+values[key])
	}
	return merged
}

func (h *runtimeHost) inject(t *testing.T, userID, message, channel string) {
	t.Helper()
	if err := h.tryInject(userID, message, channel); err != nil {
		t.Fatal(err)
	}
}

// tryInject 在宿主启动期间可能连接失败，等待就绪时用。
func (h *runtimeHost) tryInject(userID, message, channel string) error {
	body, err := json.Marshal(map[string]string{
		"user_id":    userID,
		"user_name":  "测试用户",
		"channel_id": channel,
		"text":       message,
	})
	if err != nil {
		return err
	}
	response, err := h.sent.Post("http://"+h.addr+"/inject", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("注入状态 = %d", response.StatusCode)
	}
	return nil
}

// waitReady 轮询 /agent memory status，直到记忆库就绪；避免依赖一次 LLM 往返判断启动完成。
// 探测发到专用频道，迟到的 status 回复不会被算作测试频道的回复。
func (h *runtimeHost) waitReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := h.tryInject("u1", "/agent memory status", "ready"); err == nil &&
			strings.Contains(h.sentBody(), "数据库=就绪") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("宿主未在等待时间内就绪：\n%s", h.logs.String())
}

func (h *runtimeHost) sentRecords(t *testing.T) []runtimeSentRecord {
	t.Helper()
	records, err := h.trySentRecords()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func (h *runtimeHost) trySentRecords() ([]runtimeSentRecord, error) {
	response, err := h.sent.Get("http://" + h.addr + "/sent")
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	var records []runtimeSentRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	return records, nil
}

// sentBody 返回原始发送记录 JSON，供只做子串断言的验收使用。
func (h *runtimeHost) sentBody() string {
	response, err := h.sent.Get("http://" + h.addr + "/sent")
	if err != nil {
		return ""
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return ""
	}
	return string(data)
}

func (h *runtimeHost) sentFor(t *testing.T, channel string) []runtimeSentRecord {
	t.Helper()
	var records []runtimeSentRecord
	for _, record := range h.sentRecords(t) {
		if record.Request.Target.ChannelID == channel {
			records = append(records, record)
		}
	}
	return records
}

// waitSent 等待频道的发送记录超过 before 条。
func (h *runtimeHost) waitSent(t *testing.T, channel string, before int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(h.sentFor(t, channel)) > before {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("频道 %s 未在 %s 内收到回复：\n%s", channel, timeout, h.logs.String())
}

func (h *runtimeHost) logsSince(offset int) string {
	return h.logs.String()[offset:]
}
