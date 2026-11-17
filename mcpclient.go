package main

// 纯 Go 的 MCP 客户端（协议层，与任何插件体系无关）：stdio（spawn
// command+args，行分隔 JSON-RPC 2.0）与 streamable HTTP（POST JSON-RPC，
// 响应为 JSON 或 SSE 流）两种传输。两条业务线共用本实现：
//   - 独立 MCP 连接器（mcpservers.go，宿主 mcpPlainHost：仅 ${环境变量} 展开）
//   - Claude Code 插件桥（ccplugin_mcp.go，宿主 ccPluginHost：插件变量展开）
// start 幂等：连接按需建立——独立连接器懒启动（模型 mcp_read_connector、
// 调用自愈、用户显式测试/启用都从这里拉起进程）、插件侧启动/重载/调用时
// 连接，殊途同归；调用失败自动重启一次再试。stdio 子进程 spawn 统一过
// hideConsoleWindow（Windows 桌面形态不弹控制台黑窗，纪律见
// consolehide_windows.go）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// mcpServerDef 是一个 MCP 服务器的定义（stdio 型或 http/sse 型）。
type mcpServerDef struct {
	Type    string            `json:"type"` // ""（stdio）| http | sse
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// mcpToolDef 是服务器上报的一个工具。
type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// mcpHost 是 mcpClient 的宿主：变量替换与子进程工作目录的来源。
// CC 插件路径用 ccPluginHost（${CLAUDE_PLUGIN_ROOT} 等替换 + 插件根做工作
// 目录）；独立 MCP 服务器（mcpservers.go）用 mcpPlainHost（仅 ${环境变量}）。
type mcpHost interface {
	ExpandVars(s string) string
	WorkDir() string
}

// mcpPlainHost 是独立服务器宿主：${VAR} 展开为环境变量，工作目录是
// mcp-servers 数据根（相对路径命令按此解析）。
type mcpPlainHost struct{ dir string }

func (h mcpPlainHost) ExpandVars(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return os.Expand(s, os.Getenv)
}

func (h mcpPlainHost) WorkDir() string { return h.dir }

// mcpClient 是一个已连接的 MCP 服务器（stdio 或 http）。
type mcpClient struct {
	fullName string // 插件路径 <插件名>_<服务器>；独立路径就是服务器名
	def      mcpServerDef
	host     mcpHost

	mu      sync.Mutex
	started bool
	cmd     *exec.Cmd
	stdinW  interface{ Write([]byte) (int, error) }
	stdinC  interface{ Close() error }
	stdout  *bufio.Reader
	httpSID string // streamable HTTP 的会话 id（Mcp-Session-Id）
	nextID  int64
	tools   []mcpToolDef

	// startMu 串行化握手：同一时刻至多一个 start 在进行，中断句柄归属确定。
	startMu sync.Mutex

	// lifeMu 保护 cancel/closed。shutdown 必须在**不持 c.mu** 的情况下就能打断
	// 进行中的握手（它整段持 c.mu），否则摘除一条正在握手的连接要等满读超时。
	lifeMu sync.Mutex
	cancel context.CancelFunc
	closed bool

	lines    chan string   // stdio 读行通道（见 readLoop）
	readDone chan struct{} // 关闭连接时 close：让读行 goroutine 退出
}

// jsonrpcRequest / jsonrpcResponse 是 JSON-RPC 2.0 的帧。
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`
	Params  any    `json:"params,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// start 建立连接并完成 initialize 握手 + tools/list（幂等；已启动直接返回）。
// 握手全程持 c.mu（协议字段只此一处写），所以 shutdown 不得直接等它：先把本次
// 握手的 cancel 登记出去，shutdown 取消后实际 I/O（stdio 读、HTTP 请求）立刻返回。
func (c *mcpClient) start(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.lifeMu.Lock()
	if c.closed { // 已摘除的连接不再拉起进程（摘除后要重新连接请新建实例）
		c.lifeMu.Unlock()
		return fmt.Errorf("MCP %s 连接已关闭", c.fullName)
	}
	c.cancel = cancel
	c.lifeMu.Unlock()
	defer func() {
		c.lifeMu.Lock()
		c.cancel = nil
		c.lifeMu.Unlock()
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return nil
	}
	if err := c.connect(ctx); err != nil {
		return err
	}
	if err := c.initialize(ctx); err != nil {
		c.shutdownLocked()
		return err
	}
	tools, err := c.listTools(ctx)
	if err != nil {
		c.shutdownLocked()
		return err
	}
	c.tools = tools
	c.started = true
	return nil
}

// connect 建立传输（spawn 进程或准备 HTTP 会话）。
func (c *mcpClient) connect(ctx context.Context) error {
	if c.def.Type == "http" || c.def.Type == "sse" {
		return nil // http 无预连接，逐请求进行
	}
	cmdName := c.host.ExpandVars(c.def.Command)
	args := make([]string, len(c.def.Args))
	for i, a := range c.def.Args {
		args[i] = c.host.ExpandVars(a)
	}
	cmd := exec.Command(cmdName, args...)
	env := os.Environ()
	for k, v := range c.def.Env {
		env = append(env, c.host.ExpandVars(k)+"="+c.host.ExpandVars(v))
	}
	cmd.Env = env
	cmd.Dir = c.host.WorkDir()
	hideConsoleWindow(cmd) // console 程序不弹窗（python/uv 等一律后台跑）
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = nil // 服务器自身日志走它自己的输出，不混入协议流
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 MCP 服务器 %s 失败: %w", c.fullName, err)
	}
	c.cmd = cmd
	c.stdinW = stdin
	c.stdinC = stdin
	c.stdout = bufio.NewReaderSize(stdout, 1<<20)
	c.lines = make(chan string, 16)
	c.readDone = make(chan struct{})
	go c.readLoop(c.stdout, c.lines, c.readDone)
	return nil
}

