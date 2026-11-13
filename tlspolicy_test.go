package main

// 证书策略（tlspolicy.go，2026-10-26 用户口径）：
//   域名            → 跳过校验（来源可信：打包内置/向导填写/公告下发）
//   内网/回环 IP    → 跳过校验（内网几乎不可能有公网证书，仍走 TLS 加密）
//   公网 IP         → 严格校验（广播发现可能被公网 IP 抢答，这是唯一不可信来源）
// 端到端：自签证书的 https（127.0.0.1）用 companyTransport 能连，用默认 client 连不上。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRelaxedTLSFor(t *testing.T) {
	cases := []struct {
		host string
		want bool
		why  string
	}{
		// 域名：一律免校验
		{"jxaizz.com", true, "公司公网域名"},
		{"jxkj.mycloudnas.com", true, "老域名"},
		{"api.deepseek.com", true, "第三方 API（按用户口径也放宽）"},
		{"open.bigmodel.cn", true, "第三方 API"},
		{"nas", true, "单标签内网名"},
		{"jxai-server.local", true, "mDNS 名"},
		// 内网 / 回环 / 链路本地 IP：免校验
		{"192.168.0.210", true, "内网（本次报错的地址）"},
		{"10.1.2.3", true, "10/8"},
		{"172.20.1.1", true, "172.16/12"},
		{"127.0.0.1", true, "回环"},
		{"169.254.10.10", true, "链路本地"},
		{"::1", true, "IPv6 回环"},
		{"fe80::1", true, "IPv6 链路本地"},
		{"fd00::1", true, "IPv6 唯一本地"},
		// 公网 IP：严格（这是唯一要防的场景）
		{"8.8.8.8", false, "公网 IPv4"},
		{"1.1.1.1", false, "公网 IPv4"},
		{"114.114.114.114", false, "公网 IPv4（国内 DNS）"},
		{"2001:4860:4860::8888", false, "公网 IPv6"},
		{"", false, "空主机名"},
	}
	for _, c := range cases {
		if got := relaxedTLSFor(c.host); got != c.want {
			t.Errorf("relaxedTLSFor(%q) = %v，期望 %v（%s）", c.host, got, c.want, c.why)
		}
	}
}

// 自签证书的服务端：companyTransport 应能连上（走 TLS，只是不验证书），
// 默认 client 必须失败——证明放宽确实生效、且不是"全局无视证书"。
func TestCompanyTransportAcceptsSelfSigned(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	// ① 默认 Transport：自签证书必须被拒
	if _, err := (&http.Client{Timeout: 5 * time.Second}).Get(srv.URL); err == nil {
		t.Fatal("默认 client 竟然信任了自签证书——说明环境被全局改过，测试无意义")
	} else if !strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("默认 client 失败原因不是证书问题：%v", err)
	}

	// ② companyTransport（127.0.0.1 = 回环 → 放宽）
	resp, err := newCompanyHTTPClient(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("内网/回环地址应免校验并连接成功，实际失败：%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("响应异常：%d %q", resp.StatusCode, body)
	}
}

// companyTLSConfig 是 WSS 拨号用的：判定口径必须与 http 侧完全一致，
// 否则会出现"向导能进、中继连不上"这类割裂。
func TestCompanyTLSConfigMatchesPolicy(t *testing.T) {
	for _, host := range []string{"192.168.0.210", "jxaizz.com", "8.8.8.8"} {
		cfg := companyTLSConfig(host)
		if cfg.InsecureSkipVerify != relaxedTLSFor(host) {
			t.Fatalf("%s：TLSConfig.InsecureSkipVerify=%v 与 relaxedTLSFor=%v 不一致",
				host, cfg.InsecureSkipVerify, relaxedTLSFor(host))
		}
		if cfg.ServerName != host {
			t.Fatalf("%s：ServerName 应为 %s，实际 %s", host, host, cfg.ServerName)
		}
	}
}
