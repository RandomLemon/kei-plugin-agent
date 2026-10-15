package persona

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	mcpVersion     = "2025-11-25"
	mcpPrefix      = "mcp__"
	mcpMaxResponse = 1 << 20
)

// mcpBudget 是一次 MCP 操作（初始化、一次完整目录遍历或一次工具调用）内全部响应共享的
// 读取预算：分页与服务端请求的回复都要计入，否则内存会随分页次数无上限增长
// （mcp.md §14.4）。操作内部串行使用，无需加锁。
type mcpBudget struct{ left int64 }

// newMCPBudget 返回一次操作的读取预算。
func newMCPBudget() *mcpBudget { return &mcpBudget{left: mcpMaxResponse} }

// next 返回本次响应允许读取的字节数；预算用尽时返回 0。
func (b *mcpBudget) next() int64 {
	if b.left <= 0 {
		return 0
	}
	return min(b.left, int64(mcpMaxResponse))
}

// spend 记账本次响应实际读取的字节数（含 SSE 帧与未消费的剩余字节）。
func (b *mcpBudget) spend(n int64) { b.left -= n }

// mcpClient 只有初始化串行；普通请求可以并发，网络操作不持有状态锁。
type mcpClient struct {
	url       string
	headers   map[string]string
	http      *http.Client
	seq       atomic.Uint64
	initGate  chan struct{}
	mu        sync.Mutex
	sessionID string
	ready     bool
}

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// 错误只携带本地原因码，避免远端错误正文、URL、session 或请求头泄露。
type mcpError struct{ detail string }

func (e *mcpError) Error() string { return "MCP 请求失败：" + e.detail }

type mcpToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Execution   struct {
		TaskSupport string `json:"taskSupport"`
	} `json:"execution"`
}

type mcpCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func newMCPClient(cfg *config, hc *http.Client) *mcpClient {
	return &mcpClient{url: cfg.mcpURL, headers: cfg.mcpExtraHeaders, http: hc, initGate: make(chan struct{}, 1)}
}

