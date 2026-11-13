// portalproxy.go —— 个人中心（充值门户）内嵌半边。
//
// 设计意图（2026-11-13 充值组件）：个人中心必须**在应用内打开**（充值组件的
// 浮窗 iframe），不能跳系统浏览器。iframe 直连服务端 HTTPS 会被浏览器证书
// 校验静默拒绝（自签证书连"继续访问"都没有）——与小程序平台（sitesproxy.go）
// 同一问题、同一解法：本端做透明反向代理，iframe 加载本地同源
// http://127.0.0.1:<port>/portal，证书由 companyTransport（tlspolicy.go）接管。
//
// 路径零映射：服务端门户页自身就用 /portal 前缀（首页 /portal，API 全在
// /portal/api/*，实测 2026-11-13），本端挂同名路径原样转发即可；门户引用的
// /sites-assets/qr.js 已由 sites 代理覆盖（同一份文件）。门户登录走 cookie
//（same-origin fetch），代理透传 Cookie/Set-Cookie，登录态在浮窗内可用。
//
// 目标 = Cfg.PortalURL（服务端 portal 启用时随公告/provision 下发；空=门户
// 没开，代理回提示文本——此时充值组件的三处入口本就隐藏，正常到不了这里）。
package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// portalProxyTarget 门户代理目标（Cfg.PortalURL）；没配置或不合法返回 nil。
func (a *App) portalProxyTarget() *url.URL {
	raw := strings.TrimSpace(a.Cfg.PortalURL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}
}

// handlePortalProxy 透明转发到门户服务端：路径/方法/body/cookie 原样，Transport
// 用 companyTransport（复用市场/平台同款证书策略）。Origin/Referer 改写成目标源
//（与 sitesproxy 同理：服务端若有同源校验，本机 Origin 必然对不上）。
func (a *App) handlePortalProxy(w http.ResponseWriter, r *http.Request) {
	target := a.portalProxyTarget()
	if target == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("服务端未开启个人中心（门户）：入口已隐藏；若刚开启，到 设置·模型 公司通道卡片点「刷新额度」重拉公告。"))
		return
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			if o := req.Header.Get("Origin"); o != "" {
				req.Header.Set("Origin", target.Scheme+"://"+target.Host)
			}
			if ref := req.Header.Get("Referer"); ref != "" {
				req.Header.Set("Referer", target.Scheme+"://"+target.Host+req.URL.Path)
			}
		},
		Transport: companyTransport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("个人中心连不上服务端（" + target.Host + "）：" + err.Error()))
		},
	}
	proxy.ServeHTTP(w, r)
}
