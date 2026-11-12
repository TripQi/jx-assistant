package main

// 远程工作区（SSH）：把「一套运行环境搬到远程」的能力提前搬到了本栈。
// DSH 官方的远程工作区只在 mac/Linux 宿主上可用；这里用纯 Go 的 SSH
// 协议栈实现同等模型，Windows 宿主同样可用：
//
//   - 会话挂上 remote 后，工作区边界换为远程目录（权限引擎照常把关）；
//   - 文件工具（读/写/编辑/列目录/glob/grep）走 SFTP；
//   - run_command 走 SSH 远程 shell（cd 到远程目录后执行）；
//   - 主机密钥采用 OpenSSH 式 TOFU（首连记录指纹，之后变更即拒绝），
//     指纹库存 data/ssh_known_hosts；
//   - 连接按会话缓存 + keepalive，空闲自动回收。
//
// 认证支持：私钥文件（可选口令）与密码。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// RemoteWorkspace 描述一个 SSH 远程工作区（挂在会话上，存 SQLite）。
type RemoteWorkspace struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"` // 默认 22
	User     string `json:"user,omitempty"` // 默认 root
	Auth     string `json:"auth,omitempty"` // key=私钥文件（默认） | password=密码
	KeyPath  string `json:"keyPath,omitempty"`
	Password string `json:"password,omitempty"` // 仅 auth=password 时使用
	Dir      string `json:"dir"`                // 远程工作目录（绝对路径）
	HostKey  string `json:"hostKey,omitempty"`  // SHA256:… 指纹（测试连接时记录绑定）
}

// normalize 校验并补全缺省字段，返回规范化副本。
func (r *RemoteWorkspace) normalize() (*RemoteWorkspace, error) {
	out := *r
	out.Host = strings.TrimSpace(out.Host)
	out.User = strings.TrimSpace(out.User)
	out.Dir = strings.TrimSpace(out.Dir)
	out.KeyPath = strings.TrimSpace(out.KeyPath)
	if out.Host == "" {
		return nil, fmt.Errorf("host 不能为空")
	}
	if out.Port <= 0 || out.Port > 65535 {
		out.Port = 22
	}
	if out.User == "" {
		out.User = "root"
	}
	if out.Auth != "password" {
		out.Auth = "key"
	}
	if out.Auth == "key" && out.KeyPath == "" {
		return nil, fmt.Errorf("私钥文件路径不能为空（auth=key）")
	}
	if out.Auth == "password" && out.Password == "" {
		return nil, fmt.Errorf("密码不能为空（auth=password）")
	}
	if out.Dir == "" {
		out.Dir = "/"
	}
	if !strings.HasPrefix(out.Dir, "/") {
		return nil, fmt.Errorf("远程目录必须是绝对路径（以 / 开头）")
	}
	return &out, nil
}

// Display 返回 user@host:dir 形式的展示名。
func (r *RemoteWorkspace) Display() string {
	u := r.User
	if u == "" {
		u = "root"
	}
	port := ""
	if r.Port != 0 && r.Port != 22 {
		port = fmt.Sprintf(":%d", r.Port)
	}
	return u + "@" + r.Host + port + ":" + r.Dir
}

// ---------- 连接管理 ----------

// remoteClient 是一个会话的远程连接：SSH 通道 + 惰性建立的 SFTP 通道。
type remoteClient struct {
	client     *ssh.Client
	spec       *RemoteWorkspace
	home       string // 远程用户主目录（懒探测缓存）
	mu         sync.Mutex
	sftp       *sftp.Client
	sftpErr    error
	lastUsed   time.Time
	stop       chan struct{}
	arch       string // 远端架构（amd64/arm64，探测后缓存）
	helperPath string // 远端沙箱 helper 路径（上传后缓存）
}

