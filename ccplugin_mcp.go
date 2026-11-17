package main

// Claude Code 插件组件桥：MCP servers（.mcp.json / manifest.mcpServers）。
// 协议客户端在 mcpclient.go（与独立 MCP 连接器共用同一实现，宿主注入不同：
// 本侧 ccPluginHost 做 ${CLAUDE_PLUGIN_ROOT} 等插件变量展开）。插件侧语义：
// 每个服务器的工具以 mcp__<插件名>_<服务器>__<工具> 注册进工具目录（对齐
// Claude 的插件 MCP 工具命名，插件 hooks 的 matcher 可直接匹配）。连接随
// 插件生命周期建立（引擎启动/插件重载时 MCPStartAll 后台全量连接；调用时
// 未连接也会自动拉起）；mcp 组件开关是本引擎的信任边界（显式开启 = 放行
// 该服务器的工具调用），不再逐工具审批。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// loadMCP 合并 .mcp.json（默认，可省略 mcpServers 包裹层）与 manifest.mcpServers
// （内联对象或路径；同名服务器覆盖 .mcp.json 条目，对齐 Claude 规则）。
func (p *CCPlugin) loadMCP() {
	p.MCPDefs = map[string]mcpServerDef{}
	// .mcp.json 文件
	if data, err := os.ReadFile(filepath.Join(p.Root, ".mcp.json")); err == nil {
		ccParseMCPServers(data, p.MCPDefs)
	}
	// manifest.mcpServers：对象内联或路径
	raw := p.Manifest.MCPServers
	if len(raw) == 0 {
		return
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && strings.HasSuffix(one, ".json") {
		if data, err := os.ReadFile(filepath.Join(p.Root, filepath.FromSlash(strings.TrimPrefix(one, "./")))); err == nil {
			ccParseMCPServers(data, p.MCPDefs)
		}
		return
	}
	ccParseMCPServers(raw, p.MCPDefs)
}

// ccParseMCPServers 解析一段 MCP 配置 JSON（容忍 {"mcpServers": {...}} 包裹层）。
func ccParseMCPServers(data []byte, into map[string]mcpServerDef) {
	var wrapped struct {
		MCPServers map[string]mcpServerDef `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.MCPServers) > 0 {
		for k, v := range wrapped.MCPServers {
			into[k] = v
		}
		return
	}
	var flat map[string]mcpServerDef
	if err := json.Unmarshal(data, &flat); err == nil {
		for k, v := range flat {
			into[k] = v
		}
	}
}

// ccPluginHost 是 CC 插件侧宿主（mcpHost 的插件实现）。
type ccPluginHost struct {
	p   *CCPlugin
	reg *CCPluginRegistry
}

func (h ccPluginHost) ExpandVars(s string) string {
	return h.p.expandVars(s, h.reg.UserConfigOf(h.p))
}

func (h ccPluginHost) WorkDir() string { return h.p.Root }

// ---------- 注册表侧的 MCP 管理 ----------

// mcpClients 返回全部应启动的 MCP 客户端（mcp 组件放行的插件的每个服务器）。
func (r *CCPluginRegistry) mcpClients() []*mcpClient {
	out := []*mcpClient{}
	for _, p := range r.enabled() {
		if !p.componentEnabled("mcp") {
			continue
		}
		names := make([]string, 0, len(p.MCPDefs))
		for n := range p.MCPDefs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, &mcpClient{
				fullName: p.Name + "_" + n, def: p.MCPDefs[n], host: ccPluginHost{p: p, reg: r},
			})
		}
	}
	return out
}

// MCPStartAll 连接全部启用的 MCP 服务器（引擎启动/插件重载时后台执行；失败记日志不阻断）。
func (r *CCPluginRegistry) MCPStartAll() {
	if r == nil {
		return
	}
	clients := r.mcpClients()
	if len(clients) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(c *mcpClient) {
			defer wg.Done()
			// 这些实例按设计是一次性的（mcpClients() 每次新建，工具目录里本来就
			// 看不到它们），warm-up 完必须回收：否则每次启动/插件重载都留下
			// 进程与常驻读行 goroutine。工具可见性维持现状，不在本次改动。
			defer c.shutdown()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.start(ctx); err != nil {
				log.Printf("[CC插件] MCP %s 启动失败: %v", c.fullName, err)
			} else {
				up, tools := c.connSnapshot()
				log.Printf("[CC插件] MCP %s 已连接（%v，工具 %d 个）", c.fullName, up, len(tools))
			}
		}(c)
	}
	go wg.Wait() // 后台进行，不阻塞启动
}

// MCPTools 返回全部已连接服务器的工具（挂进 toolsFor 的动态目录段）。
// 未连接成功的服务器不出现——工具目录只在重载边界变化，会话中保持稳定。
func (r *CCPluginRegistry) MCPTools() []Tool {
	if r == nil {
		return nil
	}
	out := []Tool{}
	for _, c := range r.mcpClients() {
		c.mu.Lock()
		tools, started := c.tools, c.started
		c.mu.Unlock()
		if !started {
			continue
		}
		for _, t := range tools {
			tl := t
			full := "mcp__" + c.fullName + "__" + tl.Name
			params := tl.InputSchema
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, Tool{
				Spec: ToolDef{Type: "function", Function: ToolSpec{
					Name: full, Description: ccToolDesc(tl, c.fullName), Parameters: params,
				}},
				Run: func(t *toolContext, args json.RawMessage) (string, error) {
					// 只读档：插件 MCP 工具不经命令沙箱也不走文件围栏，统一拦下
					if err := requireNotReadOnly(t, "调用插件 MCP 工具 "+full); err != nil {
						return "", err
					}
					text, images, err := t.app.CCPlugins.mcpCall(t.ctx, full, args)
					if err != nil {
						return "", err
					}
					// 随结果回传的图片入库为附件（与独立连接器同一注入链路）
					if note := mcpImagesEnvelope(t.app, images); note != "" {
						text = note + "\n" + text
					}
					return text, nil
				},
			})
		}
	}
	return out
}

// ccToolDesc 组装 MCP 工具描述（含来源标注）。
func ccToolDesc(t mcpToolDef, server string) string {
	desc := strings.TrimSpace(t.Description)
	if desc == "" {
		desc = "MCP 工具（来自插件服务器 " + server + "）"
	} else {
		desc += "（MCP 工具，来自插件服务器 " + server + "）"
	}
	return desc
}

// mcpCall 按完整工具名路由到客户端执行（图片随第二返回值交给调用方处置）。
func (r *CCPluginRegistry) mcpCall(ctx context.Context, full string, args json.RawMessage) (string, []mcpImage, error) {
	// 名形如 mcp__<插件>_<服务器>__<工具>：按工具清单反查
	for _, c := range r.mcpClients() {
		prefix := "mcp__" + c.fullName + "__"
		if strings.HasPrefix(full, prefix) {
			// 同样是一次性实例：调用结束即回收（进程与读行 goroutine 都不留）。
			defer c.shutdown()
			if up, _ := c.connSnapshot(); !up {
				if err := c.start(ctx); err != nil {
					return "", nil, err
				}
			}
			return c.callTool(ctx, strings.TrimPrefix(full, prefix), args)
		}
	}
	return "", nil, fmt.Errorf("MCP 工具 %s 不在已连接清单里", full)
}
