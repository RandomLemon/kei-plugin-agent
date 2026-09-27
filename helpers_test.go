package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// ---- 日志捕获 ----

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

// has 判断是否出现某条 reason 的决策日志。
func (c *logCapture) has(reason string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r["reason"] == reason {
			return true
		}
	}
	return false
}

// hasMsg 判断是否出现某条日志消息。
func (c *logCapture) hasMsg(msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r["msg"] == msg {
			return true
		}
	}
	return false
}

// hasAttr 判断是否出现带指定属性值的日志。
func (c *logCapture) hasAttr(key string, val any) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r[key] == val {
			return true
		}
	}
	return false
}

// attrOf 返回最后一条 msg 日志中 key 的字符串值，不存在返回 ""。
func (c *logCapture) attrOf(msg, key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.records) - 1; i >= 0; i-- {
		r := c.records[i]
		if r["msg"] != msg {
			continue
		}
		s, _ := r[key].(string)
		return s
	}
	return ""
}

// ---- 假 BotAPI ----

type sentMessage struct {
	Target bot.Target
	Msg    *bot.Message
}

type fakeBotAPI struct {
	mu    sync.Mutex
	sends []sentMessage
}

func (f *fakeBotAPI) Send(_ context.Context, target bot.Target, msg *bot.Message) (*bot.SendResult, error) {
	f.mu.Lock()
	f.sends = append(f.sends, sentMessage{Target: target, Msg: msg})
	n := len(f.sends)
	f.mu.Unlock()
	return &bot.SendResult{MessageID: "m-" + strconv.Itoa(n)}, nil
}

func (f *fakeBotAPI) Reply(ctx context.Context, ev *bot.Event, msg *bot.Message) (*bot.SendResult, error) {
	return f.Send(ctx, bot.TargetFromEvent(ev), msg)
}

func (f *fakeBotAPI) Logger() *slog.Logger { return slog.Default() }
func (f *fakeBotAPI) Storage() bot.Storage { return nil }

func (f *fakeBotAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

func (f *fakeBotAPI) at(i int) sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends[i]
}

func (f *fakeBotAPI) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sends))
	for _, s := range f.sends {
		out = append(out, plainText(s.Msg))
	}
	return out
}

// failingAPI 的 Send 永远失败。
type failingAPI struct {
	mu sync.Mutex
	n  int
}

func (f *failingAPI) Send(context.Context, bot.Target, *bot.Message) (*bot.SendResult, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return nil, errors.New("send boom")
}

func (f *failingAPI) Reply(context.Context, *bot.Event, *bot.Message) (*bot.SendResult, error) {
	return nil, errors.New("send boom")
}

func (f *failingAPI) Logger() *slog.Logger { return slog.Default() }
func (f *failingAPI) Storage() bot.Storage { return nil }

func plainText(msg *bot.Message) string {
	if msg == nil {
		return ""
	}
	return msg.PlainText()
}

// ---- 假 Storage ----

type fakeStorage struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newFakeStorage() *fakeStorage { return &fakeStorage{m: map[string][]byte{}} }

func (s *fakeStorage) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return nil, bot.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (s *fakeStorage) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = append([]byte(nil), value...)
	return nil
}