// 采集阶段的头尾分配：头部窗口占 3/4、滚动尾部窗口占 1/4。尾巴不必太大——
// 下游 clipHeadTail 还要在这 256KB 上再切一次（64KB 的 1/3≈21KB），留 64KB 够用。
const (
	remoteCaptureHeadNum = 3
	remoteCaptureHeadDen = 4
)

// capWriter 是带上限的远程命令输出收集器：总量封顶，但「头尾都留」。
//
// 布局：头部窗口先填满（占上限 3/4），之后的内容进滚动尾部窗口（占 1/4）——新
// 内容不断挤掉窗口里最老的字节，所以留下的是**真正的结尾**，而不是「前 256KB 的
// 后一小段」。被挤掉的累计字节数记在 dropped 里：下游据此把「原始总量 / 中间省略」
// 说成实数，否则按采集到的长度报数会让人以为看到的是完整输出的首尾。
//
// ssh 会话的 stdout / stderr 由两个 goroutine 各自写入，这里加锁（原实现直接往
// bytes.Buffer 里写，两个方向并发写是数据竞争）。
type capWriter struct {
	cap     int
	head    bytes.Buffer // 头部固定窗口（cap*3/4）
	tail    []byte       // 尾部窗口的底层存储：有效部分是 tail[tailOff:]
	tailOff int          // 有效尾部窗口的起始下标（滚出去的老字节留在前面，攒够再搬）
	dropped int64        // 采集阶段真正丢弃（被滚出窗口）的字节数
	mu      sync.Mutex
}

func (w *capWriter) headLimit() int { return w.cap * remoteCaptureHeadNum / remoteCaptureHeadDen }

func (w *capWriter) tailLimit() int { return w.cap - w.headLimit() }

// tailBytes 返回当前有效的尾部窗口（只含最新的 tailLimit 字节）。
func (w *capWriter) tailBytes() []byte { return w.tail[w.tailOff:] }

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.headLimit() - w.head.Len(); room > 0 {
		if len(p) <= room {
			w.head.Write(p)
			return n, nil
		}
		w.head.Write(p[:room])
		p = p[room:]
	}
	w.appendTail(p)
	return n, nil
}

// appendTail 把 p 压进滚动尾部窗口；溢出时把最老的字节挤出窗口并计入 dropped。
// 挤出靠移动 tailOff 完成（不搬数据），攒到一定量才整体整理一次，写入量大时
// 也不会退化成 O(n²)。
func (w *capWriter) appendTail(p []byte) {
	if len(p) == 0 {
		return
	}
	limit := w.tailLimit()
	if limit <= 0 {
		w.dropped += int64(len(p))
		return
	}
	if len(p) >= limit {
		// 这一次写入就不止一个窗口：旧内容全丢，只留最新的 limit 字节
		w.dropped += int64(len(w.tailBytes()) + len(p) - limit)
		w.tail = append(w.tail[:0], p[len(p)-limit:]...)
		w.tailOff = 0
		return
	}
	// 攒得够久（或容量不够）就整理一次，摊还 O(1)
	if cap(w.tail)-len(w.tail) < len(p) || w.tailOff > limit {
		w.tail = append(w.tail[:0], w.tail[w.tailOff:]...)
		w.tailOff = 0
	}
	w.tail = append(w.tail, p...)
	if over := len(w.tail) - w.tailOff - limit; over > 0 {
		w.dropped += int64(over)
		w.tailOff += over
	}
}

// Bytes 返回「头部窗口 + 尾部窗口」（中间被滚掉的部分不在其中）。接缝在取用时
// 按 rune 边界收口：ssh 按 chunk 写入，头尾切点很容易落在多字节字符中间，若不
// 修正，下游 decodeCmdOutput 会把整段误判成非 UTF-8 而走 GB18030，尾部变乱码。
func (w *capWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytesLocked()
}

