// 服务端地址的证书策略（2026-10-26 用户口径，第二次修订）：
//
// 背景：自建 jxai-server 部署在企业内网时，绝大多数场景**拿不到公网证书**——要么
// https 自签，要么证书只签了某个域名、用 IP 访问必然报
// `x509: cannot validate certificate for 192.168.0.210 because it doesn't contain
// any IP SANs`。以前这类连接会被 TLS 校验拦死，向导里只显示"连不上"，让人误以为
// 是端口/防火墙（2026-10-26 实测有人怀疑 2227 的 UDP 没放行，其实端口全通）。
//
// 规则（按目标主机判定，不看配置，不存在"改完设置才生效"的坑）：
//
//	**域名**（jxaizz.com、企业自建域名、api.deepseek.com…）→ 跳过证书校验
//	    理由：域名只可能来自可信渠道——打包时写死的总服务器地址、向导里客户
//	    自己填的地址、公告下发的地址。发布路径本身是安全的，证书不是它们的
//	    安全边界（内网自建服务根本申请不到证书）。
//	**内网/回环/链路本地 IP**（192.168/16、10/8、172.16/12、127/8、169.254/16、
//	    ::1、fe80::/10、fc00::/7）→ 跳过证书校验
//	    理由：同上；流量仍在 TLS 加密里，比把它降级成 http 安全得多。
//	**公网 IP**（8.8.8.8、1.1.1.1…）→ **保持严格校验**
//	    理由：这是唯一真正不可信的来源——UDP 广播发现时，任何一台机器（甚至跨
//	    网段/隧道过来的）都能抢答"我就是服务器"，然后把你引到某个公网 IP 上。
//	    公网 IP 通常也没有匹配的证书，所以这类目标会自然连不上——正是我们要的。
//
// 代价（明确接受）：域名一律不验证书，第三方 API（deepseek/zhipu 等）理论上可被
// 中间人冒充。客户端只主动连"用户配置/公告下发的服务端"与这些官方 API，接受该取舍；
// 想恢复严格校验时，只需把 belowHandle 里返回 true 的分支去掉。

package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"
)

// relaxedTLSFor 该主机是否跳过证书校验（判据见文件头）：域名与内网 IP 放宽，
// 只有公网 IP 要求证书严格匹配。
func relaxedTLSFor(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true // 域名（含单标签内网名）：来源可信，免校验
	}
	// 内网/回环/链路本地/未指定 → 放宽；其余（公网 IP）→ 严格
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return !ip.IsGlobalUnicast()
}

// companyTransport 连"自建服务端 / 官方 API"的统一 Transport：按目标主机决定是否
// 校验证书（relaxedTLSFor），其余参数与 http.DefaultTransport 一致（连接可共享）。
var companyTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second},
			Config:    &tls.Config{ServerName: host, InsecureSkipVerify: relaxedTLSFor(host)},
		}
		return d.DialContext(ctx, network, addr)
	}
	return t
}()

// newCompanyHTTPClient 连公司自建服务端（向导/公告/市场/更新/模型/语音…）的
// http.Client：超时由调用方给，传输层用 companyTransport。
func newCompanyHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: companyTransport}
}

// companyTLSConfig 给 WSS/websocket 拨号用（/hub 中继注册、语音 ws）：是否免校验
// 同样按目标主机判定（公网 IP 严格）。
func companyTLSConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, InsecureSkipVerify: relaxedTLSFor(host)}
}
