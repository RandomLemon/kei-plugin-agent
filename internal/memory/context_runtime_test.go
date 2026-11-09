package memory

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

// runtimeDBProxy 只切断本次宿主的数据库链路，不停止共享 PostgreSQL。
func runtimeDBProxy(t *testing.T, destination string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var mu sync.Mutex
	var connections []net.Conn
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			incoming, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = incoming.Close() }()
				dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
				upstream, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", destination)
				dialCancel()
				if err != nil {
					return
				}
				defer func() { _ = upstream.Close() }()
				mu.Lock()
				if ctx.Err() != nil {
					mu.Unlock()
					return
				}
				connections = append(connections, incoming, upstream)
				mu.Unlock()
				copied := make(chan struct{})
				go func() {
					defer close(copied)
					_, _ = io.Copy(upstream, incoming)
					_ = upstream.Close()
					_ = incoming.Close()
				}()
				_, _ = io.Copy(incoming, upstream)
				_ = upstream.Close()
				_ = incoming.Close()
				<-copied
			})
		}
	})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = listener.Close()
			mu.Lock()
			for _, connection := range connections {
				_ = connection.Close()
			}
			mu.Unlock()
			wg.Wait()
		})
	}
	t.Cleanup(stop)
	return listener.Addr().String(), stop
}

// TestRuntimeHostContext 验收真实宿主的摘要注入及数据库故障降级。
// 只向聊天模型发送下面固定的合成聚餐数据，不读取或发送真实会话。
func TestRuntimeHostContext(t *testing.T) {
	binary := os.Getenv("PERSONA_MEMORY_CONTEXT_HOST_BINARY")
	if binary == "" {
		t.Skip("未指定 S5 运行验收宿主")
	}
	dsn := memtest.NewDB(t)
	db := memtest.Open(t, dsn)
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	yesterday := store.LocalDate(time.Now(), time.UTC, 4*time.Hour).AddDate(0, 0, -1) // 与下面宿主的 random_timezone=UTC、默认 memory_daily_at=04:00 一致
	seed := store.Summary{ConvKey: "group:mock:g1", LocalDate: yesterday, Kind: "daily", Overview: "周末聚餐安排", Version: 1, Topics: []store.Topic{{ConvKey: "group:mock:g1", LocalDate: yesterday, TopicKey: "runtime:dinner", Title: "周末聚餐", Summary: "周六18点在城东餐厅聚餐，预算人均100元。", Importance: 0.9}}}
	if err := db.WithContext(t.Context()).Create(&seed).Error; err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Hostname() == "" {
		t.Fatal("运行验收需要 URL 形式的测试 DSN")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	proxy, cutDB := runtimeDBProxy(t, net.JoinHostPort(parsed.Hostname(), port))
	parsed.Host = proxy
	host := startRuntimeHostFor(t, binary, parsed.String(), 2*time.Minute,
		"KEI_LOG_LEVEL=debug",
		"KEI_PLUGINS_PERSONA_DEBUG_PROMPTS=true",
		"KEI_PLUGINS_PERSONA_GROUP_LIST=[g1]",
		"KEI_PLUGINS_PERSONA_TRIGGER_KEYWORDS=[bot]",
		"KEI_PLUGINS_PERSONA_RANDOM_TIMEZONE=UTC",
		"KEI_PLUGINS_PERSONA_BATCH_WINDOW=20ms",
		"KEI_PLUGINS_PERSONA_BATCH_MAX_WINDOW=30ms",
		"KEI_PLUGINS_PERSONA_MENTION_MIN_INTERVAL=0s",
		"KEI_PLUGINS_PERSONA_REPLY_DEDUPE=false",
		"KEI_PLUGINS_PERSONA_LLM_MAX_TOKENS=2048",
		"KEI_PLUGINS_PERSONA_MEMORY_CONTEXT_TIMEOUT=200ms",
	)
	host.waitReady(t, 15*time.Second)
	host.inject(t, "u1", "bot 前两天我们聊了什么", "g1")
	host.waitSent(t, "g1", 0, 45*time.Second)
	if body := host.sentBody(); !strings.Contains(body, "聚餐") && !strings.Contains(body, "城东餐厅") {
		t.Fatal("真实模型回复未提及合成摘要话题")
	}
	if logs := host.logs.String(); !strings.Contains(logs, "# 记忆") || !strings.Contains(logs, "近期重点") {
		t.Fatal("debug 提示词日志缺少记忆块")
	}
	cutDB()
	before := len(host.sentFor(t, "g1"))
	host.inject(t, "u1", "bot 现在还能回复吗", "g1")
	host.waitSent(t, "g1", before, 45*time.Second)
	if !strings.Contains(host.logs.String(), "memory_context") {
		t.Fatal("数据库故障未记录读取告警")
	}
	t.Log("真实宿主：记忆块出现在 debug 提示词，回复提到聚餐；切断该宿主数据库链路后仍正常回复并记录读取告警")
}
