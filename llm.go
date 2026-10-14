package persona

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// completionRequest 是一次补全请求；System/User 分别对应 system 与 user 消息。
type completionRequest struct {
	System      string
	User        contentParts  // user 消息的 content 数组（text + 可选 image_url）
	Followups   []chatMessage // 工具轮追加在 user 之后的 assistant/tool 消息（mcp.md §13.4）
	Tools       []toolDef     // 为空时请求体不带 tools 字段
	ToolChoice  string        // 为空时不发 tool_choice；轮数用尽时为 "none"
	Temperature float64
	MaxTokens   int
}

// completionResult 是一次补全的结果：最终文本或工具调用。
type completionResult struct {
	Content   string     // choices[0].message.content；为 null 或缺失时为空串
	ToolCalls []toolCall // choices[0].message.tool_calls
}

// chatMessage 是工具轮追加的一条消息。
type chatMessage struct {
	Role       string     `json:"role"`                   // "assistant" 或 "tool"
	Content    *string    `json:"content"`                // assistant 无文本时为 null
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`   // 仅 assistant
	ToolCallID string     `json:"tool_call_id,omitempty"` // 仅 tool
}

// toolCall 是模型发起的一次工具调用。
type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // 恒为 "function"
	Function toolCallFunction `json:"function"`
}

// toolCallFunction 是工具调用的函数名与参数。
type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串，原样交给工具解析
}

// toolDef 是 tools[] 中的一个工具定义。
type toolDef struct {
	Type     string       `json:"type"` // 恒为 "function"
	Function toolFunction `json:"function"`
}

// toolFunction 是工具定义的函数描述。
type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// contentPart 是 OpenAI content 数组中的一个块：text 或 image_url。
type contentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

// imageURLPart 是 image_url 块的内容。
type imageURLPart struct {
	URL string `json:"url"`
}

// contentParts 是 user 消息的 content（恒为数组）。
type contentParts []contentPart

// textParts 构造只含一个 text 块的 content，供内部与测试使用。
func textParts(s string) contentParts { return contentParts{{Type: "text", Text: s}} }

// completer 屏蔽具体 LLM 服务，便于单测注入桩。
type completer interface {
	Complete(ctx context.Context, req completionRequest) (completionResult, error)
}

type openaiClient struct { // 实现 completer
	baseURL string
	apiKey  string
	model   string
	headers map[string]string
	http    *http.Client
	log     *slog.Logger
	retries int
	timeout time.Duration
	debug   bool // 是否以 Debug 输出 LLM 请求与响应（debug_prompts）
}

// 确保 openaiClient 满足 completer。
var _ completer = (*openaiClient)(nil)

// newOpenAIClient 构造 OpenAI 兼容客户端。
func newOpenAIClient(cfg *config, hc *http.Client, log *slog.Logger) *openaiClient {
	return &openaiClient{
		baseURL: cfg.llmBaseURL,
		apiKey:  cfg.llmAPIKey,
		model:   cfg.llmModel,
		headers: cfg.llmExtraHeaders,
		http:    hc,
		log:     log,
		retries: cfg.llmMaxRetries,
		timeout: cfg.llmTimeout,
		debug:   cfg.debugPrompts,
	}
}

// Complete 调用 chat completions；仅对 429/5xx/网络错误重试。
func (c *openaiClient) Complete(ctx context.Context, req completionRequest) (completionResult, error) {
	attempts := c.retries + 1
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			backoff := 500 * time.Millisecond << (attempt - 1)
			select {
			case <-ctx.Done():
				return completionResult{}, ctx.Err()
			case <-time.After(backoff):
			}
		}
		res, retryable, err := c.attempt(ctx, req)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !retryable {
			return completionResult{}, err
		}
	}
	return completionResult{}, lastErr
}

// attempt 执行单次请求；返回是否可重试。
func (c *openaiClient) attempt(ctx context.Context, req completionRequest) (completionResult, bool, error) {
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	payload := c.payload(req, false)
	body, err := json.Marshal(payload)
	if err != nil {
		return completionResult{}, false, fmt.Errorf("persona: 请求编码失败: %w", err)
	}
	if c.debug {
		redacted, rerr := json.Marshal(c.payload(req, true))
		if rerr != nil {
			redacted = []byte("<日志序列化失败>")
		}
		c.log.Debug("persona llm 请求", "host", hostOf(c.baseURL), "model", c.model, "body", truncateRunes(string(redacted), 2048))
	}

	endpoint := trimRightSlash(c.baseURL) + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(actx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return completionResult{}, false, fmt.Errorf("persona: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// 网络错误可重试；ctx 取消直接失败。
		if ctx.Err() != nil {
			return completionResult{}, false, err
		}
		return completionResult{}, true, fmt.Errorf("persona: llm 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return completionResult{}, true, fmt.Errorf("persona: 读取响应失败: %w", err)
	}
	if c.debug {
		c.log.Debug("persona llm 响应", "host", hostOf(c.baseURL), "model", c.model, "status", resp.StatusCode, "body", truncateRunes(string(data), 2048))
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return completionResult{}, true, fmt.Errorf("persona: llm http %d: %s", resp.StatusCode, snippet(data))
	}
	if resp.StatusCode >= 400 {
		return completionResult{}, false, fmt.Errorf("persona: llm http %d: %s", resp.StatusCode, snippet(data))
	}
	res, err := parseCompletion(data)
	if err != nil {
		return completionResult{}, false, err
	}
	return res, false, nil
}

// payload 构造请求体；redact 为真时把 image_url 的 data URL 折成
// "data:<mime>;base64,<N bytes>"，仅用于 debug 日志（线上请求体从不折叠）。
// 工具轮消息按序追加在 user 之后；tools 与 tool_choice 只在非空时出现。
func (c *openaiClient) payload(req completionRequest, redact bool) map[string]any {
	user := req.User
	if redact {
		user = redactedParts(req.User)
	}
	messages := []any{
		map[string]any{"role": "system", "content": req.System},
		map[string]any{"role": "user", "content": user},
	}
	for _, m := range req.Followups {
		messages = append(messages, m)
	}
	body := map[string]any{
		"model":       c.model,
		"temperature": req.Temperature,
		"max_tokens":  req.MaxTokens,
		"messages":    messages,
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if req.ToolChoice != "" {
		body["tool_choice"] = req.ToolChoice
	}
	return body
}

// redactedParts 复制 content 块，把 data URL 折叠为占位符，其余块原样引用。
func redactedParts(parts contentParts) contentParts {
	out := make(contentParts, len(parts))
	for i, p := range parts {
		if p.ImageURL != nil && strings.HasPrefix(p.ImageURL.URL, "data:") {
			p.ImageURL = &imageURLPart{URL: shortenDataURL(p.ImageURL.URL)}
		}
		out[i] = p
	}
	return out
}

// shortenDataURL 把 "data:<mime>;base64,<b64>" 折成 "data:<mime>;base64,<N bytes>"，
// N 为解码后的字节数（DecodedLen 减 padding 个数）；非 data URL 原样返回。
func shortenDataURL(u string) string {
	const marker = ";base64,"
	i := strings.Index(u, marker)
	if !strings.HasPrefix(u, "data:") || i < 0 {
		return u
	}
	b64 := u[i+len(marker):]
	n := base64.StdEncoding.DecodedLen(len(b64)) - strings.Count(b64, "=")
	return fmt.Sprintf("%s;base64,%d bytes", u[:i], n)
}

// parseCompletion 解析 choices[0].message 的 content 与 tool_calls（llm.md §9.2）。
func parseCompletion(data []byte) (completionResult, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []toolCall      `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return completionResult{}, fmt.Errorf("persona: llm 响应解析失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return completionResult{}, fmt.Errorf("persona: llm 响应缺少 choices")
	}
	msg := resp.Choices[0].Message
	res := completionResult{ToolCalls: msg.ToolCalls}
	if len(msg.Content) == 0 || string(msg.Content) == "null" {
		if len(res.ToolCalls) == 0 {
			return completionResult{}, fmt.Errorf("persona: llm 响应缺少 content（可能只返回 reasoning_content）")
		}
		return res, nil
	}
	if err := json.Unmarshal(msg.Content, &res.Content); err != nil {
		return completionResult{}, fmt.Errorf("persona: llm 响应的 content 不是字符串")
	}
	return res, nil
}

// snippet 截断响应体片段用于日志。
func snippet(b []byte) string {
	const max = 256
	if len(b) > max {
		b = b[:max]
	}
	return string(b)
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// hostOf 返回 URL 的 host（不含路径与查询），解析失败时返回去空白的原值。
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}