func (w *capWriter) bytesLocked() []byte {
	head := w.head.Bytes()
	tail := w.tailBytes()
	if len(tail) == 0 {
		out := make([]byte, len(head))
		copy(out, head)
		return out
	}
	// 先看原样拼接是否已经合法（纯 ASCII 的常见情况什么都不用动；GBK 数据本来就
	// 不是合法 UTF-8，找不到能变合法的切法时也按原样返回，交给下游解码兜底）
	build := func(hCut, tCut int) []byte {
		out := make([]byte, 0, len(head)-hCut+len(tail)-tCut)
		out = append(out, head[:len(head)-hCut]...)
		return append(out, tail[tCut:]...)
	}
	if b := build(0, 0); utf8.Valid(b) {
		return b
	}
	for hCut := 0; hCut <= utf8.UTFMax-1 && hCut < len(head); hCut++ {
		for tCut := 0; tCut <= utf8.UTFMax-1 && tCut < len(tail); tCut++ {
			if b := build(hCut, tCut); utf8.Valid(b) {
				return b
			}
		}
	}
	return build(0, 0)
}

// Captured 返回已采集的字节数（头窗口 + 尾窗口，不含被滚掉的）。
func (w *capWriter) Captured() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.head.Len() + len(w.tailBytes())
}

// Dropped 返回采集阶段被滚动窗口挤掉的字节数（0 = 输出完整，什么都没丢）。
func (w *capWriter) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// String 调试/日志用：与 Bytes 同内容。
func (w *capWriter) String() string { return string(w.Bytes()) }

// remotePool 按会话缓存连接；keepalive 失败的连接在下次取用时重拨。
type remotePool struct {
	mu      sync.Mutex
	clients map[string]*remoteClient
}

func (a *App) knownHostsPath() string {
	return filepathAbsOf(filepath.Join(a.Cfg.DataDir, "ssh_known_hosts"))
}

// hostKeyCallback 实现 OpenSSH 式 TOFU：库里有记录则必须匹配，
// 没有则首连记录（accept-new）。会话里绑定的指纹优先于全局库。
func (a *App) hostKeyCallback(bound string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		if bound != "" {
			if bound != fp {
				return fmt.Errorf("主机指纹与该工作区绑定的不一致（当前 %s，绑定 %s），拒绝连接", fp, bound)
			}
			return nil
		}
		file := a.knownHostsPath()
		data, _ := os.ReadFile(file)
		for _, ln := range strings.Split(string(data), "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			parts := strings.Fields(ln)
			if len(parts) == 2 && parts[0] == hostname && parts[1] == fp {
				return nil // 已知且匹配
			}
			if len(parts) == 2 && parts[0] == hostname {
				return fmt.Errorf("主机指纹已变更（库中 %s，当前 %s），可能存在中间人攻击，拒绝连接；确认无误后删除 %s 中该行再试", parts[1], fp, file)
			}
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			_, _ = f.WriteString(hostname + " " + fp + "\n")
			f.Close()
		}
		return nil // 首连记录（accept-new）
	}
}

