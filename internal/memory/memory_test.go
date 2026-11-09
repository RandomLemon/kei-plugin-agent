package memory

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// logCapture 记录每条日志的消息与属性。
type logCapture struct {
	mu      sync.Mutex
	records []map[string]any
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, m)
	c.mu.Unlock()
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// find 返回第一条 operation 等于 op 的日志。
func (c *logCapture) find(op string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r["operation"] == op {
			return r
		}
	}
	return nil
}

func TestStartUnreachableDSN(t *testing.T) {
	logs := &logCapture{}
	s := New(Config{
		DatabaseDSN:    "postgres://u:p@127.0.0.1:1/db?sslmode=disable",
		Location:       time.UTC,
		QueueSize:      10,
		IngestBatch:    5,
		IngestInterval: time.Second,
	}, slog.New(logs), nil)
	s.connectBackoffMin = 10 * time.Millisecond

	begin := time.Now()
	s.Start(t.Context())
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("Start 耗时 %v，应立即返回", elapsed)
	}
	time.Sleep(50 * time.Millisecond) // 让连库协程至少失败、退避一轮
	if got := s.Stats().State; got != StateConnecting {
		t.Fatalf("State = %q, want %q", got, StateConnecting)
	}
	if logs.find("memory_connect") == nil {
		t.Fatal("连库失败应记 memory_connect 日志")
	}
	s.Enqueue(groupRecord("m1"))
	s.Enqueue(groupRecord("m2"))

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	begin = time.Now()
	s.Stop(ctx)
	if elapsed := time.Since(begin); elapsed > 500*time.Millisecond {
		t.Fatalf("Stop 耗时 %v", elapsed)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("Stop 返回后连库协程仍未退出")
	}
	if got := s.Stats().State; got != StateStopped {
		t.Fatalf("State = %q, want %q", got, StateStopped)
	}
	if r := logs.find("memory_stop"); r == nil || r["dropped"] != int64(2) {
		t.Fatalf("未就绪时停止应记 dropped=2，实际 %v", r)
	}
	if s.Enqueue(Record{}) {
		t.Fatal("停止后 Enqueue 应返回 false")
	}
	s.Stop(ctx) // 幂等
}

func TestStopWithoutStart(t *testing.T) {
	s := New(Config{QueueSize: 1, IngestBatch: 1, IngestInterval: time.Second}, discardLogger(), nil)
	s.Stop(t.Context())
	if got := s.Stats().State; got != StateStopped {
		t.Fatalf("State = %q, want %q", got, StateStopped)
	}
	s.Start(t.Context())
	if s.done != nil {
		t.Fatal("Stop 之后 Start 不应启动后台协程")
	}
}