// readLoop 持续把 stdio 行送进 lines 通道，连接断开即 close(lines)。
// 读必须放在旁路 goroutine：readResponse 才能同时盯 ctx 与超时，shutdown 的
// 取消也就能立刻打断卡住的握手（原来直接阻塞在 ReadString 上，一卡就 120s）。
func (c *mcpClient) readLoop(stdout *bufio.Reader, lines chan string, done <-chan struct{}) {
	defer close(lines)
	for {
		line, err := stdout.ReadString('\n')
		if line != "" {
			select {
			case lines <- line:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// initialize 完成 MCP 握手（initialize → initialized 通知）。
func (c *mcpClient) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "jx-digital-assistant", "version": appVersion},
	}
	if _, err := c.rpc(ctx, "initialize", params); err != nil {
		return err
	}
	// initialized 通知（无 id、不期待响应）
	if c.def.Type == "http" || c.def.Type == "sse" {
		_, _ = c.httpPost(ctx, jsonrpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}, false)
		return nil
	}
	_, err := fmt.Fprintln(c.stdinW, rpcLine(jsonrpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}))
	return err
}

// listTools 拉取工具清单。
func (c *mcpClient) listTools(ctx context.Context) ([]mcpToolDef, error) {
	res, err := c.rpc(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var page struct {
		Tools []mcpToolDef `json:"tools"`
	}
	raw, _ := json.Marshal(res)
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("tools/list 结果解析失败: %w", err)
	}
	return page.Tools, nil
}

// mcpImage 是工具结果里携带的一张图片（MCP image content，base64 已解码）。
type mcpImage struct {
	Data     []byte
	MIMEType string
}

