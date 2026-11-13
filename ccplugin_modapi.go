package main

// mods 的 $ API（$ 句柄）：fs / process / http / store / clock / env / model /
// prompt / session / command / tool / ui / mcp / settings / state / plugin。
// 实现对齐 Claude Code 的 mods API 形态，差异点（有意为之，AGENT.md 列明）：
//   - 敏感操作（fs.write / process.run / http.fetch / model.*）过会话权限门控
//     （read-only 会话拒绝写与执行）+ 危险命令护栏；Claude 无沙箱
//   - ui 仅 toast / status / log（映射到 Web 前端事件）；渲染类方法为 no-op
//   - $.env.set 只允许插件命名空间前缀（JXCC_<插件名>_）
//   - $.process.spawn、$.audio、$.telemetry 等不支持（调用即抛 TypeError）

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// installHostGlobals 安装 mod 环境的全局（对齐 Claude：无 setTimeout 等，
// Web 全局由 goja 自带；clock 由 $ 提供）。
func (h *modHost) installHostGlobals() {
	// goja 自带 console.log 等；不设 Node API（require/module 由包装提供）
}

// buildDollar 构造 $ 句柄（每次事件分发新建绑定当前会话的轻量包装）。
func (h *modHost) buildDollar(sess *modSessCtx) goja.Value {
	d := h.vm.NewObject()
	setNS := func(name string, fn func(*goja.Object)) {
		ns := h.vm.NewObject()
		fn(ns)
		_ = d.Set(name, ns)
	}

	pluginNS := h.vm.NewObject()
	_ = pluginNS.Set("name", h.plugin.Name)
	_ = pluginNS.Set("root", h.plugin.Root)
	_ = d.Set("plugin", pluginNS)

	// ---------- $.fs ----------
	setNS("fs", func(ns *goja.Object) {
		_ = ns.Set("read", wrapFS(h, func(path string) any {
			data, err := os.ReadFile(h.absPath(path, sess))
			if err != nil {
				panic(h.vm.NewGoError(err))
			}
			if len(data) > 4<<20 {
				panic(h.vm.NewTypeError("文件超过 4MiB 上限"))
			}
			return string(data)
		}))
		_ = ns.Set("write", func(call goja.FunctionCall) goja.Value {
			h.requireWrite(sess)
			full := h.absPath(call.Argument(0).String(), sess)
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				panic(h.vm.NewGoError(err))
			}
			if err := os.WriteFile(full, []byte(call.Argument(1).String()), 0644); err != nil {
				panic(h.vm.NewGoError(err))
			}
			return goja.Undefined()
		})
		_ = ns.Set("exists", wrapFS(h, func(path string) bool {
			_, err := os.Stat(h.absPath(path, sess))
			return err == nil
		}))
		_ = ns.Set("stat", wrapFS(h, func(path string) map[string]any {
			info, err := os.Stat(h.absPath(path, sess))
			if err != nil {
				panic(h.vm.NewGoError(err))
			}
			return map[string]any{"name": info.Name(), "size": info.Size(), "isDir": info.IsDir(),
				"mtime": info.ModTime().UnixMilli()}
		}))
		_ = ns.Set("list", wrapFS(h, func(path string) []map[string]any {
			entries, err := os.ReadDir(h.absPath(path, sess))
			if err != nil {
				panic(h.vm.NewGoError(err))
			}
			out := []map[string]any{}
			for _, e := range entries {
				out = append(out, map[string]any{"name": e.Name(), "kind": map[bool]string{true: "dir", false: "file"}[e.IsDir()]})
			}
			return out
		}))
	})

	// ---------- $.process ----------
	setNS("process", func(ns *goja.Object) {
		_ = ns.Set("run", func(call goja.FunctionCall) goja.Value {
			h.requireWrite(sess) // 执行进程与写文件同级门控
			var argv []string
			if a, ok := call.Argument(0).Export().([]any); ok {
				for _, item := range a {
					argv = append(argv, fmt.Sprintf("%v", item))
				}
			}
			if len(argv) == 0 {
				panic(h.vm.NewTypeError("process.run 需要参数数组"))
			}
			if why := destructiveCommandHit(strings.Join(argv, " ")); why != "" && h.reg.app != nil && h.reg.app.guardEnabled() {
				panic(h.vm.NewTypeError("命令命中危险命令护栏：" + why))
			}
			timeout := 30 * time.Second
			ctx := h.sessContext(sess)
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			exit := 0
			if err := cmd.Run(); err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					exit = ee.ExitCode()
				} else {
					panic(h.vm.NewGoError(err))
				}
			}
			return h.vm.ToValue(map[string]any{"exitCode": exit, "stdout": stdout.String(), "stderr": stderr.String()})
		})
		_ = ns.Set("spawn", func(call goja.FunctionCall) goja.Value {
			panic(h.vm.NewTypeError("process.spawn 暂不支持"))
		})
	})

	// ---------- $.http ----------
	setNS("http", func(ns *goja.Object) {
		_ = ns.Set("fetch", h.asyncOrSync(func(call goja.FunctionCall) goja.Value {
			url := call.Argument(0).String()
			if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
				panic(h.vm.NewTypeError("http.fetch 只支持 http/https"))
			}
			method, body := "GET", ""
			if init, ok := call.Argument(1).Export().(map[string]any); ok {
				if m, ok := init["method"].(string); ok {
					method = strings.ToUpper(m)
				}
				if b, ok := init["body"].(string); ok {
					body = b
				}
			}
			ctx := h.sessContext(sess)
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			var rd io.Reader
			if body != "" {
				rd = strings.NewReader(body)
			}
			req, err := http.NewRequestWithContext(cctx, method, url, rd)
			if err != nil {
				panic(h.vm.NewGoError(err))
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				panic(h.vm.NewGoError(err))
			}
			defer resp.Body.Close()
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			headers := map[string]any{}
			for k := range resp.Header {
				headers[k] = resp.Header.Get(k)
			}
			return h.vm.ToValue(map[string]any{
				"status": resp.StatusCode, "ok": resp.StatusCode < 400,
				"headers": headers, "text": string(data),
			})
		}))
	})

	// ---------- $.store ----------
	setNS("store", func(ns *goja.Object) {
		_ = ns.Set("get", func(call goja.FunctionCall) goja.Value {
			k := call.Argument(0).String()
			h.storeMu.Lock()
			defer h.storeMu.Unlock()
			if v, ok := h.store[k]; ok {
				return h.vm.ToValue(v)
			}
			return goja.Undefined()
		})
		_ = ns.Set("set", func(call goja.FunctionCall) goja.Value {
			k := call.Argument(0).String()
			h.storeMu.Lock()
			h.store[k] = call.Argument(1).Export()
			h.saveStoreLocked()
			h.storeMu.Unlock()
			return goja.Undefined()
		})
		_ = ns.Set("delete", func(call goja.FunctionCall) goja.Value {
			k := call.Argument(0).String()
			h.storeMu.Lock()
			delete(h.store, k)
			h.saveStoreLocked()
			h.storeMu.Unlock()
			return goja.Undefined()
		})
		_ = ns.Set("keys", func(call goja.FunctionCall) goja.Value {
			h.storeMu.Lock()
			defer h.storeMu.Unlock()
			keys := make([]string, 0, len(h.store))
			for k := range h.store {
				keys = append(keys, k)
			}
			return h.vm.ToValue(keys)
		})
	})

	// ---------- $.clock ----------
	setNS("clock", func(ns *goja.Object) {
		_ = ns.Set("now", func(call goja.FunctionCall) goja.Value {
			return h.vm.ToValue(time.Now().UnixMilli())
		})
		_ = ns.Set("sleep", h.asyncOrSync(func(call goja.FunctionCall) goja.Value {
			ms := call.Argument(0).ToInteger()
			if ms > 10000 {
				ms = 10000
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
			return goja.Undefined()
		}))
		_ = ns.Set("after", func(call goja.FunctionCall) goja.Value {
			ms := call.Argument(0).ToInteger()
			fn, ok := goja.AssertFunction(call.Argument(1))
			if !ok {
				panic(h.vm.NewTypeError("clock.after 需要回调函数"))
			}
			timer := time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
				h.mu.Lock()
				defer h.mu.Unlock()
				if _, err := fn(goja.Undefined()); err != nil {
					logModError(h, "clock.after", err)
				}
			})
			return h.timerValue(timer)
		})
		_ = ns.Set("every", func(call goja.FunctionCall) goja.Value {
			ms := call.Argument(0).ToInteger()
			if ms < 1000 {
				ms = 1000 // 下限 1s：纯轮询场景防打爆
			}
			fn, ok := goja.AssertFunction(call.Argument(1))
			if !ok {
				panic(h.vm.NewTypeError("clock.every 需要回调函数"))
			}
			t := &time.Ticker{}
			_ = t
			stop := make(chan struct{})
			go func() {
				ticker := time.NewTicker(time.Duration(ms) * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stop:
						return
					case <-ticker.C:
						h.mu.Lock()
						_, err := fn(goja.Undefined())
						h.mu.Unlock()
						if err != nil {
							logModError(h, "clock.every", err)
						}
					}
				}
			}()
			return h.stopperValue(stop)
		})
	})

	// ---------- $.env ----------
	setNS("env", func(ns *goja.Object) {
		_ = ns.Set("get", func(call goja.FunctionCall) goja.Value {
			name := call.Argument(0).String()
			if isSensitiveEnvName(name) {
				return goja.Undefined()
			}
			return h.vm.ToValue(os.Getenv(name))
		})
		_ = ns.Set("set", func(call goja.FunctionCall) goja.Value {
			name := call.Argument(0).String()
			prefix := "JXCC_" + strings.ToUpper(ccSanitizeName(h.plugin.Name)) + "_"
			if !strings.HasPrefix(name, prefix) {
				panic(h.vm.NewTypeError("env.set 只允许 " + prefix + " 前缀的变量名"))
			}
			_ = os.Setenv(name, call.Argument(1).String())
			return goja.Undefined()
		})
	})

	// ---------- $.ui ----------
	setNS("ui", func(ns *goja.Object) {
		emitUI := func(kind, text string) {
			if h.reg.app == nil {
				return
			}
			h.reg.app.broadcast(ccUIEvent(kind, h.plugin.Name, text))
		}
		_ = ns.Set("toast", func(call goja.FunctionCall) goja.Value {
			emitUI("toast", call.Argument(0).String())
			return goja.Undefined()
		})
		_ = ns.Set("status", func(call goja.FunctionCall) goja.Value {
			emitUI("status", call.Argument(0).String())
			return goja.Undefined()
		})
		_ = ns.Set("log", func(call goja.FunctionCall) goja.Value {
			emitUI("log", call.Argument(0).String())
			return goja.Undefined()
		})
		// 渲染类 API：no-op（文档列明的差异）
		for _, noop := range []string{"invalidate", "open", "close", "focus", "scroll", "notice", "ask", "copy", "blit", "resolve", "panes"} {
			_ = ns.Set(noop, func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
		}
	})

	// ---------- $.command / $.tool ----------
	setNS("command", func(ns *goja.Object) {
		_ = ns.Set("register", func(call goja.FunctionCall) goja.Value {
			def, _ := call.Argument(0).Export().(map[string]any)
			name, _ := def["name"].(string)
			if name == "" {
				panic(h.vm.NewTypeError("command.register 需要 name"))
			}
			desc, _ := def["description"].(string)
			hint, _ := def["argumentHint"].(string)
			h.commands[h.plugin.Name+":"+name] = modCommandDef{
				Name: h.plugin.Name + ":" + name, Description: desc, ArgumentHint: hint,
			}
			return goja.Undefined()
		})
		_ = ns.Set("list", func(call goja.FunctionCall) goja.Value {
			out := []map[string]any{}
			for _, c := range h.commands {
				out = append(out, map[string]any{"name": c.Name, "description": c.Description})
			}
			return h.vm.ToValue(out)
		})
	})
	setNS("tool", func(ns *goja.Object) {
		_ = ns.Set("register", func(call goja.FunctionCall) goja.Value {
			def, _ := call.Argument(0).Export().(map[string]any)
			name, _ := def["name"].(string)
			if name == "" {
				panic(h.vm.NewTypeError("tool.register 需要 name"))
			}
			desc, _ := def["description"].(string)
			schema, _ := def["inputSchema"].(map[string]any)
			h.tools[name] = modToolDef{Name: name, Description: desc, InputSchema: schema}
			return goja.Undefined()
		})
		_ = ns.Set("list", func(call goja.FunctionCall) goja.Value {
			out := []map[string]any{}
			for _, t := range h.tools {
				out = append(out, map[string]any{"name": t.Name, "description": t.Description})
			}
			return h.vm.ToValue(out)
		})
	})

	// ---------- $.model / $.prompt / $.session / $.mcp / $.settings / $.state ----------
	setNS("model", func(ns *goja.Object) {
		_ = ns.Set("complete", h.asyncOrSync(func(call goja.FunctionCall) goja.Value {
			if h.reg.app == nil {
				panic(h.vm.NewTypeError("宿主未就绪"))
			}
			opts, _ := call.Argument(0).Export().(map[string]any)
			prompt, _ := opts["prompt"].(string)
			if strings.TrimSpace(prompt) == "" {
				panic(h.vm.NewTypeError("model.complete 需要 prompt"))
			}
			reply, err := h.reg.app.modModelComplete(h.sessContext(sess), opts)
			if err != nil {
				return h.vm.ToValue(map[string]any{"isAnswered": false, "reason": err.Error()})
			}
			return h.vm.ToValue(map[string]any{"isAnswered": true, "text": reply})
		}))
		_ = ns.Set("fork", h.asyncOrSync(func(call goja.FunctionCall) goja.Value {
			if h.reg.app == nil || sess == nil || sess.Sess == nil {
				panic(h.vm.NewTypeError("没有活动会话"))
			}
			opts, _ := call.Argument(0).Export().(map[string]any)
			prompt, _ := opts["prompt"].(string)
			reply, err := h.reg.app.modModelComplete(h.sessContext(sess), map[string]any{"prompt": prompt, "forkSession": sess.Sess.ID})
			if err != nil {
				return h.vm.ToValue(map[string]any{"isAnswered": false, "reason": err.Error()})
			}
			return h.vm.ToValue(map[string]any{"isAnswered": true, "text": reply})
		}))
	})
	setNS("prompt", func(ns *goja.Object) {
		_ = ns.Set("submit", func(call goja.FunctionCall) goja.Value {
			if h.reg.app == nil || sess == nil || sess.Sess == nil {
				panic(h.vm.NewTypeError("没有活动会话"))
			}
			opts, _ := call.Argument(0).Export().(map[string]any)
			text, _ := opts["text"].(string)
			asUser, _ := opts["asUser"].(bool)
			origin := "plugin:" + h.plugin.Name
			if asUser {
				origin = ""
			}
			h.reg.app.modSubmitPrompt(sess.Sess.ID, text, origin)
			return goja.Undefined()
		})
		_ = ns.Set("read", func(call goja.FunctionCall) goja.Value {
			if h.reg.app == nil || sess == nil || sess.Sess == nil {
				return goja.Undefined()
			}
			return h.vm.ToValue(h.reg.app.modLastUserPrompt(sess.Sess.ID))
		})
	})
	setNS("session", func(ns *goja.Object) {
		_ = ns.Set("id", func(call goja.FunctionCall) goja.Value {
			if sess == nil || sess.Sess == nil {
				return goja.Undefined()
			}
			return h.vm.ToValue(sess.Sess.ID)
		})
		_ = ns.Set("cwd", func(call goja.FunctionCall) goja.Value {
			if sess == nil || sess.Sess == nil || sess.Sess.Workspace == "" {
				return h.vm.ToValue(h.reg.app.Cfg.Workspace)
			}
			return h.vm.ToValue(sess.Sess.Workspace)
		})
		_ = ns.Set("messages", h.asyncOrSync(func(call goja.FunctionCall) goja.Value {
			if h.reg.app == nil || sess == nil || sess.Sess == nil {
				return h.vm.ToValue([]any{})
			}
			msgs, _ := h.reg.app.Store.Messages(sess.Sess.ID)
			out := []map[string]any{}
			for _, m := range msgs {
				content, _ := m.Content.(string)
				out = append(out, map[string]any{"role": m.Role, "text": content})
			}
			return h.vm.ToValue(out)
		}))
		_ = ns.Set("model", func(call goja.FunctionCall) goja.Value {
			if sess == nil || sess.Sess == nil {
				return goja.Undefined()
			}
			return h.vm.ToValue(sess.Sess.Model)
		})
		_ = ns.Set("send", func(call goja.FunctionCall) goja.Value {
			opts, _ := call.Argument(0).Export().(map[string]any)
			text, _ := opts["text"].(string)
			to, _ := opts["to"].(map[string]any)
			sid, _ := to["sessionId"].(string)
			if h.reg.app == nil || sid == "" || strings.TrimSpace(text) == "" {
				return h.vm.ToValue(map[string]any{"isDelivered": false, "reason": "需要 to.sessionId 与 text"})
			}
			h.reg.app.modSubmitPrompt(sid, text, "plugin:"+h.plugin.Name)
			return h.vm.ToValue(map[string]any{"isDelivered": true})
		})
	})
	setNS("mcp", func(ns *goja.Object) {
		_ = ns.Set("call", h.asyncOrSync(func(call goja.FunctionCall) goja.Value {
			if h.reg.app == nil || h.reg.app.CCPlugins == nil {
				panic(h.vm.NewTypeError("MCP 未启用"))
			}
			server := call.Argument(0).String()
			tool := call.Argument(1).String()
			var args json.RawMessage
			if a := call.Argument(2); a != nil && !goja.IsUndefined(a) {
				data, _ := json.Marshal(a.Export())
				args = data
			}
			out, _, err := h.reg.app.CCPlugins.mcpCall(h.sessContext(sess), "mcp__"+server+"__"+tool, args)
			if err != nil {
				panic(h.vm.NewGoError(err))
			}
			return h.vm.ToValue(out)
		}))
	})
	setNS("settings", func(ns *goja.Object) {
		_ = ns.Set("read", func(call goja.FunctionCall) goja.Value {
			return h.vm.ToValue(map[string]any{"note": "设置读取已脱敏（详见 AGENT.md 差异清单）"})
		})
	})
	setNS("state", func(ns *goja.Object) {
		_ = ns.Set("get", func(call goja.FunctionCall) goja.Value {
			k := "state." + call.Argument(0).String()
			h.storeMu.Lock()
			defer h.storeMu.Unlock()
			if v, ok := h.store[k]; ok {
				return h.vm.ToValue(v)
			}
			return goja.Undefined()
		})
		_ = ns.Set("set", func(call goja.FunctionCall) goja.Value {
			k := "state." + call.Argument(0).String()
			h.storeMu.Lock()
			h.store[k] = call.Argument(1).Export()
			h.saveStoreLocked()
			h.storeMu.Unlock()
			return goja.Undefined()
		})
	})
	return d
}

