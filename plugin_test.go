package agent

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// TestStageCtxCancelDoesNotKillRuntime 回归 kei 的生命周期语义：传进 Start 的 ctx 是
// 「阶段上下文」，阶段函数一返回就被 cancel（internal/pluginmgr 的 run 内 defer cancel）。
// 插件级 ctx 必须由 context.WithoutCancel 派生，否则懒加载、写穿透、LLM 与发送全部失效。
func TestStageCtxCancelDoesNotKillRuntime(t *testing.T) {
	srv := httptest.NewServer(llmJSON("打球可以啊"))
	defer srv.Close()

	cfg := baseConfigMap(srv.URL)
	fastConfig(cfg)
	cfg["private_policy"] = "open"

	api := &fakeBotAPI{}
	reg := &fakeRegistrar{}
	p := &Plugin{}
	pc := bot.PluginContext{
		Name:       "agent",
		Config:     bot.NewConfig(cfg),
		Logger:     slog.New(&logCapture{}),
		Storage:    newFakeStorage(),
		HTTPClient: srv.Client(),
		Bot:        api,
	}
	stageCtx, cancelStage := context.WithCancel(bot.WithPluginContext(context.Background(), pc))
	if err := p.Setup(stageCtx, reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := p.Start(stageCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancelStage() // 模拟 kei 阶段结束时的 defer cancel()
	t.Cleanup(func() { _ = p.Stop(context.Background()) })

	h := reg.byID("agent:private")
	if h == nil {
		t.Fatal("agent:private 规则未注册")
	}
	// 首条过短消息只为建立并加载会话状态。
	_ = h(context.Background(), privateEvent("u1", "张三", "a"), bot.NewNoopReply())
	st := p.stateFor(privateEvent("u1", "张三", "a"))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !st.isLoaded() {
		time.Sleep(time.Millisecond)
	}
	_ = h(context.Background(), privateEvent("u1", "张三", "在吗"), bot.NewNoopReply())

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && api.count() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if api.count() != 1 {
		t.Fatalf("阶段 ctx 取消后仍应能回复, count=%d", api.count())
	}
	if got := plainText(api.at(0).Msg); got != "打球可以啊" {
		t.Fatalf("text = %q", got)
	}
}

// TestStopWaitsForPendingWrite 回归：Stop 必须先等在途写穿透落库，再取消插件级 ctx。
//
// 写穿透的 ctx 派生自 p.ctx；持久化后端（sqlite/mysql）下若先 cancel，最后一次
// 覆盖或策略变更会被取消丢弃，而内存后端上观察不到这个丢失。
func TestStopWaitsForPendingWrite(t *testing.T) {
	store := &gatedSetStorage{
		fakeStorage: newFakeStorage(),
		started:     make(chan struct{}),
		gate:        make(chan struct{}),
	}
	p := &Plugin{}
	pc := bot.PluginContext{
		Name:       "agent",
		Config:     bot.NewConfig(baseConfigMap("http://127.0.0.1:1/v1")),
		Logger:     slog.New(&logCapture{}),
		Storage:    store,
		HTTPClient: &http.Client{Timeout: time.Second},
		Bot:        &fakeBotAPI{},
	}
	ctx := bot.WithPluginContext(context.Background(), pc)
	if err := p.Setup(ctx, &fakeRegistrar{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 触发一次写穿透：把群聊名单模式从配置默认的 open 改成 whitelist。
	if !p.setPolicyMode(scopeGroup, "whitelist") {
		t.Fatal("setPolicyMode 未生效")
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("写穿透未开始")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(context.Background()) }()
	select {
	case <-stopped:
		t.Fatal("Stop 在在途写穿透结束前返回")
	case <-time.After(50 * time.Millisecond):
	}

	close(store.gate)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("写穿透结束后 Stop 未返回")
	}

	if store.err != nil {
		t.Fatalf("在途写入被 Stop 取消: %v", store.err)
	}
	raw, ok := store.value(policyKey)
	if !ok || !strings.Contains(raw, "whitelist") {
		t.Fatalf("策略未落库: %q ok=%v", raw, ok)
	}
}

// gatedSetStorage 的 Set 会阻塞到 gate 关闭（或 ctx 取消，此时记录 err）。
type gatedSetStorage struct {
	*fakeStorage
	started chan struct{}
	gate    chan struct{}
	err     error
}

func (s *gatedSetStorage) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	close(s.started)
	select {
	case <-s.gate:
	case <-ctx.Done():
		s.err = ctx.Err()
		return s.err
	}
	return s.fakeStorage.Set(ctx, key, value, ttl)
}
