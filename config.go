package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

const (
	personaDefaultName = "default"
	defaultLLMBaseURL  = "https://api.openai.com/v1"
	defaultSkipToken   = "[SKIP]"
)

// quietHoursPattern 匹配 HH:MM-HH:MM。
var quietHoursPattern = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d-([01]\d|2[0-3]):[0-5]\d$`)

// personaConfig 是一个人格预设。
type personaConfig struct {
	Prompt      string
	DisplayName string
	Temperature float64
	MaxTokens   int
	SkipToken   string

	hasTemperature bool
	hasMaxTokens   bool
}

// bindingConfig 是一条每会话人格绑定。
type bindingConfig struct {
	Platform  string
	BotID     string
	ChannelID string
	Persona   string
}

// config 是 agent 的全部配置（键名与默认值见 docs/configuration.md §10.1）。
type config struct {
	personas          map[string]personaConfig
	defaultPersona    string
	personaTemplate   string
	bindings          []bindingConfig
	selfIDs           []string
	triggerKeywords   []string
	triggerMinChars   int
	ignoreBots        bool
	respondToCommands bool

	mentionReplyProbability float64
	mentionMinInterval      time.Duration

	randomEnabled         bool
	randomProbability     float64
	randomCooldown        time.Duration
	randomMaxPerHour      int
	randomMinParticipants int
	randomActivityWindow  time.Duration
	randomQuietHours      string
	randomTimezone        *time.Location

	batchWindow    time.Duration
	batchMaxWindow time.Duration

	contextMaxMessages int
	contextMaxChannels int

	llmBaseURL         string
	llmAPIKey          string
	llmModel           string
	llmTemperature     float64
	llmMaxTokens       int
	llmTimeout         time.Duration
	llmMaxRetries      int
	llmSkipToken       string
	llmHistoryMaxChars int
	llmExtraHeaders    map[string]string

	replyMaxChars      int
	replyMentionSender bool
	replyDedupe        bool

	limitsMaxConcurrent int
	debugPrompts        bool
}

// loadConfig 读取全部键、填默认值并校验。
//
// 校验失败返回形如 "agent: 配置错误 <key>=<值>: <原因>" 的错误。
func loadConfig(c *bot.Config) (*config, error) {
	r := cfgReader{c: c}
	cfg := &config{}

	// LLM 全局键先读：人格预设的默认值需要回落到它们。
	cfg.llmBaseURL = r.str("llm_base_url", defaultLLMBaseURL)
	cfg.llmAPIKey = r.str("llm_api_key", "")
	cfg.llmModel = r.str("llm_model", "")
	cfg.llmSkipToken = r.str("llm_skip_token", defaultSkipToken)
	cfg.llmTimeout = r.dur("llm_timeout", 20*time.Second)
	cfg.llmExtraHeaders = readHeaders(c)

	var err error
	if cfg.llmTemperature, err = r.floatKey("llm_temperature", 0.8, "必须 >= 0"); err != nil {
		return nil, err
	}
	if cfg.llmMaxTokens, err = r.intKey("llm_max_tokens", 200, "必须 >= 1"); err != nil {
		return nil, err
	}
	if cfg.llmMaxRetries, err = r.intKey("llm_max_retries", 1, "必须 >= 0"); err != nil {
		return nil, err
	}
	if cfg.llmHistoryMaxChars, err = r.intKey("llm_history_max_chars", 4000, "必须 >= 1"); err != nil {
		return nil, err
	}

	if cfg.personas, err = readPersonas(c, cfg); err != nil {
		return nil, err
	}
	cfg.defaultPersona = r.str("default_persona", personaDefaultName)
	cfg.personaTemplate = r.str("persona_template", "")
	if cfg.bindings, err = readBindings(c); err != nil {
		return nil, err
	}

	cfg.selfIDs = c.Strings("self_ids")
	cfg.triggerKeywords = c.Strings("trigger_keywords")
	cfg.ignoreBots = r.boolean("ignore_bots", true)
	cfg.respondToCommands = r.boolean("respond_to_commands", false)
	cfg.mentionMinInterval = r.dur("mention_min_interval", 10*time.Second)
	cfg.randomEnabled = r.boolean("random_enabled", true)
	cfg.randomCooldown = r.dur("random_cooldown", 90*time.Second)
	cfg.randomActivityWindow = r.dur("random_activity_window", 5*time.Minute)
	cfg.randomQuietHours = r.str("random_quiet_hours", "")
	cfg.batchWindow = r.dur("batch_window", 2500*time.Millisecond)
	cfg.batchMaxWindow = r.dur("batch_max_window", 8*time.Second)
	cfg.replyMentionSender = r.boolean("reply_mention_sender", false)
	cfg.replyDedupe = r.boolean("reply_dedupe", true)
	cfg.debugPrompts = r.boolean("debug_prompts", false)

	if cfg.triggerMinChars, err = r.intKey("trigger_min_chars", 2, "必须 >= 0"); err != nil {
		return nil, err
	}
	if cfg.mentionReplyProbability, err = r.floatKey("mention_reply_probability", 1.0, "必须是 0..1 之间的小数"); err != nil {
		return nil, err
	}
	if cfg.randomProbability, err = r.floatKey("random_probability", 0.12, "必须是 0..1 之间的小数"); err != nil {
		return nil, err
	}
	if cfg.randomMaxPerHour, err = r.intKey("random_max_per_hour", 6, "必须 >= 0"); err != nil {
		return nil, err
	}
	if cfg.randomMinParticipants, err = r.intKey("random_min_participants", 2, "必须 >= 1"); err != nil {
		return nil, err
	}
	if cfg.contextMaxMessages, err = r.intKey("context_max_messages", 20, "必须 >= 1"); err != nil {
		return nil, err
	}
	if cfg.contextMaxChannels, err = r.intKey("context_max_channels", 512, "必须 >= 1"); err != nil {
		return nil, err
	}
	if cfg.replyMaxChars, err = r.intKey("reply_max_chars", 200, "必须 >= 1"); err != nil {
		return nil, err
	}
	if cfg.limitsMaxConcurrent, err = r.intKey("limits_max_concurrent", 2, "必须 >= 1"); err != nil {
		return nil, err
	}

	// ---- 校验 ----
	if cfg.llmBaseURL == "" {
		return nil, cfgErr("llm_base_url", "", "不能为空")
	}
	if cfg.llmAPIKey == "" {
		return nil, cfgErr("llm_api_key", "", "不能为空")
	}
	if cfg.llmModel == "" {
		return nil, cfgErr("llm_model", "", "不能为空")
	}
	if cfg.llmSkipToken == "" {
		return nil, cfgErr("llm_skip_token", "", "不能为空")
	}
	if _, ok := cfg.personas[cfg.defaultPersona]; !ok {
		return nil, cfgErr("default_persona", cfg.defaultPersona, "未在 personas 中定义")
	}
	for _, b := range cfg.bindings {
		if _, ok := cfg.personas[b.Persona]; !ok {
			return nil, cfgErr("bindings.persona", b.Persona, "未在 personas 中定义")
		}
	}
	if err := needMinInt("trigger_min_chars", cfg.triggerMinChars, 0); err != nil {
		return nil, err
	}
	if err := needUnitFloat("mention_reply_probability", cfg.mentionReplyProbability); err != nil {
		return nil, err
	}
	if err := needUnitFloat("random_probability", cfg.randomProbability); err != nil {
		return nil, err
	}
	if err := needMinInt("llm_max_tokens", cfg.llmMaxTokens, 1); err != nil {
		return nil, err
	}
	if err := needMinInt("llm_history_max_chars", cfg.llmHistoryMaxChars, 1); err != nil {
		return nil, err
	}
	if err := needMinInt("reply_max_chars", cfg.replyMaxChars, 1); err != nil {
		return nil, err
	}
	if err := needMinInt("context_max_messages", cfg.contextMaxMessages, 1); err != nil {
		return nil, err
	}
	if err := needMinInt("context_max_channels", cfg.contextMaxChannels, 1); err != nil {
		return nil, err
	}
	if err := needMinInt("limits_max_concurrent", cfg.limitsMaxConcurrent, 1); err != nil {
		return nil, err
	}
	if err := needMinInt("random_max_per_hour", cfg.randomMaxPerHour, 0); err != nil {
		return nil, err
	}
	if err := needMinInt("random_min_participants", cfg.randomMinParticipants, 1); err != nil {
		return nil, err
	}
	if err := needMinInt("llm_max_retries", cfg.llmMaxRetries, 0); err != nil {
		return nil, err
	}
	if err := needMinFloat("llm_temperature", cfg.llmTemperature, 0); err != nil {
		return nil, err
	}
	for _, d := range []struct {
		key string
		val time.Duration
	}{
		{"mention_min_interval", cfg.mentionMinInterval},
		{"random_cooldown", cfg.randomCooldown},
		{"random_activity_window", cfg.randomActivityWindow},
		{"batch_window", cfg.batchWindow},
		{"batch_max_window", cfg.batchMaxWindow},
		{"llm_timeout", cfg.llmTimeout},
	} {
		if err := needMinDur(d.key, d.val); err != nil {
			return nil, err
		}
	}
	if cfg.randomQuietHours != "" && !quietHoursPattern.MatchString(cfg.randomQuietHours) {
		return nil, cfgErr("random_quiet_hours", cfg.randomQuietHours, "必须是 HH:MM-HH:MM 格式")
	}

	loc, err := time.LoadLocation(r.str("random_timezone", "Local"))
	if err != nil {
		return nil, cfgErr("random_timezone", r.str("random_timezone", "Local"), "不是合法时区")
	}
	cfg.randomTimezone = loc
	return cfg, nil
}

// readPersonas 解析 personas，并回落各字段默认值。
func readPersonas(c *bot.Config, cfg *config) (map[string]personaConfig, error) {
	raw, ok := c.Get("personas")
	if !ok {
		return nil, cfgErr("personas", "", "必须是非空对象")
	}
	m, ok := asStringMap(raw)
	if !ok || len(m) == 0 {
		return nil, cfgErr("personas", "", "必须是非空对象")
	}
	out := make(map[string]personaConfig, len(m))
	for name, v := range m {
		p := personaConfig{DisplayName: name}
		switch t := v.(type) {
		case string:
			p.Prompt = t
		case map[string]any:
			if s, ok := t["prompt"].(string); ok {
				p.Prompt = s
			}
			if s, ok := t["display_name"].(string); ok && s != "" {
				p.DisplayName = s
			}
			if rawTemp, ok := t["temperature"]; ok {
				f, ok := toFloat(rawTemp)
				if !ok {
					return nil, cfgErr("personas."+name+".temperature", fmt.Sprintf("%v", rawTemp), "必须 >= 0")
				}
				p.Temperature, p.hasTemperature = f, true
			}
			if rawTok, ok := t["max_tokens"]; ok {
				n, ok := toInt(rawTok)
				if !ok {
					return nil, cfgErr("personas."+name+".max_tokens", fmt.Sprintf("%v", rawTok), "必须 >= 1")
				}
				p.MaxTokens, p.hasMaxTokens = n, true
			}
			if s, ok := t["skip_token"].(string); ok && s != "" {
				p.SkipToken = s
			}
		default:
			return nil, cfgErr("personas", "", "必须是非空对象")
		}
		if p.Prompt == "" {
			return nil, cfgErr("personas."+name+".prompt", "", "prompt 不能为空")
		}
		if !p.hasTemperature {
			p.Temperature = cfg.llmTemperature
		}
		if !p.hasMaxTokens {
			p.MaxTokens = cfg.llmMaxTokens
		}
		if p.SkipToken == "" {
			p.SkipToken = cfg.llmSkipToken
		}
		if p.DisplayName == "" {
			p.DisplayName = name
		}
		out[name] = p
	}
	return out, nil
}

// readBindings 解析 bindings；未配置时返回 nil。
func readBindings(c *bot.Config) ([]bindingConfig, error) {
	raw, ok := c.Get("bindings")
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, cfgErr("bindings", fmt.Sprintf("%v", raw), "必须是非空对象")
	}
	out := make([]bindingConfig, 0, len(list))
	for _, item := range list {
		m, ok := asStringMap(item)
		if !ok {
			return nil, cfgErr("bindings", fmt.Sprintf("%v", item), "必须是非空对象")
		}
		b := bindingConfig{
			Platform:  strOf(m["platform"]),
			BotID:     strOf(m["bot_id"]),
			ChannelID: strOf(m["channel_id"]),
			Persona:   strOf(m["persona"]),
		}
		if b.ChannelID == "" {
			return nil, cfgErr("bindings.channel_id", "", "channel_id 不能为空")
		}
		out = append(out, b)
	}
	return out, nil
}

// readHeaders 解析 llm_extra_headers。
func readHeaders(c *bot.Config) map[string]string {
	out := map[string]string{}
	raw, ok := c.Get("llm_extra_headers")
	if !ok {
		return out
	}
	m, ok := asStringMap(raw)
	if !ok {
		return out
	}
	for k, v := range m {
		out[k] = strOf(v)
	}
	return out
}

// matchBinding 按「非空字段最多、并列取靠前」返回命中的人格名。
func (c *config) matchBinding(platform, botID, channelID string) (string, bool) {
	best, bestScore := -1, 0
	for i, b := range c.bindings {
		if b.ChannelID != channelID {
			continue
		}
		if b.Platform != "" && b.Platform != platform {
			continue
		}
		if b.BotID != "" && b.BotID != botID {
			continue
		}
		score := 0
		if b.Platform != "" {
			score++
		}
		if b.BotID != "" {
			score++
		}
		if b.ChannelID != "" {
			score++
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return "", false
	}
	return c.bindings[best].Persona, true
}

// cfgReader 封装带类型解析的配置读取。
type cfgReader struct{ c *bot.Config }

func (r cfgReader) str(key, def string) string        { return r.c.String(key, def) }
func (r cfgReader) boolean(key string, def bool) bool { return r.c.Bool(key, def) }
func (r cfgReader) dur(key string, def time.Duration) time.Duration {
	return r.c.Duration(key, def)
}

func (r cfgReader) intKey(key string, def int, reason string) (int, error) {
	v, ok := r.c.Get(key)
	if !ok {
		return def, nil
	}
	n, ok := toInt(v)
	if !ok {
		return 0, cfgErr(key, fmt.Sprintf("%v", v), reason)
	}
	return n, nil
}

func (r cfgReader) floatKey(key string, def float64, reason string) (float64, error) {
	v, ok := r.c.Get(key)
	if !ok {
		return def, nil
	}
	f, ok := toFloat(v)
	if !ok {
		return 0, cfgErr(key, fmt.Sprintf("%v", v), reason)
	}
	return f, nil
}

// toInt 把配置原始值转成 int。
func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		n, err := t.Int64()
		return int(n), err == nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	}
	return 0, false
}

// toFloat 把配置原始值转成 float64。
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// asStringMap 把 map[string]any / map[any]any 归一为 map[string]any。
func asStringMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[strOf(k)] = val
		}
		return out, true
	}
	return nil, false
}

// strOf 把任意值转成字符串。
func strOf(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// cfgErr 构造统一格式的配置错误。
func cfgErr(key, value, reason string) error {
	return fmt.Errorf("agent: 配置错误 %s=%s: %s", key, value, reason)
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func needMinInt(key string, v, min int) error {
	if v < min {
		return cfgErr(key, strconv.Itoa(v), fmt.Sprintf("必须 >= %d", min))
	}
	return nil
}

func needMinFloat(key string, v, min float64) error {
	if v < min {
		return cfgErr(key, formatFloat(v), fmt.Sprintf("必须 >= %s", formatFloat(min)))
	}
	return nil
}

func needMinDur(key string, v time.Duration) error {
	if v < 0 {
		return cfgErr(key, v.String(), "必须 >= 0")
	}
	return nil
}

func needUnitFloat(key string, v float64) error {
	if v < 0 || v > 1 {
		return cfgErr(key, formatFloat(v), "必须是 0..1 之间的小数")
	}
	return nil
}