// dialSSH 按规格建立一条 SSH 连接。
func (a *App) dialSSH(spec *RemoteWorkspace, boundHostKey string) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            spec.User,
		HostKeyCallback: a.hostKeyCallback(boundHostKey),
		Timeout:         12 * time.Second,
	}
	switch spec.Auth {
	case "password":
		cfg.Auth = []ssh.AuthMethod{ssh.Password(spec.Password)}
	default:
		pem, err := os.ReadFile(spec.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取私钥失败: %w", err)
		}
		signer, perr := ssh.ParsePrivateKey(pem)
		if perr != nil {
			var missing *ssh.PassphraseMissingError
			if errors.As(perr, &missing) {
				return nil, fmt.Errorf("私钥设置了口令，当前版本请改用无口令私钥或密码认证")
			}
			return nil, fmt.Errorf("解析私钥失败: %w", perr)
		}
		cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	}
	addr := net.JoinHostPort(spec.Host, fmt.Sprintf("%d", spec.Port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	return client, nil
}

// getRemoteClient 取会话的远程连接；没有或已断开则重拨。
func (a *App) getRemoteClient(sess *SessionMeta) (*remoteClient, error) {
	if sess.Remote == nil {
		return nil, fmt.Errorf("该会话未挂载远程工作区")
	}
	a.remoteMu.Lock()
	c, ok := a.remotes[sess.ID]
	a.remoteMu.Unlock()
	if ok {
		_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
		if err == nil {
			c.lastUsed = time.Now()
			return c, nil
		}
		// 连接已死：回收后重拨
		c.close()
		a.dropRemote(sess.ID)
	}
	spec, err := sess.Remote.normalize()
	if err != nil {
		return nil, err
	}
	client, err := a.dialSSH(spec, spec.HostKey)
	if err != nil {
		return nil, err
	}
	c = &remoteClient{client: client, spec: spec, lastUsed: time.Now(), stop: make(chan struct{})}
	a.remoteMu.Lock()
	a.remotes[sess.ID] = c
	a.remoteMu.Unlock()
	go c.keepAlive()
	return c, nil
}

func (a *App) dropRemote(sessID string) {
	a.remoteMu.Lock()
	c, ok := a.remotes[sessID]
	if ok {
		delete(a.remotes, sessID)
	}
	a.remoteMu.Unlock()
	if ok {
		c.close()
	}
}

// closeRemoteSession 会话删除/解除远程时调用，立即断开其连接。
func (a *App) closeRemoteSession(sessID string) { a.dropRemote(sessID) }

func (c *remoteClient) close() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	if c.sftp != nil {
		_ = c.sftp.Close()
	}
	_ = c.client.Close()
}

// keepAlive 每 25s 一次保活探活，失败即退出（下次取用时重拨）。
func (c *remoteClient) keepAlive() {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			if _, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}

// sftpClient 惰性建立（并缓存）SFTP 通道。
func (c *remoteClient) sftpClient() (*sftp.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sftp != nil {
		return c.sftp, nil
	}
	if c.sftpErr != nil {
		return nil, c.sftpErr
	}
	sc, err := sftp.NewClient(c.client)
	if err != nil {
		c.sftpErr = fmt.Errorf("打开 SFTP 子系统失败: %w", err)
		return nil, c.sftpErr
	}
	c.sftp = sc
	return sc, nil
}

// shellQuote 按 POSIX 规则给路径加引号。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// run 在远程目录下执行命令，行为对齐本地 runCommand：合并输出、超时终止、
// 输出截断；ctx 取消时杀掉远程命令。
func (c *remoteClient) run(ctx context.Context, dir, command string, timeout time.Duration) (string, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("建立远程会话失败: %w", err)
	}
	defer sess.Close()
	bufCap := 256 * 1024
	var buf = capWriter{cap: bufCap} // 远程输出截断（作者预留的 bufCap 按此接上）
	sess.Stdout = &buf
	sess.Stderr = &buf
	wrapped := "cd " + shellQuote(dir) + " && { " + command + " ;}"
	if err := sess.Start(wrapped); err != nil {
		return "", fmt.Errorf("启动远程命令失败: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	timedOut := false
	timer := time.AfterFunc(timeout, func() {
		timedOut = true
		_ = sess.Close()
	})
	defer timer.Stop()
	watchCtx := make(chan struct{})
	defer close(watchCtx)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-watchCtx:
		}
	}()
	<-done
	out := buf.Bytes()
	note := ""
	if timedOut {
		note += "\n[远程命令超时被终止]"
	}
	if ctx.Err() != nil {
		note += "\n[远程命令被取消]"
	}
	// 先解码再截断：字节层截断会把 GBK / UTF-8 多字节序列切断，尾部变乱码。
	// 采集层（capWriter 滚动窗口）已丢掉的字节要算进「原始总量」：否则标记里的
	// 「原始共 N 字节 / 中间省略 X 字节」只统计了采集到的部分，看上去像是把一份
	// 完整输出从中间挖掉一段，而不是「本来还有更多、采集阶段就滚掉了」。口径都是
	// 字节数（GBK 输出解码后长度会变，量级仍如实）。
	dropped := buf.Dropped()
	text := strings.TrimRight(decodeCmdOutput(out), "\r\n")
	res := clipHeadTail(text, cmdOutputMaxBytes, func(total, head, tail int) string {
		return cmdTruncNote(total+int(dropped), head, tail)
	}) + note
	if strings.TrimSpace(res) == "" {
		res = "（命令执行完成，无输出）"
	}
	return res, nil
}

