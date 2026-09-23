package engine

import (
	"container/heap"
	"errors"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 调度队列
//
// 背景：原先使用 chan string 做队列，出队顺序等于入队顺序（FIFO），
// TaskRun.Priority 只落库、从未参与调度，高优任务（线上故障）会被低优任务阻塞。
//
// 本实现提供优先级出队：
//  1. Priority 越大越优先（domain 约定 0-9）；
//  2. 同优先级按入队先后（seq 单调递增）保证 FIFO，避免同级别乱序；
//  3. 老化（aging）：等待越久有效优先级越高，防止持续高优流量把低优任务饿死。
// ---------------------------------------------------------------------------

var (
	// ErrQueueFull 队列已满（调用方据此映射 429 + Retry-After）。
	ErrQueueFull = errors.New("任务队列已满")
	// ErrQueueClosed 队列已关闭（引擎停止，不再受理）。
	ErrQueueClosed = errors.New("任务队列已关闭")
)

const (
	// agingInterval 每等待这么久，有效优先级 +1。
	agingInterval = 30 * time.Second
	// maxAgingBoost 老化加成上限：与 Priority 取值域（0-9）同宽，
	// 保证"等待足够久"的低优任务最终能追上最高优先级的任务。
	maxAgingBoost = 9
	// priorityMin / priorityMax 提交优先级的合法区间，越界值会被夹紧。
	priorityMin = 0
	priorityMax = 9
)

// clampPriority 把提交优先级夹紧到 0-9（负值或异常大值不得破坏排序）。
func clampPriority(p int) int {
	if p < priorityMin {
		return priorityMin
	}
	if p > priorityMax {
		return priorityMax
	}
	return p
}

// queueItem 一个排队中的运行。
type queueItem struct {
	runID      string
	priority   int
	seq        uint64
	enqueuedAt time.Time
}

// effectivePriority 返回参与比较的有效优先级：基础优先级 + 老化加成。
func (it *queueItem) effectivePriority(now time.Time) int {
	boost := int(now.Sub(it.enqueuedAt) / agingInterval)
	if boost < 0 {
		boost = 0
	}
	if boost > maxAgingBoost {
		boost = maxAgingBoost
	}
	return it.priority + boost
}

// itemHeap 优先级堆：有效优先级降序，同级 seq 升序（FIFO）。
type itemHeap struct {
	items []*queueItem
	now   time.Time
}

func (h *itemHeap) Len() int { return len(h.items) }

func (h *itemHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *itemHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	pa, pb := a.effectivePriority(h.now), b.effectivePriority(h.now)
	if pa != pb {
		return pa > pb
	}
	return a.seq < b.seq
}

func (h *itemHeap) Push(x any) { h.items = append(h.items, x.(*queueItem)) }

func (h *itemHeap) Pop() any {
	old := h.items
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	h.items = old[:n-1]
	return it
}

// priorityQueue 并发安全的优先级阻塞队列。
type priorityQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	items    []*queueItem
	capacity int
	seq      uint64
	closed   bool
	// journal 可选的入队/出队日志：用于进程重启后恢复排队任务。
	journal *queueJournal
}

// newPriorityQueue 创建队列，capacity<=0 表示无界。
func newPriorityQueue(capacity int) *priorityQueue {
	q := &priorityQueue{capacity: capacity}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// SetJournal 绑定队列日志（必须在任何入队之前调用）。
func (q *priorityQueue) SetJournal(j *queueJournal) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.journal = j
}

// Capacity 返回队列容量（0 表示无界）。
func (q *priorityQueue) Capacity() int { return q.capacity }

// Len 返回当前排队数量。
func (q *priorityQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Push 入队。队列满或已关闭时返回错误，调用方据此拒绝受理。
func (q *priorityQueue) Push(runID string, priority int) error {
	return q.push(runID, priority, time.Now(), true)
}

// PushRestored 恢复入队：写日志由恢复流程统一负责，避免重复追加。
func (q *priorityQueue) PushRestored(runID string, priority int, enqueuedAt time.Time) error {
	return q.push(runID, priority, enqueuedAt, false)
}

func (q *priorityQueue) push(runID string, priority int, at time.Time, log bool) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrQueueClosed
	}
	if q.capacity > 0 && len(q.items) >= q.capacity {
		q.mu.Unlock()
		return ErrQueueFull
	}
	q.seq++
	it := &queueItem{runID: runID, priority: clampPriority(priority), seq: q.seq, enqueuedAt: at}
	h := &itemHeap{items: q.items, now: time.Now()}
	heap.Push(h, it)
	q.items = h.items
	journal := q.journal
	q.mu.Unlock()

	// 日志在锁外写：避免磁盘 IO 拉长持锁时间。
	if log && journal != nil {
		if err := journal.Enqueue(runID, it.priority, at); err != nil {
			return err
		}
	}
	q.cond.Signal()
	return nil
}

// Pop 阻塞取出下一个应执行的运行；队列关闭且已排空时返回 false。
//
// 出队后同步写日志：进程重启时据此判定"哪些任务已出队"，避免重复执行。
func (q *priorityQueue) Pop() (string, bool) {
	q.mu.Lock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		q.mu.Unlock()
		return "", false
	}
	h := &itemHeap{items: q.items, now: time.Now()}
	it := heap.Pop(h).(*queueItem)
	q.items = h.items
	journal := q.journal
	q.mu.Unlock()

	if journal != nil {
		_ = journal.Dequeue(it.runID)
	}
	return it.runID, true
}

// Drain 取出并清空全部排队任务（引擎停止时用于把排队任务标记为失败）。
func (q *priorityQueue) Drain() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.items))
	for _, it := range q.items {
		out = append(out, it.runID)
	}
	q.items = q.items[:0]
	return out
}

// Snapshot 返回当前排队中的条目（按出队顺序），用于恢复流程与运维观测。
func (q *priorityQueue) Snapshot() []queueItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	src := make([]*queueItem, len(q.items))
	copy(src, q.items)
	h := &itemHeap{items: src, now: time.Now()}
	out := make([]queueItem, 0, len(src))
	for h.Len() > 0 {
		it := heap.Pop(h).(*queueItem)
		out = append(out, *it)
	}
	return out
}

// Close 关闭队列并唤醒所有等待中的 worker。
func (q *priorityQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// queuedOrder 按调度顺序输出排队任务（诊断/测试用）。
func (q *priorityQueue) queuedOrder() []string {
	items := q.Snapshot()
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.runID)
	}
	return out
}