// ---------- 辅助 ----------

// wrapFS 包装 fs 方法：解包路径参数并装回 JS 值。
func wrapFS[T any](h *modHost, fn func(path string) T) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		return h.vm.ToValue(fn(call.Argument(0).String()))
	}
}

// requireWrite 敏感操作门控：read-only 会话拒绝（Claude 无此闸，见差异清单）。
func (h *modHost) requireWrite(sess *modSessCtx) {
	if sess != nil && sess.Sess != nil && sess.Sess.Permission == "read-only" {
		panic(h.vm.NewTypeError("当前会话是全电脑只读模式，mod 的写/执行操作被拒绝。可用 request_permission(target=workspace-write, justification=…) 向用户申请切换权限档，或请用户把权限芯片切到「修改工作区」后再试"))
	}
}

// absPath 相对路径按会话工作目录解析（绝对路径原样）。
func (h *modHost) absPath(p string, sess *modSessCtx) string {
	if filepath.IsAbs(p) {
		return p
	}
	base := ""
	if sess != nil && sess.Sess != nil && sess.Sess.Workspace != "" {
		base = sess.Sess.Workspace
	} else if h.reg.app != nil {
		base = h.reg.app.Cfg.Workspace
	}
	return filepath.Join(base, filepath.FromSlash(p))
}