// ---------- SFTP 文件操作 ----------

func (c *remoteClient) readFile(p string) ([]byte, error) {
	sc, err := c.sftpClient()
	if err != nil {
		return nil, err
	}
	f, err := sc.Open(p)
	if err != nil {
		return nil, fmt.Errorf("打开远程文件失败: %w", err)
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 16<<20))
}

func (c *remoteClient) mkdirAll(dir string) error {
	sc, err := c.sftpClient()
	if err != nil {
		return err
	}
	cur := ""
	for _, seg := range strings.Split(path.Clean(dir), "/") {
		if seg == "" {
			cur = "/"
			continue
		}
		cur = path.Join(cur, seg)
		if err := sc.Mkdir(cur); err != nil && !os.IsExist(err) {
			if info, serr := sc.Stat(cur); serr == nil && info.IsDir() {
				continue
			}
			return fmt.Errorf("创建远程目录 %s 失败: %w", cur, err)
		}
	}
	return nil
}

func (c *remoteClient) writeFile(p string, data []byte) error {
	sc, err := c.sftpClient()
	if err != nil {
		return err
	}
	f, err := sc.Create(p)
	if err != nil {
		return fmt.Errorf("创建远程文件失败: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("写入远程文件失败: %w", err)
	}
	return nil
}

// remove 删除远程文件（文件快照回退里"撤回新建文件"用；不存在视为成功）。
func (c *remoteClient) remove(p string) error {
	sc, err := c.sftpClient()
	if err != nil {
		return err
	}
	if err := sc.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除远程文件失败: %w", err)
	}
	return nil
}

// envEntry 统一形态定义在 tools.go（执行环境抽象），此处不再重复。

func (c *remoteClient) listDir(p string) ([]envEntry, error) {
	sc, err := c.sftpClient()
	if err != nil {
		return nil, err
	}
	infos, err := sc.ReadDir(p)
	if err != nil {
		return nil, fmt.Errorf("读取远程目录失败: %w", err)
	}
	out := []envEntry{}
	for _, fi := range infos {
		out = append(out, envEntry{Name: fi.Name(), IsDir: fi.IsDir(), Size: fi.Size()})
	}
	return out, nil
}

func (c *remoteClient) stat(p string) (bool, int64, error) {
	sc, err := c.sftpClient()
	if err != nil {
		return false, 0, err
	}
	fi, err := sc.Stat(p)
	if err != nil {
		return false, 0, err
	}
	return fi.IsDir(), fi.Size(), nil
}