// callTool 调用一个工具并返回文本结果与随附图片（isError 语义转为错误文本
// 前缀）。图片与文本并列返回：图片字节拿到即唯一一份，由调用方入库注入
// （见 attachment.go 的 mcpImagesEnvelope）。
func (c *mcpClient) callTool(ctx context.Context, tool string, args json.RawMessage) (string, []mcpImage, error) {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		// 单次自愈：未启动或上次死亡时重连一次（独立连接器懒启动的落地点）
		if err := c.start(ctx); err != nil {
			return "", nil, err
		}
		c.mu.Lock()
	}
	c.mu.Unlock()
	params := map[string]any{"name": tool, "arguments": json.RawMessage(args)}
	if len(args) == 0 {
		params["arguments"] = map[string]any{}
	}
	res, err := c.rpc(ctx, "tools/call", params)
	if err != nil {
		// stdio 服务器可能已死：重启一次再试
		if c.def.Type != "http" && c.def.Type != "sse" {
			c.mu.Lock()
			c.shutdownLocked()
			c.mu.Unlock()
			if err2 := c.start(ctx); err2 == nil {
				if res2, err3 := c.rpc(ctx, "tools/call", params); err3 == nil {
					res, err = res2, nil
				}
			}
		}
		if err != nil {
			return "", nil, err
		}
	}
	return mcpResultContent(res)
}