// sessContext 取事件的 ctx（无会话时兜底 Background）。
func (h *modHost) sessContext(sess *modSessCtx) context.Context {
	if sess != nil && sess.Ctx != nil {
		return sess.Ctx
	}
	return context.Background()
}

// asyncOrSync 包装一个同步实现：await 它等价于直接取值（mod handler 里
// await Promise 包裹的宿主调用即刻 settle）。
func (h *modHost) asyncOrSync(fn func(goja.FunctionCall) goja.Value) func(goja.FunctionCall) goja.Value {
	return fn
}

// timerValue / stopperValue 包装可取消句柄。
func (h *modHost) timerValue(t *time.Timer) goja.Value {
	obj := h.vm.NewObject()
	_ = obj.Set("cancel", func(call goja.FunctionCall) goja.Value { t.Stop(); return goja.Undefined() })
	return obj
}

func (h *modHost) stopperValue(stop chan struct{}) goja.Value {
	obj := h.vm.NewObject()
	_ = obj.Set("cancel", func(call goja.FunctionCall) goja.Value { close(stop); return goja.Undefined() })
	return obj
}

// isSensitiveEnvName 拒读常见密钥环境变量。
func isSensitiveEnvName(name string) bool {
	up := strings.ToUpper(name)
	for _, frag := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(up, frag) {
			return true
		}
	}
	return false
}