// testRemoteWorkspace 测试一个远程工作区配置：拨号 → 跑 echo → 检查目录。
// 返回实际主机指纹（供前端展示与绑定到会话）。
func (a *App) testRemoteWorkspace(spec *RemoteWorkspace) (string, error) {
	norm, err := spec.normalize()
	if err != nil {
		return "", err
	}
	// 测试连接不做 TOFU 记录绑定冲突处理：dial 时 accept-new 已记录。
	client, err := a.dialSSH(norm, "")
	if err != nil {
		return "", err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("建立远程会话失败: %w", err)
	}
	defer sess.Close()
	var out bytes.Buffer
	sess.Stdout = &out
	if err := sess.Run("printf __ok__"); err != nil || strings.TrimSpace(out.String()) != "__ok__" {
		return "", fmt.Errorf("远程命令通道不可用: %v", err)
	}
	// 指纹：从 known_hosts 里回查（dial 刚写入或已存在）
	fp := ""
	if data, rerr := os.ReadFile(a.knownHostsPath()); rerr == nil {
		host := net.JoinHostPort(norm.Host, fmt.Sprintf("%d", norm.Port))
		for _, ln := range strings.Split(string(data), "\n") {
			parts := strings.Fields(strings.TrimSpace(ln))
			if len(parts) == 2 && (parts[0] == host || (norm.Port == 22 && parts[0] == norm.Host)) {
				fp = parts[1]
			}
		}
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return fp, fmt.Errorf("SFTP 子系统不可用（文件工具需要它）: %w", err)
	}
	defer sc.Close()
	fi, err := sc.Stat(norm.Dir)
	if err != nil {
		return fp, fmt.Errorf("目录 %s 不存在或不可访问: %w", norm.Dir, err)
	}
	if !fi.IsDir() {
		return fp, fmt.Errorf("%s 不是目录", norm.Dir)
	}
	return fp, nil
}

// filepathAbsOf 是 filepath.Abs 的宽松版（失败时原样返回）。
func filepathAbsOf(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// ---------- 目录浏览（前端「选择远程工作目录」用） ----------

// browseKey 对一个连接规格生成稳定的伪会话键，让目录浏览复用连接池，
// 连续导航不重复拨号。
func browseKey(spec *RemoteWorkspace) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s",
		spec.Host, spec.Port, spec.User, spec.Auth, spec.KeyPath)))
	return "browse:" + hex.EncodeToString(sum[:8])
}

// homeDir 探测远程用户主目录（懒执行一次并缓存），探测失败回退 "/"。
func (c *remoteClient) homeDir() string {
	c.mu.Lock()
	if c.home != "" {
		h := c.home
		c.mu.Unlock()
		return h
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := c.run(ctx, "/", `printf %s "${HOME:-/root}"`, 6*time.Second)
	h := strings.TrimSpace(out)
	if err != nil || h == "" || !strings.HasPrefix(h, "/") {
		h = "/"
	}
	c.mu.Lock()
	c.home = h
	c.mu.Unlock()
	return h
}

// getBrowseClient 取（或建立）一条用于目录浏览的连接；闲置超 5 分钟的
// 浏览连接在建立新连接时顺手回收。
func (a *App) getBrowseClient(spec *RemoteWorkspace) (*remoteClient, error) {
	key := browseKey(spec)
	a.remoteMu.Lock()
	c, ok := a.remotes[key]
	a.remoteMu.Unlock()
	if ok {
		if _, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil); err == nil {
			c.lastUsed = time.Now()
			return c, nil
		}
		c.close()
		a.dropRemote(key)
	}
	norm, err := spec.normalize()
	if err != nil {
		return nil, err
	}
	client, err := a.dialSSH(norm, norm.HostKey)
	if err != nil {
		return nil, err
	}
	nc := &remoteClient{client: client, spec: norm, lastUsed: time.Now(), stop: make(chan struct{})}
	a.remoteMu.Lock()
	if existing, busy := a.remotes[key]; busy { // 并发拨号时后来者让位
		a.remoteMu.Unlock()
		nc.close()
		existing.lastUsed = time.Now()
		return existing, nil
	}
	var stale []*remoteClient
	for k, oc := range a.remotes {
		if strings.HasPrefix(k, "browse:") && time.Since(oc.lastUsed) > 5*time.Minute {
			stale = append(stale, oc)
			delete(a.remotes, k)
		}
	}
	a.remotes[key] = nc
	a.remoteMu.Unlock()
	for _, oc := range stale {
		oc.close()
	}
	go nc.keepAlive()
	return nc, nil
}