func (c *mcpClient) session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *mcpClient) initialize(ctx context.Context, budget *mcpBudget) error {
	select {
	case c.initGate <- struct{}{}:
		defer func() { <-c.initGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	if ready {
		return nil
	}
	params, err := json.Marshal(map[string]any{
		"protocolVersion": mcpVersion,
		"capabilities":    struct{}{},
		"clientInfo":      map[string]string{"name": "kei-plugin-persona", "version": "v0.1.0"},
	})
	if err != nil {
		return err
	}
	result, session, err := c.post(ctx, budget, mcpMessage{JSONRPC: "2.0", ID: c.nextID(), Method: "initialize", Params: params}, "")
	// 即使初始化内容校验失败，也保留已建立的 session，供启动失败时清理。
	c.mu.Lock()
	c.sessionID = session
	c.mu.Unlock()
	if err != nil {
		return err
	}
	var info struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(result, &info) != nil || info.ProtocolVersion != mcpVersion || info.Capabilities.Tools == nil {
		return &mcpError{detail: "bad_initialize"}
	}
	if _, _, err := c.post(ctx, budget, mcpMessage{JSONRPC: "2.0", Method: "notifications/initialized"}, session); err != nil {
		return err
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	return nil
}

func (c *mcpClient) nextID() json.RawMessage {
	return json.RawMessage(strconv.FormatUint(c.seq.Add(1), 10))
}

func (c *mcpClient) request(ctx context.Context, budget *mcpBudget, method string, params any, out any) error {
	if err := c.initialize(ctx, budget); err != nil {
		return err
	}
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	session := c.session()
	result, _, err := c.post(ctx, budget, mcpMessage{JSONRPC: "2.0", ID: c.nextID(), Method: method, Params: data}, session)
	var failure *mcpError
	if errors.As(err, &failure) && failure.detail == "http_404" && session != "" {
		c.mu.Lock()
		if c.sessionID == session {
			c.sessionID, c.ready = "", false
		}
		c.mu.Unlock()
		// 恢复会话供后续操作使用；原请求始终返回失败，绝不重放。
		if initErr := c.initialize(ctx, budget); initErr != nil {
			return initErr
		}
	}
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(bytes.TrimSpace(result), []byte("{")) || json.Unmarshal(result, out) != nil {
		return &mcpError{detail: "bad_result"}
	}
	return nil
}

func (c *mcpClient) newRequest(ctx context.Context, method string, body io.Reader, session string) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.url, body)
	if err != nil {
		return nil, &mcpError{detail: "bad_url"}
	}
	for name, value := range c.headers {
		r.Header.Set(name, value)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", mcpVersion)
	if session != "" {
		r.Header.Set("MCP-Session-Id", session)
	}
	return r, nil
}

func (c *mcpClient) do(r *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(r)
	if err != nil {
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		return nil, &mcpError{detail: "network"}
	}
	return resp, nil
}

func (c *mcpClient) post(ctx context.Context, budget *mcpBudget, msg mcpMessage, session string) (json.RawMessage, string, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, "", err
	}
	r, err := c.newRequest(ctx, http.MethodPost, bytes.NewReader(data), session)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if msg.ID == nil || msg.Method == "" { // 通知、服务端请求的回复。
		if resp.StatusCode == http.StatusAccepted {
			return nil, "", nil
		}
		return nil, "", &mcpError{detail: "http_" + strconv.Itoa(resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", &mcpError{detail: "http_" + strconv.Itoa(resp.StatusCode)}
	}
	responseSession := resp.Header.Get("MCP-Session-Id")
	if responseSession != "" {
		session = responseSession
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, responseSession, &mcpError{detail: "bad_content_type"}
	}
	limit := budget.next()
	if limit <= 0 {
		return nil, responseSession, &mcpError{detail: "response_too_large"}
	}
	reader := &io.LimitedReader{R: resp.Body, N: limit + 1}
	defer func() { budget.spend(limit + 1 - reader.N) }()
	var result json.RawMessage
	switch media {
	case "application/json":
		body, readErr := io.ReadAll(reader)
		if readErr != nil {
			return nil, responseSession, &mcpError{detail: "read_response"}
		}
		if int64(len(body)) > limit {
			return nil, responseSession, &mcpError{detail: "response_too_large"}
		}
		var done bool
		result, done, err = c.receive(ctx, budget, body, msg.ID, session)
		if err == nil && !done {
			err = &mcpError{detail: "missing_response"}
		}
	case "text/event-stream":
		result, err = c.readSSE(ctx, budget, reader, msg.ID, session)
	default:
		err = &mcpError{detail: "bad_content_type"}
	}
	return result, responseSession, err
}

func (c *mcpClient) readSSE(ctx context.Context, budget *mcpBudget, reader *io.LimitedReader, id json.RawMessage, session string) (json.RawMessage, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), mcpMaxResponse+1)
	var data []string
	for scanner.Scan() {
		if reader.N == 0 {
			return nil, &mcpError{detail: "response_too_large"}
		}
		line := scanner.Text()
		if line == "" {
			if len(data) == 0 {
				continue
			}
			result, done, err := c.receive(ctx, budget, []byte(strings.Join(data, "\n")), id, session)
			data = nil
			if err != nil || done {
				return result, err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if reader.N == 0 {
		return nil, &mcpError{detail: "response_too_large"}
	}
	return nil, &mcpError{detail: "incomplete_stream"}
}

func (c *mcpClient) receive(ctx context.Context, budget *mcpBudget, data []byte, id json.RawMessage, session string) (json.RawMessage, bool, error) {
	var msg mcpMessage
	if json.Unmarshal(data, &msg) != nil || msg.JSONRPC != "2.0" {
		return nil, false, &mcpError{detail: "bad_response"}
	}
	if msg.Method != "" {
		if msg.ID == nil {
			return nil, false, nil
		} // 不订阅工具变更通知。
		reply := mcpMessage{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{}`)}
		if msg.Method != "ping" {
			reply.Result = nil
			reply.Error = &mcpRPCError{Code: -32601, Message: "Method not found"}
		}
		_, _, err := c.post(ctx, budget, reply, session)
		return nil, false, err
	}
	if !bytes.Equal(bytes.TrimSpace(msg.ID), id) || (msg.Result == nil) == (msg.Error == nil) {
		return nil, false, &mcpError{detail: "bad_response"}
	}
	if msg.Error != nil {
		return nil, true, &mcpError{detail: "rpc_" + strconv.Itoa(msg.Error.Code)}
	}
	return msg.Result, true, nil
}

func (c *mcpClient) listTools(ctx context.Context) ([]mcpToolDef, error) {
	budget := newMCPBudget()
	var tools []mcpToolDef
	cursor := ""
	for {
		var page struct {
			Tools      []mcpToolDef `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		params := map[string]string{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.request(ctx, budget, "tools/list", params, &page); err != nil {
			return nil, err
		}
		if page.Tools == nil {
			return nil, &mcpError{detail: "bad_tool_list"}
		}
		tools = append(tools, page.Tools...)
		if page.NextCursor == "" {
			return tools, nil
		}
		cursor = page.NextCursor
	}
}

func (c *mcpClient) callTool(ctx context.Context, name string, args json.RawMessage) (mcpCallResult, error) {
	budget := newMCPBudget()
	var result mcpCallResult
	err := c.request(ctx, budget, "tools/call", map[string]any{"name": name, "arguments": args}, &result)
	return result, err
}

func (c *mcpClient) close(ctx context.Context) error {
	select {
	case c.initGate <- struct{}{}:
		defer func() { <-c.initGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	session := c.session()
	if session == "" {
		return nil
	}
	r, err := c.newRequest(ctx, http.MethodDelete, nil, session)
	if err != nil {
		return err
	}
	resp, err := c.do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && resp.StatusCode != http.StatusMethodNotAllowed {
		return &mcpError{detail: "http_" + strconv.Itoa(resp.StatusCode)}
	}
	c.mu.Lock()
	c.sessionID, c.ready = "", false
	c.mu.Unlock()
	return nil
}

type mcpTool struct {
	definition toolDef
	remoteName string
	client     *mcpClient
}

func (t *mcpTool) Def() toolDef { return t.definition }

func validMCPToolName(name string) bool {
	if !strings.HasPrefix(name, mcpPrefix) || len(name) <= len(mcpPrefix) || len(name) > 64 {
		return false
	}
	for _, ch := range []byte(name) {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func buildMCPTools(c *mcpClient, defs []mcpToolDef) ([]tool, error) {
	tools := make([]tool, 0, len(defs))
	names := make(map[string]bool, len(defs))
	for _, def := range defs {
		name := mcpPrefix + def.Name
		if !validMCPToolName(name) || names[name] {
			return nil, &mcpError{detail: "bad_tool_name"}
		}
		names[name] = true
		var schema struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(def.InputSchema, &schema) != nil || schema.Type != "object" {
			return nil, &mcpError{detail: "bad_tool_schema"}
		}
		if def.Execution.TaskSupport == "required" {
			return nil, &mcpError{detail: "tasks_required"}
		}
		tools = append(tools, &mcpTool{client: c, remoteName: def.Name, definition: toolDef{
			Type: "function", Function: toolFunction{Name: name, Description: def.Description, Parameters: def.InputSchema},
		}})
	}
	slices.SortFunc(tools, func(a, b tool) int { return strings.Compare(a.Def().Function.Name, b.Def().Function.Name) })
	return tools, nil
}

func (t *mcpTool) Call(ctx context.Context, args string) toolResult {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) != nil || obj == nil {
		return toolResult{Content: "[工具调用失败] 参数必须是 JSON 对象。", Status: "error", Detail: "bad_args"}
	}
	result, err := t.client.callTool(ctx, t.remoteName, json.RawMessage(args))
	if err != nil {
		detail := "network"
		var failure *mcpError
		if errors.As(err, &failure) {
			detail = failure.detail
		}
		return toolResult{Content: "[工具调用失败] 外部工具暂时不可用。", Status: "error", Detail: detail}
	}
	out := toolResult{Status: "ok"}
	var parts []string
	hasText := false
	for _, block := range result.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
			hasText = true
		} else {
			parts = append(parts, "[未展示的 "+block.Type+" 内容]")
		}
	}
	switch {
	case hasText:
		out.Content = strings.Join(parts, "\n")
	case len(result.StructuredContent) != 0 && string(result.StructuredContent) != "null":
		var compact bytes.Buffer
		if err := json.Compact(&compact, result.StructuredContent); err != nil {
			return toolResult{Content: "[工具调用失败] 返回内容无效。", Status: "error", Detail: "bad_result"}
		}
		out.Content = compact.String()
	case len(result.Content) != 0:
		out = toolResult{Content: "[工具调用失败] 外部工具返回了不支持的内容类型。", Status: "error", Detail: "unsupported_content"}
	default:
		out.Content = "工具执行成功，无返回内容"
	}
	if result.IsError {
		out.Status, out.Detail = "error", "tool_error"
		if len(result.Content) == 0 && (len(result.StructuredContent) == 0 || string(result.StructuredContent) == "null") {
			out.Content = "工具执行失败，无错误信息"
		}
	}
	return out
}

func (p *Plugin) loadMCPTools(ctx context.Context) ([]tool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.mcpTimeout)
	defer cancel()
	defs, err := p.mcp.listTools(ctx)
	if err != nil {
		return nil, err
	}
	return buildMCPTools(p.mcp, defs)
}
