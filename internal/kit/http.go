package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
)

var ExecCommand = exec.Command

// proxyRR 是 Cline 通道代理列表的轮询计数器(仅 kit.HTTPClient 使用)。
var proxyRR atomic.Uint64

// httpMu 保护 sharedClient 及其生效中的代理配置。
var (
	httpMu         sync.Mutex
	activeProxies  []string
	activeStrategy string
	sharedClient   = &http.Client{Transport: buildProxyTransport(nil, "")}
)

// HTTPClient 返回 Cline 上游(及 admin / model_metadata / zen_protocols / proxy
// 等公共出站)共用的 HTTP 客户端。其 transport 由 RebuildHTTPClient 在代理配置
// 变更时线程安全地整体替换。
//
// 出站优先级:config 代理列表(非空) > HTTP_PROXY/HTTPS_PROXY 环境变量 > 直连。
// https 流量统一套 uTLS Chrome 120 指纹(ALPN 只协商 http/1.1),即便穿越代理仍
// 保留指纹以规避 CF 风控;SSE 流式走 HTTP/1.1 chunked 最稳,且不依赖 h2-only 的
// http2.Transport —— 后者在穿越不稳定网络路径时会出现 200 后 body 不出数据、
// 客户端报 "stream ended without terminal event" 的问题。
func HTTPClient() *http.Client {
	httpMu.Lock()
	defer httpMu.Unlock()
	return sharedClient
}

// RebuildHTTPClient 以新的代理列表+策略重建 Cline 通道 transport。
// proxies 为空时回落到 HTTP_PROXY/HTTPS_PROXY 环境变量,再为空则直连。
func RebuildHTTPClient(proxies []string, strategy string) {
	httpMu.Lock()
	defer httpMu.Unlock()
	activeProxies = proxies
	activeStrategy = strategy
	sharedClient = &http.Client{Transport: buildProxyTransport(proxies, strategy)}
}

// NewProxiedClient 返回一个带超时、复用当前 Cline 通道代理配置的一次性客户端,
// 适用于模型同步等短请求。配置取当前快照,不随后续 RebuildHTTPClient 热更新。
func NewProxiedClient(timeout time.Duration) *http.Client {
	httpMu.Lock()
	proxies, strategy := activeProxies, activeStrategy
	httpMu.Unlock()
	return &http.Client{
		Timeout:   timeout,
		Transport: buildProxyTransport(proxies, strategy),
	}
}

// pickProxy 按策略从列表选一个代理;列表为空返回 ""。
func pickProxy(proxies []string, strategy string) string {
	n := len(proxies)
	if n == 0 {
		return ""
	}
	switch strategy {
	case "random":
		return proxies[int(time.Now().UnixNano()%int64(n))]
	case "fill":
		return proxies[0]
	default: // round_robin
		return proxies[int(proxyRR.Add(1)-1)%n]
	}
}

// dialConn 是 Cline 通道统一的底层拨号:config 代理 > 环境变量代理 > 直连。
func dialConn(ctx context.Context, network, addr string, proxies []string, strategy string) (net.Conn, error) {
	if p := pickProxy(proxies, strategy); p != "" {
		return DialViaProxy(ctx, p, network, addr)
	}
	if p := EnvProxyURL(addr); p != "" {
		return DialViaProxy(ctx, p, network, addr)
	}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// buildProxyTransport 构造一个代理感知的 http.Transport。
// 明文 http 走 DialContext;https 走 DialTLSContext —— 先经(可选)代理拨到目标,
// 再在隧道上做 uTLS 握手,保证穿越代理仍保留 Chrome 指纹。
func buildProxyTransport(proxies []string, strategy string) *http.Transport {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialConn(ctx, network, addr, proxies, strategy)
	}
	return &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   false,
		DialContext: dial,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return utlsHandshake(ctx, raw, addr)
		},
	}
}

// utlsHandshake 在已建立的 raw 连接(可能是经代理的 CONNECT 隧道)上做 uTLS
// Chrome 120 握手。Chrome 预设自带 ALPN(h2+http/1.1),Config.NextProtos 会被
// 预设覆盖;这里取出 spec 把 ALPN 改成只 http/1.1,再以 HelloCustom 应用,确保
// 协商 HTTP/1.1,避免标准 http.Transport 把 h2 SETTINGS 帧当乱码报
// "malformed HTTP response"。
func utlsHandshake(ctx context.Context, raw net.Conn, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		raw.Close()
		return nil, err
	}
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_120)
	if err != nil {
		raw.Close()
		return nil, err
	}
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
		}
	}
	uconn := utls.UClient(raw, &utls.Config{ServerName: host}, utls.HelloCustom)
	if err := uconn.ApplyPreset(&spec); err != nil {
		raw.Close()
		return nil, err
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return uconn, nil
}

func HTTPPostForm(rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return HTTPClient().Do(req)
}

func HTTPPostJSON(rawURL string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", rawURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return HTTPClient().Do(req)
}

func ReadBody(resp *http.Response) string {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	return string(data)
}

func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func RunCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}