// mcpResultContent 从 tools/call 结果提取文本（content 数组的 text 项拼接）
// 与图片（image content 的 base64 解码）。
func mcpResultContent(res any) (string, []mcpImage, error) {
	raw, _ := json.Marshal(res)
	var doc struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MIMEType string `json:"mimeType"`
		} `json:"content"`
		IsError bool   `json:"isError"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", nil, err
	}
	var b strings.Builder
	var images []mcpImage
	for _, item := range doc.Content {
		switch {
		case item.Type == "text" && item.Text != "":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(item.Text)
		case item.Type == "image" && item.Data != "":
			if data, err := base64.StdEncoding.DecodeString(item.Data); err == nil {
				mt := item.MIMEType
				if mt == "" {
					mt = "image/png"
				}
				images = append(images, mcpImage{Data: data, MIMEType: mt})
			}
		}
	}
	if b.Len() == 0 && doc.Text != "" {
		b.WriteString(doc.Text)
	}
	text := b.String()
	if doc.IsError {
		text = "（MCP 工具报告错误）" + text
	}
	if text == "" && len(images) == 0 {
		text = "（工具执行完成，无文本输出）"
	}
	return text, images, nil
}

// rpc 发一个 JSON-RPC 请求并等匹配 id 的响应。
func (c *mcpClient) rpc(ctx context.Context, method string, params any) (any, error) {
	if c.def.Type == "http" || c.def.Type == "sse" {
		c.nextID++
		return c.httpRPC(ctx, jsonrpcRequest{JSONRPC: "2.0", ID: c.nextID, Method: method, Params: params})
	}
	c.nextID++
	id := c.nextID
	line := rpcLine(jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if _, err := fmt.Fprintln(c.stdinW, line); err != nil {
		return nil, fmt.Errorf("MCP %s 写入失败: %w", c.fullName, err)
	}
	return c.readResponse(ctx, id)
}

// readResponse 读 stdio 行直到拿到匹配 id 的响应（跳过服务器的 notification）。
// 读行由 readLoop 旁路送出，本函数只等行、ctx 与总超时：握手被取消时能立即
// 返回，不再死等服务器的首帧。
func (c *mcpClient) readResponse(ctx context.Context, id int64) (any, error) {
	lines := c.lines // 取本地引用：shutdownLocked 会把字段清空，通道本身仍会关闭
	if lines == nil {
		return nil, fmt.Errorf("MCP %s 连接已关闭", c.fullName)
	}
	timeout := time.NewTimer(120 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("MCP %s 等待响应被取消: %w", c.fullName, ctx.Err())
		case <-timeout.C:
			return nil, fmt.Errorf("MCP %s 响应超时", c.fullName)
		case raw, ok := <-lines:
			if !ok {
				return nil, fmt.Errorf("MCP %s 连接断开", c.fullName)
			}
			line := strings.TrimSpace(raw)
			if line == "" {
				continue
			}
			var resp jsonrpcRequest
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				continue // 非 JSON 行（服务器日志混入等）：跳过
			}
			if resp.ID == 0 || resp.Method != "" {
				continue // notification / 服务器发起的请求：本客户端只做应答方以外的角色
			}
			if resp.ID != id {
				continue
			}
			if resp.Error != nil {
				return nil, fmt.Errorf("MCP %s 错误 %d: %s", c.fullName, resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}
	}
}

// httpRPC 走 streamable HTTP：POST 请求体，响应为 JSON 或 SSE（取匹配 id 的 data）。
func (c *mcpClient) httpRPC(ctx context.Context, req jsonrpcRequest) (any, error) {
	data, err := c.httpPost(ctx, req, true)
	if err != nil {
		return nil, err
	}
	var resp jsonrpcRequest
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("MCP %s HTTP 响应解析失败: %w", c.fullName, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP %s 错误 %d: %s", c.fullName, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// httpPost 发一个 JSON-RPC 帧，期待响应时返回响应体（处理 SSE 包装）。
func (c *mcpClient) httpPost(ctx context.Context, req jsonrpcRequest, wantResponse bool) ([]byte, error) {
	url := c.host.ExpandVars(c.def.URL)
	body, _ := json.Marshal(req)
	hctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	hreq, err := http.NewRequestWithContext(hctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.def.Headers {
		hreq.Header.Set(k, c.host.ExpandVars(v))
	}
	if c.httpSID != "" {
		hreq.Header.Set("Mcp-Session-Id", c.httpSID)
	}
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("MCP %s HTTP 请求失败: %w", c.fullName, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.httpSID = sid
	}
	if !wantResponse {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("MCP %s HTTP %d", c.fullName, resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSEFirstData(resp.Body)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return buf.Bytes(), nil
}

// readSSEFirstData 从 SSE 流里取第一个含 JSON-RPC 响应的 data 帧并读到流尾
// （streamable HTTP 的响应流可能带多个事件，取首个 id 匹配的由调用方校验）。
func readSSEFirstData(body interface{ Read([]byte) (int, error) }) ([]byte, error) {
	br := bufio.NewReaderSize(body, 1<<20)
	deadline := time.Now().Add(120 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(t, "data:"))
				if payload != "" && payload != "[DONE]" {
					var probe jsonrpcRequest
					if err := json.Unmarshal([]byte(payload), &probe); err == nil && probe.ID != 0 {
						return []byte(payload), nil
					}
					last = []byte(payload) // 兜底：最后一个能解析的 data
				}
			}
		}
		if err != nil {
			break // 流结束
		}
	}
	if last != nil {
		return last, nil
	}
	return nil, fmt.Errorf("SSE 流里没有 JSON-RPC 响应")
}

// rpcLine 序列化一个 JSON-RPC 帧为单行。
func rpcLine(req jsonrpcRequest) string {
	data, _ := json.Marshal(req)
	return string(data)
}

// shutdownLocked 关闭连接（调用方持锁）。
func (c *mcpClient) shutdownLocked() {
	if c.stdinC != nil {
		_ = c.stdinC.Close()
		c.stdinC = nil
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
		c.cmd = nil
	}
	if c.readDone != nil {
		close(c.readDone) // 让 readLoop 退出（它随后 close(lines)，等行的一侧立即收到）
		c.readDone = nil
	}
	c.lines = nil
	c.stdout = nil
	c.stdinW = nil
	c.started = false
	c.tools = nil
}

// shutdown 对外关闭。先在不持 c.mu 的情况下标记关闭并取消握手（stdio 读与 HTTP
// 请求都会随 ctx 立即返回），再取 c.mu 清理进程与管道——否则一次卡住的握手会让
// 调用方（包括持注册表锁的摘除流程）跟着等满读超时。
func (c *mcpClient) shutdown() {
	c.lifeMu.Lock()
	c.closed = true
	cancel := c.cancel
	c.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shutdownLocked()
}
