package app

import (
	"cline-go-proxy/internal/kit"
	"fmt"
	"os"
	"strconv"
	"sync"
)

// 日志轮转：cline-proxy.log 只追加不轮转会一直涨（容器里跑几个月就是几个 G）。
// 按大小轮转成 cline-proxy.log.1/.2/...，超过 keep 份的最旧文件删掉。
// 可用环境变量覆盖：LOG_MAX_MB（默认 10）、LOG_KEEP（默认 5）。
const (
	defaultLogMaxBytes = 10 << 20
	defaultLogKeep     = 5
)

// rotatingWriter 是一个按大小自我轮转的日志写入器（log.SetOutput 的 sink）。
// 写失败只返回错误，不 panic：日志系统本身不能把代理带崩。
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func newRotatingWriter(path string, maxBytes int64, keep int) (*rotatingWriter, error) {
	if maxBytes <= 0 {
		maxBytes = defaultLogMaxBytes
	}
	if keep <= 0 {
		keep = defaultLogKeep
	}
	w := &rotatingWriter{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// logSizeSettings 读取环境变量覆盖（0/非法值回落默认）。
func logSizeSettings() (int64, int) {
	maxMB := 0
	if v, err := strconv.Atoi(os.Getenv("LOG_MAX_MB")); err == nil {
		maxMB = v
	}
	keep := 0
	if v, err := strconv.Atoi(os.Getenv("LOG_KEEP")); err == nil {
		keep = v
	}
	maxBytes := int64(defaultLogMaxBytes)
	if maxMB > 0 {
		maxBytes = int64(maxMB) << 20
	}
	if keep <= 0 {
		keep = defaultLogKeep
	}
	return maxBytes, keep
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	size := int64(0)
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	w.f = f
	w.size = size
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, fmt.Errorf("log file not open")
	}
	if w.size+int64(len(p)) > w.maxBytes {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 关闭底层文件（进程退出/测试清理时用）。
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// rotate 调用方必须持有锁：关闭当前文件，.N → .N+1，当前文件 → .1，然后重新打开。
func (w *rotatingWriter) rotate() {
	_ = w.f.Close()
	for i := w.keep - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", w.path, i)
		next := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(old); err == nil {
			_ = os.Rename(old, next)
		}
	}
	_ = os.Rename(w.path, w.path+".1")
	// 超过 keep 份的最旧文件清掉
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.keep+1))
	if err := w.open(); err != nil {
		w.f = nil
		w.size = 0
	}
}

// openStreamLog 打开 Anthropic 流的逐事件调试日志（排查用，内容就是原始 SSE）。
// 每个流式请求都会追加，所以打开时按大小轮转一次，只保留一份历史。
func openStreamLog() *os.File {
	path := kit.ResolveDataPath("cline-proxy-stream.log")
	const maxBytes = 64 << 20
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxBytes {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil
	}
	return f
}