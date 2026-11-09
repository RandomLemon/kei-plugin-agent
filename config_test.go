package persona

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(bot.NewConfig(map[string]any{
		"personas":    map[string]any{"default": "普通群友"},
		"llm_api_key": "k",
		"llm_model":   "m",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"default_persona", cfg.defaultPersona, "default"},
		{"llm_base_url", cfg.llmBaseURL, defaultLLMBaseURL},
		{"llm_temperature", cfg.llmTemperature, 0.8},
		{"llm_max_tokens", cfg.llmMaxTokens, 200},
		{"llm_timeout", cfg.llmTimeout, 20 * time.Second},
		{"llm_max_retries", cfg.llmMaxRetries, 1},
		{"llm_skip_token", cfg.llmSkipToken, "[SKIP]"},
		{"llm_skip_enabled", cfg.llmSkipEnabled, true},
		{"llm_history_max_chars", cfg.llmHistoryMaxChars, 4000},
		{"trigger_min_chars", cfg.triggerMinChars, 2},
		{"ignore_bots", cfg.ignoreBots, true},
		{"respond_to_commands", cfg.respondToCommands, false},
		{"group_policy", cfg.groupPolicy, "open"},
		{"private_policy", cfg.privatePolicy, "off"},
		{"mention_reply_probability", cfg.mentionReplyProbability, 1.0},
		{"mention_min_interval", cfg.mentionMinInterval, 10 * time.Second},
		{"random_enabled", cfg.randomEnabled, true},
		{"random_probability", cfg.randomProbability, 0.12},
		{"random_cooldown", cfg.randomCooldown, 90 * time.Second},
		{"random_max_per_hour", cfg.randomMaxPerHour, 6},
		{"random_min_participants", cfg.randomMinParticipants, 2},
		{"random_activity_window", cfg.randomActivityWindow, 5 * time.Minute},
		{"batch_window", cfg.batchWindow, 2500 * time.Millisecond},
		{"batch_max_window", cfg.batchMaxWindow, 8 * time.Second},
		{"context_max_messages", cfg.contextMaxMessages, 20},
		{"context_max_channels", cfg.contextMaxChannels, 512},
		{"reply_max_chars", cfg.replyMaxChars, 200},
		{"reply_mention_sender", cfg.replyMentionSender, false},
		{"reply_dedupe", cfg.replyDedupe, true},
		{"limits_max_concurrent", cfg.limitsMaxConcurrent, 2},
		{"debug_prompts", cfg.debugPrompts, false},
		{"llm_vision_enabled", cfg.llmVisionEnabled, false},
		{"llm_vision_max_images", cfg.llmVisionMaxImages, 4},
		{"llm_vision_max_image_bytes", cfg.llmVisionMaxImageBytes, 4194304},
		{"tools_max_rounds", cfg.toolsMaxRounds, 2},
		{"tools_call_timeout", cfg.toolsCallTimeout, 8 * time.Second},
		{"tools_result_max_chars", cfg.toolsResultMaxChars, 2000},
		{"mcp_url", cfg.mcpURL, ""},
		{"memory_tools_enabled", cfg.memoryToolsEnabled, false},
		{"mcp_timeout", cfg.mcpTimeout, 8 * time.Second},
		{"memory_database_dsn", cfg.memory.DatabaseDSN, ""},
		{"memory_db_max_open_conns", cfg.memory.MaxOpenConns, 8},
		{"memory_ingest_batch", cfg.memory.IngestBatch, 50},
		{"memory_ingest_interval", cfg.memory.IngestInterval, 5 * time.Second},
		{"memory_queue_size", cfg.memory.QueueSize, 2048},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if cfg.llmVisionAllowedFormats != nil {
		t.Errorf("llm_vision_allowed_formats 默认应为 nil, got %#v", cfg.llmVisionAllowedFormats)
	}
	pf := cfg.personas["default"]
	if pf.DisplayName != "default" || pf.Temperature != 0.8 || pf.MaxTokens != 200 || pf.SkipToken != "[SKIP]" {
		t.Errorf("人格默认回落错误: %+v", pf)
	}
	if pf.hasTools {
		t.Errorf("默认人格应继承工具目录: %+v", pf)
	}
	if len(cfg.groupList) != 0 || len(cfg.privateList) != 0 {
		t.Errorf("名单默认应为空: group=%v private=%v", cfg.groupList, cfg.privateList)
	}
	if cfg.randomTimezone == nil {
		t.Error("randomTimezone 未解析")
	}
	if cfg.memory.Location != cfg.randomTimezone {
		t.Error("memory.Location 应取自 random_timezone")
	}
}

func TestLoadConfigSkipDisabled(t *testing.T) {
	cfg, err := loadConfig(bot.NewConfig(map[string]any{
		"personas":         map[string]any{"default": "普通群友"},
		"llm_model":        "m",
		"llm_skip_enabled": false,
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.llmSkipEnabled {
		t.Fatal("llm_skip_enabled=false 未生效")
	}
}

func TestLoadConfigVisionAllowedFormats(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"personas": map[string]any{"default": "p"}, "llm_model": "m"}
	}

	t.Run("归一为集合", func(t *testing.T) {
		c := base()
		c["llm_vision_allowed_formats"] = []any{"JPG", "image/png", " gif ", "", "image/jpeg;charset=x"}
		cfg, err := loadConfig(bot.NewConfig(c))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		want := map[string]bool{"jpeg": true, "png": true, "gif": true}
		if !reflect.DeepEqual(cfg.llmVisionAllowedFormats, want) {
			t.Errorf("llmVisionAllowedFormats = %#v, want %#v", cfg.llmVisionAllowedFormats, want)
		}
	})

	t.Run("空列表得 nil", func(t *testing.T) {
		c := base()
		c["llm_vision_allowed_formats"] = []any{}
		cfg, err := loadConfig(bot.NewConfig(c))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.llmVisionAllowedFormats != nil {
			t.Errorf("空列表应得 nil, got %#v", cfg.llmVisionAllowedFormats)
		}
	})

	t.Run("缺省得 nil", func(t *testing.T) {
		cfg, err := loadConfig(bot.NewConfig(base()))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.llmVisionAllowedFormats != nil {
			t.Errorf("缺省应得 nil, got %#v", cfg.llmVisionAllowedFormats)
		}
	})
}

func TestLoadConfigAPIKeyOptional(t *testing.T) {
	cfg, err := loadConfig(bot.NewConfig(map[string]any{
		"personas":  map[string]any{"default": "普通群友"},
		"llm_model": "m",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.llmAPIKey != "" {
		t.Fatalf("llmAPIKey = %q, want empty", cfg.llmAPIKey)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"personas":     map[string]any{"default": "p"},
			"llm_base_url": "http://example.test/v1",
			"llm_api_key":  "k",
			"llm_model":    "m",
		}
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"default_persona 未定义", func(c map[string]any) { c["default_persona"] = "cat" },
			"persona: 配置错误 default_persona=cat: 未在 personas 中定义"},
		{"personas 为空", func(c map[string]any) { c["personas"] = map[string]any{} },
			"persona: 配置错误 personas=: 必须是非空对象"},
		{"random_probability 越界", func(c map[string]any) { c["random_probability"] = 1.5 },
			"persona: 配置错误 random_probability=1.5: 必须是 0..1 之间的小数"},
		{"quiet_hours 格式", func(c map[string]any) { c["random_quiet_hours"] = "23:00" },
			"persona: 配置错误 random_quiet_hours=23:00: 必须是 HH:MM-HH:MM 格式"},
		{"context_max_messages 越界", func(c map[string]any) { c["context_max_messages"] = 0 },
			"persona: 配置错误 context_max_messages=0: 必须 >= 1"},
		{"prompt 为空", func(c map[string]any) {
			c["personas"] = map[string]any{"default": map[string]any{"prompt": ""}}
		}, "persona: 配置错误 personas.default.prompt=: prompt 不能为空"},
		{"bindings channel_id 为空", func(c map[string]any) {
			c["bindings"] = []any{map[string]any{"persona": "default"}}
		}, "persona: 配置错误 bindings.channel_id=: channel_id 不能为空"},
		{"bindings persona 未定义", func(c map[string]any) {
			c["bindings"] = []any{map[string]any{"channel_id": "g1", "persona": "nope"}}
		}, "persona: 配置错误 bindings.persona=nope: 未在 personas 中定义"},
		{"时区非法", func(c map[string]any) { c["random_timezone"] = "No/SuchZone" },
			"persona: 配置错误 random_timezone=No/SuchZone: 不是合法时区"},
		{"llm_max_tokens 越界", func(c map[string]any) { c["llm_max_tokens"] = 0 },
			"persona: 配置错误 llm_max_tokens=0: 必须 >= 1"},
		{"random_min_participants 越界", func(c map[string]any) { c["random_min_participants"] = 0 },
			"persona: 配置错误 random_min_participants=0: 必须 >= 1"},
		{"llm_timeout 为负", func(c map[string]any) { c["llm_timeout"] = -1 },
			"persona: 配置错误 llm_timeout=-1s: 必须 >= 0"},
		{"persona temperature 类型错误", func(c map[string]any) {
			c["personas"] = map[string]any{"default": map[string]any{"prompt": "p", "temperature": "hot"}}
		}, "persona: 配置错误 personas.default.temperature=hot: 必须 >= 0"},
		{"persona max_tokens 类型错误", func(c map[string]any) {
			c["personas"] = map[string]any{"default": map[string]any{"prompt": "p", "max_tokens": "many"}}
		}, "persona: 配置错误 personas.default.max_tokens=many: 必须 >= 1"},
		{"group_policy 非法", func(c map[string]any) { c["group_policy"] = "all" },
			"persona: 配置错误 group_policy=all: 必须是 off|open|whitelist|blacklist 之一"},
		{"private_policy 非法", func(c map[string]any) { c["private_policy"] = "on" },
			"persona: 配置错误 private_policy=on: 必须是 off|open|whitelist|blacklist 之一"},
		{"llm_vision_max_images 为负", func(c map[string]any) { c["llm_vision_max_images"] = -1 },
			"persona: 配置错误 llm_vision_max_images=-1: 必须 >= 0"},
		{"llm_vision_max_image_bytes 为零", func(c map[string]any) { c["llm_vision_max_image_bytes"] = 0 },
			"persona: 配置错误 llm_vision_max_image_bytes=0: 必须 >= 1"},
		{"人格 tools 非 MCP 名称", func(c map[string]any) {
			c["personas"] = map[string]any{"default": map[string]any{"prompt": "p", "tools": []any{"knowledge_search"}}}
		}, "persona: 配置错误 personas.default.tools=knowledge_search: 不是合法 MCP 工具名"},
		{"人格 tools 未启用 MCP", func(c map[string]any) {
			c["personas"] = map[string]any{"default": map[string]any{"prompt": "p", "tools": []any{"mcp__search"}}}
		}, "persona: 配置错误 personas.default.tools=mcp__search: 未启用 MCP"},
		{"mcp_url 非法", func(c map[string]any) { c["mcp_url"] = "file:///tmp/mcp" },
			"persona: 配置错误 mcp_url=: 必须是带 host 的 http(s) URL"},
		{"mcp_timeout 为零", func(c map[string]any) { c["mcp_timeout"] = "0s" },
			"persona: 配置错误 mcp_timeout=0s: 必须 > 0"},
		{"MCP 保留请求头", func(c map[string]any) { c["mcp_extra_headers"] = map[string]any{"mCp-SeSsIoN-Id": "secret"} },
			"persona: 配置错误 mcp_extra_headers=: 不得覆盖 MCP 协议请求头"},
		{"MCP 请求头类型", func(c map[string]any) { c["mcp_extra_headers"] = map[string]any{"X-Token": 123} },
			"persona: 配置错误 mcp_extra_headers=: 必须是字符串键值映射"},
		{"tools_max_rounds 为零", func(c map[string]any) { c["tools_max_rounds"] = 0 },
			"persona: 配置错误 tools_max_rounds=0: 必须 >= 1"},
		{"tools_result_max_chars 为零", func(c map[string]any) { c["tools_result_max_chars"] = 0 },
			"persona: 配置错误 tools_result_max_chars=0: 必须 >= 1"},
		{"tools_call_timeout 为负", func(c map[string]any) { c["tools_call_timeout"] = -1 },
			"persona: 配置错误 tools_call_timeout=-1s: 必须 > 0"},
		{"tools_call_timeout 为零", func(c map[string]any) { c["tools_call_timeout"] = "0s" },
			"persona: 配置错误 tools_call_timeout=0s: 必须 > 0"},
		{"memory_ingest_interval 为零", func(c map[string]any) { c["memory_ingest_interval"] = "0s" },
			"persona: 配置错误 memory_ingest_interval=0s: 必须 > 0"},
		{"memory_db_max_open_conns 为零", func(c map[string]any) { c["memory_db_max_open_conns"] = 0 },
			"persona: 配置错误 memory_db_max_open_conns=0: 必须 >= 1"},
		{"memory_ingest_batch 为零", func(c map[string]any) { c["memory_ingest_batch"] = 0 },
			"persona: 配置错误 memory_ingest_batch=0: 必须 >= 1"},
		{"memory_queue_size 为零", func(c map[string]any) { c["memory_queue_size"] = 0 },
			"persona: 配置错误 memory_queue_size=0: 必须 >= 1"},
		{"memory_ingest_batch 大于队列", func(c map[string]any) {
			c["memory_ingest_batch"] = 100
			c["memory_queue_size"] = 10
		}, "persona: 配置错误 memory_ingest_batch=100: 不能大于 memory_queue_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			gotErr := func() string {
				_, err := loadConfig(bot.NewConfig(cfg))
				if err == nil {
					return ""
				}
				return err.Error()
			}()
			if gotErr != tc.want {
				t.Fatalf("错误 = %q, want %q", gotErr, tc.want)
			}
		})
	}
}

func TestLoadConfigTools(t *testing.T) {
	cfg, err := loadConfig(bot.NewConfig(map[string]any{
		"personas": map[string]any{
			"inherit": "p",
			"null":    map[string]any{"prompt": "p", "tools": nil},
			"quiet":   map[string]any{"prompt": "p", "tools": []any{}},
			"picked":  map[string]any{"prompt": "p", "tools": "mcp__future"},
		},
		"default_persona":   "inherit",
		"llm_model":         "m",
		"mcp_url":           "http://mcp.test/tools",
		"mcp_extra_headers": map[string]any{"authorization": "Bearer t"},
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.mcpURL != "http://mcp.test/tools" || cfg.mcpExtraHeaders["Authorization"] != "Bearer t" {
		t.Fatalf("MCP 配置未保留: %+v", cfg)
	}
	if pf := cfg.personas["inherit"]; pf.hasTools {
		t.Fatalf("字符串人格应继承: %+v", pf)
	}
	if pf := cfg.personas["null"]; pf.hasTools {
		t.Fatalf("tools 为 null 应继承: %+v", pf)
	}
	if pf := cfg.personas["quiet"]; !pf.hasTools || len(pf.Tools) != 0 {
		t.Fatalf("tools: [] 应为不使用工具: %+v", pf)
	}
	if pf := cfg.personas["picked"]; !pf.hasTools || !reflect.DeepEqual(pf.Tools, []string{"mcp__future"}) {
		t.Fatalf("名单应按 readStringList 口径解析: %+v", pf)
	}
}

func TestSetupRequiresNetworkPermission(t *testing.T) {
	p := &Plugin{}
	pc := bot.PluginContext{
		Name:   "persona",
		Config: bot.NewConfig(baseConfigMap("http://x")),
	}
	ctx := bot.WithPluginContext(context.Background(), pc)
	err := p.Setup(ctx, &fakeRegistrar{})
	if err == nil || err.Error() != "persona: 需要 network 权限" {
		t.Fatalf("err = %v, want 需要 network 权限", err)
	}
}

func TestSetupRequiresPluginContext(t *testing.T) {
	p := &Plugin{}
	if err := p.Setup(context.Background(), &fakeRegistrar{}); err == nil {
		t.Fatal("缺少 PluginContext 应报错")
	}
}

func TestMemorySchedulingConfig(t *testing.T) {
	for _, tc := range []struct {
		key    string
		value  any
		reason string
	}{
		{"memory_daily_at", "4:00", "必须是 HH:MM"}, {"memory_daily_at", "24:00", "必须是 HH:MM"},
		{"memory_message_retention_days", 0, "必须 >= 1"}, {"memory_summary_retention_days", 0, "必须 >= 1"},
	} {
		t.Run(tc.key+fmt.Sprint(tc.value), func(t *testing.T) {
			c := baseConfigMap("http://x")
			c["memory_database_dsn"] = "test"
			c[tc.key] = tc.value
			_, err := loadConfig(bot.NewConfig(c))
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestDisabledMemoryIgnoresSchedulingConfig(t *testing.T) {
	c := baseConfigMap("http://x")
	c["memory_daily_at"] = "bad"
	c["memory_message_retention_days"] = 0
	c["memory_summary_retention_days"] = 0
	if _, err := loadConfig(bot.NewConfig(c)); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryLLMFallback(t *testing.T) {
	for _, endpoint := range []string{"", "http://other/v1"} {
		c := baseConfigMap("http://main/v1")
		c["memory_database_dsn"] = "test"
		c["llm_extra_headers"] = map[string]any{"X-Test": "main"}
		c["memory_llm_base_url"] = endpoint
		cfg, err := loadConfig(bot.NewConfig(c))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.memory.LLMTimeout != 120*time.Second || cfg.memory.LLMModelSmall != cfg.llmModel || cfg.memory.LLMModelLarge != cfg.llmModel {
			t.Fatal("模型或超时回落错误")
		}
		if endpoint == "" {
			if cfg.memory.LLMAPIKey != cfg.llmAPIKey || cfg.memory.LLMHeaders["X-Test"] != "main" {
				t.Fatal("未继承主端点凭据")
			}
		} else if cfg.memory.LLMAPIKey != "" || len(cfg.memory.LLMHeaders) != 0 {
			t.Fatal("凭据跨端点继承")
		}
		c["memory_llm_api_key"] = "explicit"
		c["memory_llm_extra_headers"] = map[string]any{"X-Test": "explicit"}
		c["memory_llm_model_small"] = "s"
		cfg, err = loadConfig(bot.NewConfig(c))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.memory.LLMAPIKey != "explicit" || cfg.memory.LLMHeaders["X-Test"] != "explicit" || cfg.memory.LLMModelLarge != "s" {
			t.Fatal("显式值未生效")
		}
	}
	for _, tc := range []struct{ key, value, reason string }{{"memory_llm_base_url", "bad", "必须是带 host 的 http(s) URL"}, {"memory_llm_timeout", "0s", "必须 > 0"}} {
		c := baseConfigMap("http://main")
		c["memory_database_dsn"] = "test"
		c[tc.key] = tc.value
		_, err := loadConfig(bot.NewConfig(c))
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestMemoryContextConfig(t *testing.T) {
	for _, tc := range []struct {
		key    string
		value  any
		reason string
	}{{"memory_context_timeout", "0s", "必须 > 0"}, {"memory_context_max_chars", 0, "必须 >= 1"}, {"memory_context_max_chars", "invalid", "必须 >= 1"}} {
		c := baseConfigMap("http://x")
		c["memory_database_dsn"] = "test"
		c[tc.key] = tc.value
		_, err := loadConfig(bot.NewConfig(c))
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Fatalf("%s: %v", tc.key, err)
		}
		delete(c, "memory_database_dsn")
		if _, err = loadConfig(bot.NewConfig(c)); err != nil {
			t.Fatalf("未启用记忆时校验了上下文配置：%v", err)
		}
	}
	c := baseConfigMap("http://x")
	c["memory_database_dsn"] = "test"
	cfg, err := loadConfig(bot.NewConfig(c))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.memory.ContextTimeout != 1500*time.Millisecond || cfg.memory.ContextMaxChars != 1200 {
		t.Fatal("上下文默认值错误")
	}
}

func TestMemoryToolConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name, tool, dsn string
		enabled         bool
		want            string
	}{
		{name: "unknown", tool: "memory__other", dsn: "postgres://test", enabled: true, want: "不是合法记忆工具名"},
		{name: "disabled", tool: "memory__summary", dsn: "postgres://test", want: "未启用记忆工具"},
		{name: "no_memory", tool: "memory__summary", enabled: true, want: "未启用记忆工具"},
		{name: "enabled", tool: "memory__summary", dsn: "postgres://test", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(bot.NewConfig(map[string]any{"llm_model": "m", "memory_database_dsn": tc.dsn, "memory_tools_enabled": tc.enabled, "personas": map[string]any{"default": map[string]any{"prompt": "p", "tools": []any{tc.tool}}}}))
			if tc.want != "" {
				want := "persona: 配置错误 personas.default.tools=" + tc.tool + ": " + tc.want
				if err == nil || err.Error() != want {
					t.Fatalf("%v want %s", err, want)
				}
			} else if err != nil || !cfg.memoryToolsEnabled {
				t.Fatalf("%+v %v", cfg, err)
			}
		})
	}
}
