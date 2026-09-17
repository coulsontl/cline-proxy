package app

import (
	"bytes"
	"log"
	"net/http"
	"sync"
	"time"
)

// ============================================================================
// 提交前缓冲（pre-commit buffering）
//
// 所有 SSE 出口都要面对同一件事：上游空回/中途断流时，如果已经给客户端发了
// 200 + 半截流，就再也改不了（客户端只会报 "stream ended without terminal event"）。
// 所以先只往内存里写，等到「出现有意义内容」再一次性提交（下发 header + 回放缓冲），
// 之后就是普通透传。提交前发现空回/断流 → 整段丢弃，换号重试或干净地返回 502。
//
// commitGate 就是这个"内存阶段"的 ResponseWriter：SSE 处理器完全不用改写法。
// ============================================================================

// commitGate 是提交前的内存 ResponseWriter：Write 只进缓冲，Commit 时才把
// header + 缓冲一次性下发并转为透传，Discard 则整段作废（供换号重试）。
type commitGate struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	flusher   http.Flusher
	hdr       http.Header
	buf       bytes.Buffer
	status    int
	committed bool
	finished  bool // 已提交或已丢弃：之后的写入不再缓冲/不再产生副作用
	discarded bool
	timeout   time.Duration
	timer     *time.Timer
}

// newCommitGate 创建提交门。timeout > 0 时，超过该时长还没提交就自动提交
// （客户端先用上 header，代价是这次请求不能再换号重试）。
//
// headers 在构造时给定、之后只由 Commit 读取：map 不是线程安全的，而超时兜底跑在
// 另一个 goroutine 里，若允许处理器事后随意改这张 map，就会与 timer 产生数据竞争。
func newCommitGate(w http.ResponseWriter, timeout time.Duration, headers map[string]string) *commitGate {
	f, _ := w.(http.Flusher)
	g := &commitGate{w: w, flusher: f, hdr: http.Header{}, status: http.StatusOK, timeout: timeout}
	for k, v := range headers {
		g.hdr.Set(k, v)
	}
	if timeout > 0 {
		// 赋值必须持锁：定时器可能立刻就触发，而 Commit/Discard 都会在锁内读 g.timer
		g.mu.Lock()
		g.timer = time.AfterFunc(timeout, func() {
			g.mu.Lock()
			pending := !g.committed && !g.discarded
			g.mu.Unlock()
			if pending {
				log.Printf("  pre-commit timeout (%v), committing without retry", timeout)
				g.Commit()
			}
		})
		g.mu.Unlock()
	}
	return g
}

// Header 返回的是门自己的 map：仅在 Commit 时被读取，处理器不要在提交前后改它
// （需要额外 header 请通过 newCommitGate 的 headers 参数传入）。
func (g *commitGate) Header() http.Header {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.committed {
		return g.w.Header()
	}
	return g.hdr
}

func (g *commitGate) WriteHeader(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.finished {
		return
	}
	g.status = status
}

func (g *commitGate) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.discarded {
		return len(p), nil // 这次尝试已作废，写进来的字节直接丢掉
	}
	if g.committed {
		return g.w.Write(p)
	}
	return g.buf.Write(p)
}

// Flush 在提交前是空操作（还没给客户端发 header），提交后透传。
func (g *commitGate) Flush() {
	g.mu.Lock()
	committed, flusher := g.committed, g.flusher
	g.mu.Unlock()
	if committed && flusher != nil {
		flusher.Flush()
	}
}

// Commit 下发 header 与已缓冲的字节，之后转为透传。幂等。
//
// 全程持锁：超时兜底是在另一个 goroutine 里触发的，真实写入必须与读循环里的
// Write 串行化（否则 SSE 字节会交错，-race 也会报 ResponseWriter 数据竞争）。
func (g *commitGate) Commit() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.finished {
		return
	}
	g.committed = true
	g.finished = true
	if g.timer != nil {
		g.timer.Stop()
	}
	status := g.status
	if status == 0 {
		status = http.StatusOK
	}
	dst := g.w.Header()
	for k, vv := range g.hdr {
		dst[k] = vv
	}
	g.w.WriteHeader(status)
	if g.buf.Len() > 0 {
		_, _ = g.w.Write(g.buf.Bytes())
	}
	g.buf = bytes.Buffer{}
	if g.flusher != nil {
		g.flusher.Flush()
	}
}

// Discard 丢弃缓冲：这次尝试的字节一个都不下发，调用方可以换号重试。
func (g *commitGate) Discard() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.committed {
		return // 已经发给客户端了，收不回来
	}
	g.discarded = true
	g.finished = true
	if g.timer != nil {
		g.timer.Stop()
	}
	g.buf = bytes.Buffer{}
}

func (g *commitGate) Committed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.committed
}

// BufferedLen 是提交前已缓冲的字节数（用于缓冲上限判定）。
func (g *commitGate) BufferedLen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Len()
}

// emptyCommittedMessage 是"提交前超时兜底先发了 200，结果上游一路没内容"的记账口径。
func emptyCommittedMessage() string {
	return "empty response detected (committed by pre-commit timeout, status already sent)"
}

// convertedStreamResult 是"边转换边转发"出口（/v1/messages、/v1/responses）的结果：
// 转换层自己知道内容什么时候出现、上游什么时候断，所以由它回报，调用方决定记账与重试。
type convertedStreamResult struct {
	outcome streamOutcome // streamCommitted / streamEmpty / streamFatal
	aborted error         // 非 nil 表示提交之后上游断流（客户端拿到截断流 + error 事件）
	usage   tokenUsage
	// sawContent 表示整条流里出现过有意义内容。为 false 而 outcome 又是 streamCommitted
	// 只有一种情况：提交前超时兜底把 header 先发了出去，随后上游其实什么都没给。
	// 这时客户端改不了状态码，但统计绝不能记成功（否则就是查不到的静默空答）。
	sawContent bool
}
