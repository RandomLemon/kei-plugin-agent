package agent

import (
	"net/http"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// fastConfig 让随机参与与寻址回复确定性地触发。
func fastConfig(c map[string]any) {
	c["batch_window"] = "1ms"
	c["batch_max_window"] = "5ms"
	c["random_probability"] = 1.0
	c["random_min_participants"] = 1
	c["random_cooldown"] = "0s"
	c["mention_min_interval"] = "0s"
}

func atEvent(channel, user, name, text string) *bot.Event {
	ev := groupEvent(channel, user, name, text)
	ev.Message.Segments = []bot.Segment{
		{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "bot1", bot.KeyUserName: "小助手"}},
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
	}
	return ev
}

func TestFilterReasons(t *testing.T) {
	t.Run("no_sender", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		ev := groupEvent("g1", "u1", "张三", "hi")
		ev.Sender = nil
		_ = env.deliver(ev)
		if !env.cap.has("no_sender") {
			t.Fatal("want no_sender")
		}
	})
	t.Run("not_group", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		ev := groupEvent("g1", "u1", "张三", "hi")
		ev.Message.Kind = bot.MessagePrivate
		_ = env.deliver(ev)
		if !env.cap.has("not_group") {
			t.Fatal("want not_group")
		}
	})
	t.Run("bot_sender", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		ev := groupEvent("g1", "b1", "某机器人", "hi")
		ev.Sender.IsBot = true
		_ = env.deliver(ev)
		if !env.cap.has("bot_sender") {
			t.Fatal("want bot_sender")
		}
	})
	t.Run("command", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		ev := groupEvent("g1", "u1", "张三", "hi")
		ev.Command = &bot.Command{Name: "echo"}
		_ = env.deliver(ev)
		if !env.cap.has("command") {
			t.Fatal("want command")
		}
	})
	t.Run("agent_command_not_in_history", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		st := env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		before := len(st.snapshotHistory(100))
		ev := groupEvent("g1", "u1", "张三", "hi")
		ev.Command = &bot.Command{Name: "agent", Args: []string{"status"}}
		_ = env.deliver(ev)
		if !env.cap.has("command") {
			t.Fatal("want command")
		}
		if after := len(st.snapshotHistory(100)); after != before {
			t.Fatalf("史条数 %d -> %d，/agent 不应进历史", before, after)
		}
	})
	t.Run("empty_text", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", ""))
		if !env.cap.has("empty_text") {
			t.Fatal("want empty_text")
		}
	})
	t.Run("too_short", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "a"))
		if !env.cap.has("too_short") {
			t.Fatal("want too_short")
		}
	})
	t.Run("channel_off", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		env.command("off")
		_ = env.deliver(groupEvent("g1", "u1", "张三", "hi"))
		if !env.cap.has("channel_off") {
			t.Fatal("want channel_off")
		}
	})
	t.Run("loading", func(t *testing.T) {
		gate := make(chan struct{})
		store := &blockingStorage{fakeStorage: newFakeStorage(), gate: gate}
		env := newTestEnvWith(t, nil, nil, store, nil)
		_ = env.deliver(groupEvent("g1", "u1", "张三", "hi"))
		if !env.cap.has("loading") {
			t.Fatal("want loading")
		}
		close(gate)
	})
	t.Run("not_addressed", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["random_enabled"] = false })
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("not_addressed") {
			t.Fatal("want not_addressed")
		}
	})
	t.Run("hour_quota", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["random_max_per_hour"] = 0 })
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("hour_quota") {
			t.Fatal("want hour_quota")
		}
	})
	t.Run("min_participants", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("min_participants") {
			t.Fatal("want min_participants")
		}
	})
	t.Run("probability", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["random_min_participants"] = 1
			c["random_probability"] = 0.0
		})
		env.setRand(0)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("probability") {
			t.Fatal("want probability")
		}
	})
	t.Run("quiet_hours", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["random_quiet_hours"] = "23:00-07:00"
			c["random_timezone"] = "UTC"
		})
		env.fixNow(time.Date(2026, 9, 26, 23, 30, 0, 0, time.UTC))
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("quiet_hours") {
			t.Fatal("want quiet_hours")
		}
	})
	t.Run("inflight", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["batch_window"] = "1h" })
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		env.setRand(0)
		env.p.cfg.randomMinParticipants = 0
		env.p.cfg.randomProbability = 1
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好")) // 布防 awaiting
		_ = env.deliver(groupEvent("g1", "u1", "张三", "又来了")) // inflight
		if !env.cap.has("inflight") {
			t.Fatal("want inflight")
		}
	})
}

