package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	neturl "net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestP1F20SecretsAndBudgets(t *testing.T) {
	withNoProxy(t)
	raw := "https://secret-user:secret-pass@api.example.test:8443/path-token?key=query-token"
	req, _ := http.NewRequestWithContext(context.Background(), "POST", raw, nil)
	err := &neturl.Error{Op: "Post", URL: raw, Err: errors.New("dial tcp: connection refused")}
	cf := newConnFailure(req, urlHost(raw), time.Now(), err)
	msg := cf.Error()
	t.Log(msg)
	for _, secret := range []string{"secret-user", "secret-pass", "path-token", "query-token"} {
		if strings.Contains(msg, secret) {
			t.Errorf("secret leaked: %s", secret)
		}
	}
	for _, want := range []string{"api.example.test:8443", "connection refused", "未经代理", "耗时"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q", want)
		}
	}
	cf.target = strings.Repeat("h", 500) + ":8443"
	cf.err = errors.New(strings.Repeat("reason ", 200))
	cf.proxy = "经代理 https://" + strings.Repeat("p", 500)
	msg = cf.Error()
	if !strings.Contains(msg, "耗时") || !strings.Contains(msg, "经代理") || utf8.RuneCountInString(msg) > 300 {
		t.Errorf("field budget lost: %s", msg)
	}
}

func TestP1F21DeliveredPOST(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 0)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if hits.Add(1) == 1 {
			c, b, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprint(b, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n")
			b.Flush()
			c.Close()
			return
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	withStreamHTTP(t, srv.Client())
	err := StreamChat(context.Background(), connTestTarget("", srv.URL, srv.URL), connTestReq(), StreamCallbacks{})
	t.Logf("hits=%d err=%v", hits.Load(), err)
	if hits.Load() != 1 {
		t.Errorf("hits=%d want=1", hits.Load())
	}
	if err == nil || !strings.Contains(err.Error(), "请求体已完整写出") {
		t.Errorf("missing stage error: %v", err)
	}
	if isConnFailure(err) || isRetryableModelErr(err) {
		t.Error("delivered request must not retry or fail over")
	}
}

type p1RoundTripFunc func(*http.Request) (*http.Response, error)

func (f p1RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 传输跟踪报告写出失败时仍保留原阶梯，不把部分写出误当作完整提交。
func TestP1F21PartialWriteStillRetries(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 0)
	n := 0
	withStreamHTTP(t, &http.Client{Transport: p1RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		httptrace.ContextClientTrace(r.Context()).WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("broken pipe")})
		return nil, errors.New("broken pipe")
	})})
	err := StreamChat(context.Background(), connTestTarget("", "http://partial.test", ""), connTestReq(), StreamCallbacks{})
	if n != 2 || !isConnFailure(err) {
		t.Fatalf("partial write: attempts=%d err=%v", n, err)
	}
}

func TestP1F22MixedLadders(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 0, 0, 0, 0, 0)
	withConnRetryTimeoutDelays(t, 0, 0)
	withConnRetryTimeoutBudget(t, time.Hour)
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	n := 0
	withStreamHTTP(t, &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		n++
		if n <= 2 {
			return nil, errors.New("connection refused")
		}
		return nil, errors.New("i/o timeout")
	}}})
	err := StreamChat(context.Background(), connTestTarget("", "http://mixed.test", ""), connTestReq(), StreamCallbacks{})
	t.Logf("attempts=%d err=%v logs=\n%s", n, err, buf.String())
	if n != 5 {
		t.Errorf("attempts=%d want=5: timeout ladder must start at zero", n)
	}
	if !strings.Contains(buf.String(), "第 1/2 次重试") {
		t.Error("timeout level zero skipped")
	}
	if n > connRetryMaxAttempts {
		t.Error("total limit exceeded")
	}
}

func TestP1F22BoundAcrossAddresses(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 0, 0, 0, 0, 0)
	withConnRetryTimeoutDelays(t, 0, 0)
	n := 0
	withStreamHTTP(t, &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		n++
		if (n-1)%8 >= 5 && (n-1)%8 < 7 {
			return nil, errors.New("i/o timeout")
		}
		return nil, errors.New("connection refused")
	}}})
	err := StreamChat(context.Background(), connTestTarget("", "http://primary.test", "http://alt.test"), connTestReq(), StreamCallbacks{})
	t.Logf("attempts=%d err=%v", n, err)
	if n != connRetryMaxAttempts {
		t.Errorf("unexpected total attempts %d", n)
	}
}

func TestP1F22HardCapWithLongLadders(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, make([]time.Duration, 30)...)
	client, dials := refusedTransport(t, "connection refused")
	withStreamHTTP(t, client)
	err := StreamChat(context.Background(), connTestTarget("", "http://primary.test", "http://alt.test"), connTestReq(), StreamCallbacks{})
	if got := atomic.LoadInt32(dials); got != connRetryMaxAttempts {
		t.Errorf("attempts=%d err=%v", got, err)
	}
}
