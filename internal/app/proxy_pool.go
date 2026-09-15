package app

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"cline-go-proxy/internal/kit"

	utls "github.com/refraction-networking/utls"
)

var (
	zenHTTPClient  = &http.Client{Transport: buildZenTransport()}
	zenProxyCount  atomic.Uint64
	zenTransportMu sync.Mutex

	zenProxyCooldowns   = map[int]time.Time{} // 代理索引 -> 冷却截止
	zenProxyCooldownsMu sync.Mutex
)

// cooldownZenProxy 标记某出口代理冷却,冷却期内轮询跳过
func cooldownZenProxy(idx int, d time.Duration) {
	if idx < 0 {
		return
	}
	if d <= 0 {
		d = 10 * time.Minute
	}
	zenProxyCooldownsMu.Lock()
	zenProxyCooldowns[idx] = time.Now().Add(d)
	zenProxyCooldownsMu.Unlock()
}

func zenProxyAvailable(idx int) bool {
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	until, ok := zenProxyCooldowns[idx]
	if !ok {
		return true
	}
	if time.Now().After(until) {
		delete(zenProxyCooldowns, idx)
		return true
	}
	return false
}

func zenProxyCooldownStatus() map[string]string {
	cfg := getZenConfig()
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	out := map[string]string{}
	for idx, until := range zenProxyCooldowns {
		if idx >= 0 && idx < len(cfg.Proxies) {
			if time.Now().Before(until) {
				out[cfg.Proxies[idx]] = until.Format("15:04:05")
			}
		}
	}
	return out
}

// rebuildZenTransport 代理池或配置变化时重建 zen 上游 HTTP 客户端
func rebuildZenTransport() {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	zenHTTPClient = &http.Client{Transport: buildZenTransport()}
}

func getZenHTTPClient() *http.Client {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	return zenHTTPClient
}

// pickZenProxy 按策略选择代理,返回 (代理URL, 索引);无代理返回 ("", -1)。
// 跳过冷却中的代理;全部冷却时返回最早恢复的近似(轮询位)。
// 每次调用递增计数,保证 round_robin 顺序与日志索引一致。
func pickZenProxy() (string, int) {
	cfg := getZenConfig()
	n := len(cfg.Proxies)
	if n == 0 {
		return "", -1
	}
	idx := int(zenProxyCount.Add(1)-1) % n
	switch cfg.ProxyStrategy {
	case "random":
		idx = int(time.Now().UnixNano() % int64(n))
	case "fill":
		idx = 0
	}
	// 冷却跳过:线性探测下一个可用代理
	for i := 0; i < n; i++ {
		if zenProxyAvailable(idx) {
			break
		}
		idx = (idx + 1) % n
	}
	return cfg.Proxies[idx], idx
}

// lastZenProxyIdx 最近一次选择的代理索引(日志用)
func lastZenProxyIdx() int {
	v := int64(zenProxyCount.Load())
	if v <= 0 {
		return -1
	}
	return int((v - 1) % int64(max(1, len(getZenConfig().Proxies))))
}

func maskProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("***")
	return u.String()
}

func buildZenTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   false,
		DialContext:         zenDialContext,
		// https 走 uTLS Chrome 指纹(规避 Go 原生指纹被 CF 风控)，ALPN 只协商 http/1.1：
		// 与 cline 上游同理，http2.Transport(h2-only) 在流式场景会出现 200 后 body 不出数据。
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := zenDialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
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
		},
	}
	return t
}

func zenDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	// 优先级:zen config 代理池 > HTTP_PROXY/HTTPS_PROXY 环境变量 > 直连
	if p, _ := pickZenProxy(); p != "" {
		return kit.DialViaProxy(ctx, p, network, addr)
	}
	if envP := kit.EnvProxyURL(addr); envP != "" {
		return kit.DialViaProxy(ctx, envP, network, addr)
	}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}
