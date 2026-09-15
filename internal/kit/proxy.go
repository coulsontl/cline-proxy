package kit

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// EnvProxyURL 返回拨向 addr(host:port) 时应使用的代理 URL,读取
// HTTPS_PROXY/HTTP_PROXY/NO_PROXY(及小写形式),复用 stdlib http.ProxyFromEnv
// 的全部语义。无代理适用(或命中 NO_PROXY)时返回 ""。
//
// 端口 80 视为明文 http(读 HTTP_PROXY),其余视为 https(读 HTTPS_PROXY)。
//
// 注意:stdlib http.ProxyFromEnvironment 对 https 严格使用 HTTPS_PROXY、不回退
// HTTP_PROXY;且其结果经 sync.Once 缓存,固化于进程首次调用时的环境(容器内
// env 启动即固定,符合预期)。为兼容"只设 HTTP_PROXY 也期望 https 走代理"的
// 常见用法,在未设置 NO_PROXY(nil 必是"对应 scheme 变量为空"而非 no_proxy
// 命中)时做一次宽松回退:ALL_PROXY > 对侧 scheme 变量。
func EnvProxyURL(addr string) string {
	scheme := "https"
	if _, port, err := net.SplitHostPort(addr); err == nil && port == "80" {
		scheme = "http"
	}
	req := &http.Request{URL: &url.URL{Scheme: scheme, Host: addr}, Host: addr}
	if p, err := http.ProxyFromEnvironment(req); err == nil && p != nil {
		return p.String()
	}
	if os.Getenv("NO_PROXY") == "" && os.Getenv("no_proxy") == "" {
		fallback := []string{"ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy"}
		if scheme == "https" {
			fallback = []string{"ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}
		}
		for _, n := range fallback {
			if v := strings.TrimSpace(os.Getenv(n)); v != "" {
				return v
			}
		}
	}
	return ""
}

// DialViaProxy 经指定代理建立到 addr 的 TCP 连接(原始 socket,不做 TLS 握手)。
// 支持 http/https 代理(CONNECT 隧道)与 socks5/socks5h。
func DialViaProxy(ctx context.Context, raw, network, addr string) (net.Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad proxy url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return dialHTTPProxy(ctx, u, network, addr)
	case "socks5", "socks5h":
		auth := &proxy.Auth{}
		if u.User != nil {
			auth.User = u.User.Username()
			auth.Password, _ = u.User.Password()
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		type ctxDialer interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}
		if cd, ok := d.(ctxDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		// 旧接口无 ctx:包装
		type result struct {
			c   net.Conn
			err error
		}
		ch := make(chan result, 1)
		go func() {
			c, err := d.Dial(network, addr)
			ch <- result{c, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			return r.c, r.err
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

// dialHTTPProxy 通过 http(s) 代理建立 CONNECT 隧道。
func dialHTTPProxy(ctx context.Context, u *url.URL, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	rawConn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		tlsConn := tls.Client(rawConn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		rawConn = tlsConn
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u.User != nil {
		cred := base64.StdEncoding.EncodeToString([]byte(u.User.String()))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(rawConn); err != nil {
		rawConn.Close()
		return nil, err
	}

	br := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		rawConn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rawConn.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("proxy CONNECT %s: %s %s", u.Host, resp.Status, strings.TrimSpace(string(b)))
	}
	return rawConn, nil
}