// loadStore / saveStoreLocked 持久化 $.store（调用方持 h.mu；load 自持）。
func (h *modHost) loadStore() {
	data, err := os.ReadFile(filepath.Join(h.plugin.DataDir, "store.json"))
	if err == nil {
		_ = json.Unmarshal(data, &h.store)
	}
}

func (h *modHost) saveStoreLocked() {
	if h.plugin.DataDir == "" {
		return
	}
	_ = os.MkdirAll(h.plugin.DataDir, 0755)
	data, _ := json.Marshal(h.store)
	_ = os.WriteFile(filepath.Join(h.plugin.DataDir, "store.json"), data, 0644)
}

func logModError(h *modHost, where string, err error) {
	log.Printf("[CC插件] mods %s %s: %v", h.plugin.Key, where, err)
}

// ---------- App 侧的 mod 辅助（$ API 的宿主实现） ----------

// modModelComplete 单次模型调用（$.model.complete/fork 的宿主；maxTokens
// 默认 1024、上限 64000，对齐 Claude 限制）。forkSession 目前与 complete 同
// 路径（不继承会话历史——差异清单列明）。
func (a *App) modModelComplete(ctx context.Context, opts map[string]any) (string, error) {
	prompt, _ := opts["prompt"].(string)
	system, _ := opts["system"].(string)
	target, terr := a.DefaultTarget()
	if terr != nil {
		return "", terr
	}
	if m, ok := opts["model"].(string); ok && strings.TrimSpace(m) != "" {
		target.Model = strings.TrimSpace(m)
	}
	msgs := []Message{}
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, TextMessage("system", system))
	}
	msgs = append(msgs, TextMessage("user", prompt))
	req := ChatRequest{Model: target.Model, Messages: msgs}
	var out strings.Builder
	// 用量明细（组件 usage-stats）：插件 mods 的模型调用记成「其他辅助生成」
	meterCb, finishUsage := a.meterUsage(usagePurposeAssist, nil, target, "", StreamCallbacks{
		OnText: func(s string) { out.WriteString(s) },
	})
	err := StreamChat(ctx, target, req, meterCb)
	finishUsage(err)
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

// modSubmitPrompt 向会话注入一条 mod 提交的提示（排队为独立一轮；
// origin 标注来源插件，前端按通知渲染；asUser 时 origin 留空按用户消息）。
func (a *App) modSubmitPrompt(sessID, text, origin string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	kind := kindReport
	if origin == "" {
		kind = ""
	}
	a.pushPending(sessID, "queue", text, kind, origin)
}

// modLastUserPrompt 取会话最近一条用户消息原文。
func (a *App) modLastUserPrompt(sessID string) string {
	msgs, _ := a.Store.Messages(sessID)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			s, _ := msgs[i].Content.(string)
			return s
		}
	}
	return ""
}

// ccUIEvent 组一个 UI 事件（toast/status → 前端；log → 会话报告行）。
func ccUIEvent(kind, plugin, text string) Event {
	return Event{Type: "cc_mod_ui", Output: kind + "|" + plugin + "|" + text}
}
