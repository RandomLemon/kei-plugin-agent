package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/adapters/mock"
	"github.com/RandomLemon/kei/pkg/bot"
)

// pluginSink 把 mock 适配器注入的事件直接投递给插件 Handler。
type pluginSink struct{ p *Plugin }

func (s *pluginSink) Emit(ctx context.Context, ev *bot.Event) error {
	return s.p.handleGroupMessage(ctx, ev, bot.NewNoopReply())
}

// TestE2EMockAdapterInject 用 mock 适配器的 HTTP 控制面注入事件，
// 走「事件 → 决策 → LLM 桩 → 发送」完整链路。
func TestE2EMockAdapterInject(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["random_min_participants"] = 2
	})

	ad := mock.New(mock.Options{
		Name:       "bot1",
		Platform:   "mock",
		ListenAddr: "127.0.0.1:0",
		Logger:     slog.Default(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ad.Start(ctx, &pluginSink{p: env.p}) }()
	addr := waitAddr(t, ad)

	inject := func(body string) {
		t.Helper()
		resp, err := http.Post("http://"+addr+"/inject", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("inject: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("inject status = %d", resp.StatusCode)
		}
	}

	inject(`{"text":"今晚谁去打球","user_id":"u1","user_name":"张三","channel_id":"g1"}`)
	waitStateLoaded(t, env, "mock:bot1:g1")
	inject(`{"text":"我也想去","user_id":"u2","user_name":"李四","channel_id":"g1"}`)

	if !env.waitSends(1, 3*time.Second) {
		t.Fatal("mock 适配器注入后应触发一次回复")
	}
	if got := plainText(env.fake.at(0).Msg); got != "打球可以啊" {
		t.Fatalf("文本 = %q", got)
	}

	// /agent off 后不再增长。
	env.command("off")
	before := env.fake.count()
	inject(`{"text":"还在吗","user_id":"u3","user_name":"王五","channel_id":"g1"}`)
	time.Sleep(200 * time.Millisecond)
	if env.fake.count() != before {
		t.Fatalf("off 后仍发送: %d -> %d", before, env.fake.count())
	}
	if err := ad.Stop(context.Background()); err != nil {
		t.Fatalf("adapter stop: %v", err)
	}
}

func waitAddr(t *testing.T, ad *mock.Adapter) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := ad.Addr(); a != "" {
			return a
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("mock 控制面未监听")
	return ""
}

func waitStateLoaded(t *testing.T, env *testEnv, key string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		env.p.mu.RLock()
		st := env.p.channels[key]
		env.p.mu.RUnlock()
		if st != nil && st.isLoaded() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("会话 %s 未完成懒加载", key)
}