func (s *fakeStorage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

func (s *fakeStorage) value(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return string(v), ok
}

// blockingStorage 的 Get 会一直阻塞到 gate 关闭。
type blockingStorage struct {
	*fakeStorage
	gate chan struct{}
}

func (s *blockingStorage) Get(ctx context.Context, key string) ([]byte, error) {
	select {
	case <-s.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.fakeStorage.Get(ctx, key)
}

// ---- 假 Registrar ----

type fakeRegistrar struct {
	mu    sync.Mutex
	rules []*bot.Rule
}

func (r *fakeRegistrar) add(rule *bot.Rule) {
	r.mu.Lock()
	r.rules = append(r.rules, rule)
	r.mu.Unlock()
}

func (r *fakeRegistrar) OnCommand(name string, h bot.Handler, opts ...bot.Option) {
	rule := &bot.Rule{Handler: h, Command: name}
	for _, o := range opts {
		o(rule)
	}
	r.add(rule)
}

func (r *fakeRegistrar) OnRegex(string, bot.Handler, ...bot.Option)     {}
func (r *fakeRegistrar) OnKeyword([]string, bot.Handler, ...bot.Option) {}
func (r *fakeRegistrar) OnAll(bot.Handler, ...bot.Option)               {}
func (r *fakeRegistrar) Use(bot.Middleware)                             {}

func (r *fakeRegistrar) OnEvent(t bot.EventType, h bot.Handler, opts ...bot.Option) {
	rule := &bot.Rule{Handler: h, EventType: t}
	for _, o := range opts {
		o(rule)
	}
	r.add(rule)
}

func (r *fakeRegistrar) byID(id string) bot.Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rule := range r.rules {
		if rule.ID == id {
			return rule.Handler
		}
	}
	return nil
}

// rule 返回指定 ID 的规则（含 Kind 等过滤字段），未注册返回 nil。
func (r *fakeRegistrar) rule(id string) *bot.Rule {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rule := range r.rules {
		if rule.ID == id {
			return rule
		}
	}
	return nil
}

// ---- 测试环境 ----

type testEnv struct {
	t      *testing.T
	p      *Plugin
	api    bot.BotAPI
	fake   *fakeBotAPI // 使用 fakeBotAPI 时非 nil
	reg    *fakeRegistrar
	store  *fakeStorage // 使用 fakeStorage 时非 nil
	cap    *logCapture
	srv    *httptest.Server
	cancel context.CancelFunc
}

func llmJSON(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}
}

func baseConfigMap(url string) map[string]any {
	return map[string]any{
		"personas": map[string]any{
			"default":  map[string]any{"prompt": "默认人格"},
			"tsundere": map[string]any{"prompt": "傲娇", "display_name": "小傲娇"},
		},
		"llm_base_url": url,
		"llm_api_key":  "test-key",
		"llm_model":    "test-model",
	}
}

// newTestEnv 构造并启动一个完整插件实例。mutate 可覆盖配置。
func newTestEnv(t *testing.T, llm http.Handler, mutate func(map[string]any)) *testEnv {
	t.Helper()
	return newTestEnvWith(t, llm, mutate, nil, nil)
}

