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
	"time"

	utls "github.com/refraction-networking/utls"
)

var ExecCommand = exec.Command

// HTTPTransport 对 https 流量套 uTLS Chrome 120 指纹，规避 Go 原生 TLS 指纹被上游风控/重置。
// 通过标准 http.Transport 的 DialTLSContext 注入 uTLS 连接，并只协商 http/1.1：
// SSE 流式走 HTTP/1.1 chunked 最稳，且不依赖 h2-only 的 http2.Transport，
// 后者在穿越不稳定网络路径时会出现 200 后 body 不出数据、客户端报
// "stream ended without terminal event" 的问题。
var HTTPTransport = newUTLSTransport()

func newUTLSTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   false,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
			raw, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				raw.Close()
				return nil, err
			}
			// Chrome 预设自带 ALPN(h2+http/1.1)，Config.NextProtos 会被预设覆盖、
			// 放任协商出 h2 时标准 http.Transport 不讲 h2，会把 h2 SETTINGS 帧当
			// 乱码报 "malformed HTTP response"。这里取出 Chrome spec，把 ALPN 改成
			// 只 http/1.1，再以 HelloCustom 应用，确保协商 HTTP/1.1，SSE chunked 最稳。
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
		},
	}
	return t
}

var HTTPClient = &http.Client{
	Transport: HTTPTransport,
}

func HTTPPostForm(rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return HTTPClient.Do(req)
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
	return HTTPClient.Do(req)
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
