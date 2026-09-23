// Package eventbus 提供进程内实时事件总线，支撑可观测与前端实时推送。
package eventbus

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/codeagent/backend/internal/domain"
)

// Bus 进程内事件总线。
//
// 订阅者缓冲区满时丢弃最旧事件而不是阻塞发布方，保证任务执行永不被观测链路拖垮。
type Bus struct {
	mu      sync.RWMutex
	subs    map[int64]*subscriber
	nextID  int64
	seq     int64
	bufSize int
	// history 环形历史，供新订阅者回放最近的运行事件。
	history  []domain.Event
	historyN int
	counter  atomic.Int64
	dropped  atomic.Int64
}

type subscriber struct {
	id       int64
	tenantID string
	ch       chan domain.Event
}

// New 创建事件总线。
func New(bufSize int) *Bus {
	if bufSize <= 0 {
		bufSize = 256
	}
	return &Bus{
		subs:     make(map[int64]*subscriber),
		bufSize:  bufSize,
		history:  make([]domain.Event, 0, 512),
		historyN: 512,
	}
}

// Publish 发布事件。
func (b *Bus) Publish(ev domain.Event) {
	ev.Seq = b.counter.Add(1)
	if ev.At.IsZero() {
		ev.At = time.Now()
	}

	b.mu.Lock()
	b.history = append(b.history, ev)
	if len(b.history) > b.historyN {
		b.history = b.history[len(b.history)-b.historyN:]
	}
	targets := make([]*subscriber, 0, len(b.subs))
	for _, s := range b.subs {
		if s.tenantID == "" || s.tenantID == ev.TenantID {
			targets = append(targets, s)
		}
	}
	b.mu.Unlock()

	for _, s := range targets {
		select {
		case s.ch <- ev:
		default:
			// 缓冲满：丢弃最旧的一条，保证实时性优先。
			select {
			case <-s.ch:
			default:
			}
			b.dropped.Add(1)
			select {
			case s.ch <- ev:
			default:
			}
		}
	}
}

// Subscribe 订阅事件。返回取消函数，必须调用以避免泄漏。
func (b *Bus) Subscribe(tenantID string) (<-chan domain.Event, func()) {
	ch := make(chan domain.Event, b.bufSize)
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subs[id] = &subscriber{id: id, tenantID: tenantID, ch: ch}
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if s, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(s.ch)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// History 返回指定运行的历史事件（用于前端断线重连补齐）。
func (b *Bus) History(tenantID, runID string, limit int) []domain.Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]domain.Event, 0, limit)
	for i := len(b.history) - 1; i >= 0 && len(out) < limit; i-- {
		ev := b.history[i]
		if ev.TenantID != tenantID {
			continue
		}
		if runID != "" && ev.RunID != runID {
			continue
		}
		out = append(out, ev)
	}
	// 反转回时间正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Subscribers 返回当前订阅者数量。
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Dropped 返回因背压丢弃的事件数。
func (b *Bus) Dropped() int64 { return b.dropped.Load() }
