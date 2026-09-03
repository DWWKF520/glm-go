package glmclient

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// QueueTimeoutError 队列等待超时错误
// 当请求在并发队列中等待时间超过配置的 GLM_QUEUE_WAIT_TIMEOUT_SECONDS 时返回
type QueueTimeoutError struct {
	msg string
}

// Error 实现 error 接口
func (e *QueueTimeoutError) Error() string { return e.msg }

// QueueLease 队列租约，代表一个请求占用了队列中的一个执行槽位
// 使用租约模式确保请求完成后正确释放槽位
type QueueLease struct {
	ticket          int       // 分配的票据号（递增整数）
	releaseCallback func(int) // 释放时的回调函数
	released        bool      // 是否已释放（防止重复释放）
}

// Release 释放租约，将执行槽位归还给队列
func (l *QueueLease) Release() {
	if l.released {
		return
	}
	l.released = true
	l.releaseCallback(l.ticket)
}

// ConcurrentRequestQueue 并发请求队列
// 通过票据机制控制同时向 GLM 发送的请求数量，实现请求排队和限流
//
// 原理：
//   - 每个请求获取一个递增的 ticket（票据号）
//   - 只有当 ticket - servingTicket < maxConcurrency 时，请求才能执行
//   - 请求完成后通过 release 释放槽位，servingTicket 前进
//   - 超过 waitTimeout 仍未获得槽位的请求返回 QueueTimeoutError
type ConcurrentRequestQueue struct {
	logger          *slog.Logger  // 日志记录器
	waitTimeout     time.Duration // 最大等待超时时间
	maxConcurrency  int           // 最大并发数（GLM 同时处理的对话数）
	mu              *sync.Cond    // 条件变量，用于请求等待/唤醒
	nextTicket      int           // 下一个将分配的票据号
	servingTicket   int           // 当前正在服务的最小票据号
	releasedTickets map[int]bool  // 已释放但尚未推进 servingTicket 的票据
}

// NewConcurrentRequestQueue 创建并发请求队列
// logger: 日志记录器
// waitTimeout: 请求最大等待时间
// maxConcurrency: 最大并发执行数
func NewConcurrentRequestQueue(logger *slog.Logger, waitTimeout time.Duration, maxConcurrency int) *ConcurrentRequestQueue {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	q := &ConcurrentRequestQueue{
		logger:          logger,
		waitTimeout:     waitTimeout,
		maxConcurrency:  maxConcurrency,
		releasedTickets: map[int]bool{},
	}
	q.mu = sync.NewCond(&sync.Mutex{})
	return q
}

// MaxConcurrency 返回最大并发数
func (q *ConcurrentRequestQueue) MaxConcurrency() int {
	return q.maxConcurrency
}

// Acquire 获取队列租约，阻塞直到获得执行槽位或超时
// requestName: 请求描述（用于日志）
// 返回值：
//   - *QueueLease: 租约对象，使用完毕后必须调用 Release()
//   - error: 超时返回 *QueueTimeoutError
func (q *ConcurrentRequestQueue) Acquire(requestName string) (*QueueLease, error) {
	q.mu.L.Lock()
	defer q.mu.L.Unlock()

	ticket := q.nextTicket
	q.nextTicket++
	// 计算队列前方等待的请求数量
	queueAhead := ticket - (q.servingTicket + q.maxConcurrency) + 1
	if queueAhead < 0 {
		queueAhead = 0
	}
	start := time.Now()

	if queueAhead > 0 {
		q.logger.Info("请求进入 GLM 队列", "ticket", ticket, "ahead", queueAhead, "request", requestName)
	}

	// 等待直到 ticket 落入并发窗口内
	for ticket >= q.servingTicket+q.maxConcurrency {
		remaining := q.waitTimeout - time.Since(start)
		if remaining <= 0 {
			return nil, &QueueTimeoutError{
				msg: fmt.Sprintf("GLM 队列等待超时，前方仍有 %d 个请求，请稍后重试。", ticket-(q.servingTicket+q.maxConcurrency)+1),
			}
		}
		// sync.Cond 不支持带超时的 Wait，使用 goroutine + 定时器模拟
		done := make(chan struct{})
		go func() {
			time.Sleep(remaining)
			close(done)
			q.mu.Broadcast() // 超时后唤醒所有等待者
		}()
		q.mu.Wait()
		select {
		case <-done:
		default:
		}
	}

	activeSlots := ticket - q.servingTicket + 1
	q.logger.Info("请求获得 GLM 执行槽位", "ticket", ticket, "active", activeSlots, "max", q.maxConcurrency, "request", requestName)
	return &QueueLease{ticket: ticket, releaseCallback: q.release}, nil
}

// release 释放指定票据对应的执行槽位
// 通过遍历已释放票据，推进 servingTicket 到下一个连续未释放的位置
func (q *ConcurrentRequestQueue) release(ticket int) {
	q.mu.L.Lock()
	defer q.mu.L.Unlock()
	q.releasedTickets[ticket] = true
	// 推进 servingTicket：跳过所有已释放的连续票据
	for q.releasedTickets[q.servingTicket] {
		delete(q.releasedTickets, q.servingTicket)
		q.servingTicket++
	}
	q.logger.Info("请求离开 GLM 执行槽位", "ticket", ticket)
	q.mu.Broadcast() // 唤醒等待中的请求
}