// newTestEnvWith 允许注入自定义 Storage 与 BotAPI。
func newTestEnvWith(t *testing.T, llm http.Handler, mutate func(map[string]any), store bot.Storage, api bot.BotAPI) *testEnv {
	t.Helper()
	if llm == nil {
		llm = llmJSON("打球可以啊")
	}
	srv := httptest.NewServer(llm)
	cfg := baseConfigMap(srv.URL)
	if mutate != nil {
		mutate(cfg)
	}
	if store == nil {
		store = newFakeStorage()
	}
	if api == nil {
		api = &fakeBotAPI{}
	}
	reg := &fakeRegistrar{}
	cap := &logCapture{}
	p := &Plugin{}

	pc := bot.PluginContext{
		Name:       "agent",
		Config:     bot.NewConfig(cfg),
		Logger:     slog.New(cap),
		Storage:    store,
		HTTPClient: srv.Client(),
		Bot:        api,
	}
	ctx := bot.WithPluginContext(context.Background(), pc)
	if err := p.Setup(ctx, reg); err != nil {
		srv.Close()
		t.Fatalf("Setup: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	if err := p.Start(runCtx); err != nil {
		srv.Close()
		t.Fatalf("Start: %v", err)
	}
	env := &testEnv{t: t, p: p, api: api, reg: reg, cap: cap, srv: srv, cancel: cancel}
	if f, ok := api.(*fakeBotAPI); ok {
		env.fake = f
	}
	if f, ok := store.(*fakeStorage); ok {
		env.store = f
	}
	t.Cleanup(func() {
		_ = p.Stop(context.Background())
		cancel()
		srv.Close()
	})
	return env
}

func (e *testEnv) groupHandler() bot.Handler {
	h := e.reg.byID("agent:group")
	if h == nil {
		e.t.Fatal("agent:group 规则未注册")
	}
	return h
}

func (e *testEnv) commandHandler() bot.Handler {
	h := e.reg.byID("agent:admin")
	if h == nil {
		e.t.Fatal("agent:admin 规则未注册")
	}
	return h
}

// deliver 把事件投递给群消息 Handler。
func (e *testEnv) deliver(ev *bot.Event) error {
	return e.groupHandler()(context.Background(), ev, bot.NewNoopReply())
}

// command 投递一条 /agent 命令。
func (e *testEnv) command(args ...string) string {
	e.t.Helper()
	ev := groupEvent("g1", "u1", "张三", "")
	ev.Command = &bot.Command{Name: "agent", Args: args}
	r := bot.NewNoopReply()
	if err := e.commandHandler()(context.Background(), ev, r); err != nil {
		e.t.Fatalf("command %v: %v", args, err)
	}
	return r.PlainText()
}

func (e *testEnv) fixNow(t time.Time) {
	e.p.now = func() time.Time { return t }
}

func (e *testEnv) setRand(f float64) {
	e.p.randFloat = func() float64 { return f }
}

func (e *testEnv) waitSends(n int, d time.Duration) bool {
	if e.fake == nil {
		e.t.Fatal("waitSends 需要 fakeBotAPI")
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if e.fake.count() >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return e.fake.count() >= n
}

// waitReason 等待某条 reason 出现。
func (e *testEnv) waitReason(reason string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if e.cap.has(reason) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return e.cap.has(reason)
}

func groupEvent(channelID, userID, userName, text string) *bot.Event {
	return &bot.Event{
		ID:       "ev-" + channelID + "-" + userID + "-" + text,
		Type:     bot.EventMessage,
		Platform: "mock",
		BotID:    "bot1",
		Time:     time.Now(),
		Sender:   &bot.User{ID: userID, Name: userName},
		Channel:  &bot.Channel{ID: channelID, Name: "群" + channelID, Kind: bot.MessageGroup},
		Message: &bot.Message{
			Kind:     bot.MessageGroup,
			Segments: []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}}},
		},
	}
}

// privateEvent 构造一条私聊消息事件（无 Channel）。
func privateEvent(userID, userName, text string) *bot.Event {
	return &bot.Event{
		ID:       "ev-p-" + userID + "-" + text,
		Type:     bot.EventMessage,
		Platform: "mock",
		BotID:    "bot1",
		Time:     time.Now(),
		Sender:   &bot.User{ID: userID, Name: userName},
		Message: &bot.Message{
			Kind:     bot.MessagePrivate,
			Segments: []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}}},
		},
	}
}

// privateHandler 返回 agent:private 规则的 Handler。
func (e *testEnv) privateHandler() bot.Handler {
	h := e.reg.byID("agent:private")
	if h == nil {
		e.t.Fatal("agent:private 规则未注册")
	}
	return h
}

// deliverPrivate 把事件投递给私聊 Handler。
func (e *testEnv) deliverPrivate(ev *bot.Event) error {
	return e.privateHandler()(context.Background(), ev, bot.NewNoopReply())
}

// privateCommand 投递一条来自私聊的 /agent 命令，返回回复文本。
func (e *testEnv) privateCommand(userID string, args ...string) string {
	e.t.Helper()
	ev := privateEvent(userID, "张三", "")
	ev.Command = &bot.Command{Name: "agent", Args: args}
	r := bot.NewNoopReply()
	if err := e.commandHandler()(context.Background(), ev, r); err != nil {
		e.t.Fatalf("private command %v: %v", args, err)
	}
	return r.PlainText()
}

// waitLoaded 等待会话状态完成 Storage 懒加载。
func (e *testEnv) waitLoaded(ev *bot.Event) *channelState {
	e.t.Helper()
	st := e.p.stateFor(ev)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if st.isLoaded() {
			return st
		}
		time.Sleep(time.Millisecond)
	}
	e.t.Fatal("会话状态懒加载超时")
	return nil
}
