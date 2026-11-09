// Package llm 实现摘要流水线使用的 OpenAI 兼容 JSON 客户端。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"
)

// ErrInvalidOutput 表示结构不合规且修复请求仍失败。
var ErrInvalidOutput = errors.New("LLM 输出修复失败")

// Config 是后台 LLM 连接参数。
type Config struct {
	BaseURL, APIKey string
	Headers         map[string]string
	Timeout         time.Duration
	JSONMode        bool
}

// Client 只复用宿主 Transport，以请求 ctx 控制超时。
type Client struct {
	cfg       Config
	http      *http.Client
	retryWait time.Duration
	Log       *slog.Logger
}

// New 创建不带宿主硬超时的客户端，不进行网络操作。
func New(cfg Config, host *http.Client, log *slog.Logger) *Client {
	var transport http.RoundTripper
	if host != nil {
		transport = host.Transport
	}
	return &Client{cfg: cfg, http: &http.Client{Transport: transport}, retryWait: 2 * time.Second, Log: log}
}

// Request 是一次 JSON 对话请求。
type Request struct {
	Model, System, User string
	Temperature         float64
}

// Usage 是所有尝试的累计 token 用量。
type Usage struct{ PromptTokens, CompletionTokens int64 }

// Validator 检查模型输出是否包含必要结构。
type Validator interface{ Validate() error }

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type response struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// ChatJSON 校验 JSON 输出，结构错误修复一次；失败时也返回已知用量。
func (c *Client) ChatJSON(ctx context.Context, req Request, out Validator) (Usage, error) {
	messages := []message{{"system", req.System}, {"user", req.User}}
	var usage Usage
	for repair := 0; repair < 2; repair++ {
		raw, u, err := c.chat(ctx, req, messages)
		usage.PromptTokens += u.PromptTokens
		usage.CompletionTokens += u.CompletionTokens
		if err != nil {
			return usage, err
		}
		target := reflect.ValueOf(out)
		if target.Kind() != reflect.Pointer || target.IsNil() {
			return usage, fmt.Errorf("输出目标必须是非空指针")
		}
		target.Elem().SetZero() // 修复响应不能继承上次部分解码的字段。
		err = json.Unmarshal([]byte(raw), out)
		if err == nil {
			err = out.Validate()
		}
		if err == nil {
			return usage, nil
		}
		c.Log.Info("persona memory: LLM 输出不合规", "operation", "memory_llm", "err", err)
		if repair == 1 {
			return usage, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
		}
		messages = append(messages, message{"assistant", raw}, message{"user", "上次输出不合规：" + err.Error() + "。请只输出符合要求的 JSON。"})
	}
	return usage, nil
}

func (c *Client) chat(ctx context.Context, req Request, messages []message) (string, Usage, error) {
	body := map[string]any{"model": req.Model, "messages": messages, "temperature": req.Temperature}
	if c.cfg.JSONMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", Usage{}, err
	}
	var total Usage
	for attempt := 0; attempt < 2; attempt++ {
		content, usage, retry, err := c.once(ctx, data)
		total.PromptTokens += usage.PromptTokens
		total.CompletionTokens += usage.CompletionTokens
		if err == nil {
			return content, total, nil
		}
		if !retry || attempt == 1 || ctx.Err() != nil {
			return "", total, err
		}
		timer := time.NewTimer(c.retryWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", total, ctx.Err()
		case <-timer.C:
		}
	}
	return "", total, err
}

func (c *Client) once(parent context.Context, data []byte) (string, Usage, bool, error) {
	ctx, cancel := context.WithTimeout(parent, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", Usage{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	for key, value := range c.cfg.Headers {
		req.Header.Set(key, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.Log.Warn("persona memory: LLM 请求失败", "operation", "memory_llm", "err", err)
		return "", Usage{}, true, err
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded response
	// HTTP 错误只记录状态码，不输出服务端正文或凭据。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retry := resp.StatusCode == 429 || resp.StatusCode >= 500
		if retry {
			c.Log.Warn("persona memory: LLM 服务异常", "operation", "memory_llm", "status", resp.StatusCode)
		}
		return "", Usage{}, retry, fmt.Errorf("LLM HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&decoded)
	usage := Usage{decoded.Usage.PromptTokens, decoded.Usage.CompletionTokens}
	if err != nil {
		return "", usage, true, fmt.Errorf("LLM 响应解码: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return "", usage, false, fmt.Errorf("LLM 没有返回 choices")
	}
	return decoded.Choices[0].Message.Content, usage, false, nil
}