func TestAddressedAndCooldown(t *testing.T) {
	t.Run("via_at", func(t *testing.T) {
		env := newTestEnv(t, nil, fastConfig)
		env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("被 @ 应触发回复")
		}
		sent := env.fake.at(0)
		if sent.Target.ChannelID != "g1" || sent.Target.Kind != bot.MessageGroup || sent.Target.BotID != "bot1" {
			t.Fatalf("目标错误: %+v", sent.Target)
		}
		if got := plainText(sent.Msg); got != "打球可以啊" {
			t.Fatalf("text = %q", got)
		}
	})
	t.Run("via_reply", func(t *testing.T) {
		env := newTestEnv(t, nil, fastConfig)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		env.p.replyIDs.Add("mid-1")
		ev := groupEvent("g1", "u1", "张三", "再聊聊")
		ev.Message.Segments = []bot.Segment{
			{Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: "mid-1"}},
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: "再聊聊"}},
		}
		_ = env.deliver(ev)
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("引用自己应触发回复")
		}
	})
	t.Run("via_keyword", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["trigger_keywords"] = []any{"小助手"}
		})
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "小助手在吗"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("关键词应触发回复")
		}
	})
	t.Run("cooldown", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["random_cooldown"] = "90s"
		})
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("首条应回复")
		}
		_ = env.deliver(groupEvent("g1", "u2", "李四", "又来了"))
		if !env.cap.has("cooldown") {
			t.Fatal("want cooldown")
		}
	})
}

func TestReplyMentionSender(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["reply_mention_sender"] = true
	})
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u7", "老王", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("应回复")
	}
	msg := env.fake.at(0).Msg
	if len(msg.Segments) != 2 || msg.Segments[0].Type != bot.SegAt {
		t.Fatalf("应前置 At 段: %+v", msg.Segments)
	}
	if got := msg.Segments[0].Data[bot.KeyUserID]; got != "u7" {
		t.Fatalf("At 目标 = %v, want u7", got)
	}
}

func TestGenerateSkipReasons(t *testing.T) {
	cases := []struct {
		name string
		llm  func() string
		want string
	}{
		{"skipped_by_llm", func() string { return "[SKIP]" }, "skipped_by_llm"},
		{"empty_reply", func() string { return "   " }, "empty_reply"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, llmJSON(tc.llm()), fastConfig)
			env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
			_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
			if !env.waitReason(tc.want, 2*time.Second) {
				t.Fatalf("want %s", tc.want)
			}
			if env.fake.count() != 0 {
				t.Fatal("不应发送任何消息")
			}
		})
	}
}

func TestDuplicateReply(t *testing.T) {
	env := newTestEnv(t, llmJSON("你好"), fastConfig)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("首次应回复")
	}
	_ = env.deliver(atEvent("g1", "u1", "张三", "再说一次"))
	if !env.waitReason("duplicate_reply", 2*time.Second) {
		t.Fatal("want duplicate_reply")
	}
	if env.fake.count() != 1 {
		t.Fatalf("重复回复不应发送, count=%d", env.fake.count())
	}
}

func TestLLMError(t *testing.T) {
	bad := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) }
	env := newTestEnv(t, http.HandlerFunc(bad), fastConfig)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitReason("llm_error", 2*time.Second) {
		t.Fatal("want llm_error")
	}
	if env.fake.count() != 0 {
		t.Fatal("LLM 失败不应发送")
	}
}

func TestSendError(t *testing.T) {
	env := newTestEnvWith(t, nil, fastConfig, nil, &failingAPI{})
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitReason("send_error", 2*time.Second) {
		t.Fatal("want send_error")
	}
}

func TestSemaphoreFull(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["limits_max_concurrent"] = 1
	})
	// 直接占满唯一的并发名额，模拟其它会话正在生成。
	evB := atEvent("gB", "u1", "张三", "hi")
	env.waitLoaded(evB)
	if !env.p.sem.TryAcquire() {
		t.Fatal("名额应为空")
	}
	_ = env.deliver(evB)
	if !env.waitReason("semaphore_full", 2*time.Second) {
		t.Fatal("want semaphore_full")
	}
	env.p.sem.Release()
	if env.fake.count() != 0 {
		t.Fatal("名额占满时不应发送")
	}
}

func TestGenerateStale(t *testing.T) {
	env := newTestEnv(t, nil, fastConfig)
	ev := atEvent("g1", "u1", "张三", "hi")
	st := env.waitLoaded(ev)
	st.mu.Lock()
	st.epoch = 9
	st.mu.Unlock()
	env.p.wg.Add(1)
	env.p.generate(st, 1, nil)
	if !env.cap.has("stale") {
		t.Fatal("want stale")
	}
	if env.fake.count() != 0 {
		t.Fatal("stale 不应发送")
	}
}

func TestStatusAndPersonaCommands(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))

	if got := env.command("status"); got != "agent: 开 · persona=default(default) · 历史 0 条 · 近 1 小时回复 0/6 · 上次回复 从未 · llm 错误 0 · 已跳 0" {
		t.Fatalf("status = %q", got)
	}
	if got := env.command("persona"); got != "agent: persona=default 来源=default" {
		t.Fatalf("persona = %q", got)
	}
	if got := env.command("persona", "tsundere"); got != "agent: persona=tsundere" {
		t.Fatalf("persona set = %q", got)
	}
	if got := env.command("persona"); got != "agent: persona=tsundere 来源=override" {
		t.Fatalf("persona after set = %q", got)
	}
	if got := env.command("persona", "nope"); got != "agent: 未找到人格 nope" {
		t.Fatalf("persona unknown = %q", got)
	}
	if got := env.command("bogus"); got != agentUsage() {
		t.Fatalf("unknown = %q", got)
	}
	// 覆盖应写穿透到 Storage。
	key := "agent:override:mock:bot1:g1"
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, ok := env.store.value(key); ok && v != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	v, ok := env.store.value(key)
	if !ok || !containsStr(v, "tsundere") {
		t.Fatalf("override 未持久化: %q ok=%v", v, ok)
	}
}

func TestResetCommand(t *testing.T) {
	env := newTestEnv(t, nil, fastConfig)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("应回复")
	}
	env.command("reset")
	st := env.p.stateFor(groupEvent("g1", "u1", "张三", "hi"))
	if n := len(st.snapshotHistory(100)); n != 0 {
		t.Fatalf("reset 后历史应为空, got %d", n)
	}
	if st.isDisabled() {
		t.Fatal("reset 后应为开启")
	}
}

func TestStopNoSend(t *testing.T) {
	env := newTestEnv(t, nil, fastConfig)
	ev := atEvent("g1", "u1", "张三", "hi")
	env.waitLoaded(ev)
	if err := env.p.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_ = env.deliver(ev)
	time.Sleep(200 * time.Millisecond)
	if env.fake.count() != 0 {
		t.Fatalf("Stop 后不应发送, count=%d", env.fake.count())
	}
}

func TestStopIsIdempotent(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	if err := env.p.Stop(t.Context()); err != nil {
		t.Fatalf("首次 Stop: %v", err)
	}
	if err := env.p.Stop(t.Context()); err != nil {
		t.Fatalf("二次 Stop: %v", err)
	}
}
