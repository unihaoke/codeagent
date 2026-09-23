package engine

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 出队顺序：优先级降序（越大越优先）。
func TestPriorityQueueOrder(t *testing.T) {
	q := newPriorityQueue(16)
	for _, it := range []struct {
		id  string
		pri int
	}{{"low", 1}, {"high", 9}, {"mid", 5}} {
		if err := q.Push(it.id, it.pri); err != nil {
			t.Fatalf("入队 %s 失败: %v", it.id, err)
		}
	}
	want := []string{"high", "mid", "low"}
	for _, w := range want {
		got, ok := q.Pop()
		if !ok {
			t.Fatalf("队列提前耗尽，期望 %s", w)
		}
		if got != w {
			t.Fatalf("出队顺序错误：期望 %s，实际 %s", w, got)
		}
	}
	if q.Len() != 0 {
		t.Fatalf("队列应为空，实际 %d", q.Len())
	}
}

// 同优先级保持入队先后（FIFO），避免同级别任务乱序。
func TestPriorityQueueSamePriorityFIFO(t *testing.T) {
	q := newPriorityQueue(16)
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Push(id, 3); err != nil {
			t.Fatalf("入队失败: %v", err)
		}
	}
	for _, w := range []string{"a", "b", "c"} {
		got, _ := q.Pop()
		if got != w {
			t.Fatalf("同优先级未保持 FIFO：期望 %s，实际 %s", w, got)
		}
	}
}

// 老化：等待足够久的低优任务应追平高优任务（同分时仍按入队先后）。
func TestPriorityQueueAging(t *testing.T) {
	q := newPriorityQueue(16)
	old := time.Now().Add(-time.Duration(maxAgingBoost) * agingInterval)
	// 直接落队列，绕开默认的"当前时间入队"。
	if err := q.push("old-low", 0, old, false); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if err := q.Push("new-high", 9); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// old-low 有效优先级 = 0 + 9 = 9，与 new-high 打平，按入队先后先出。
	got, _ := q.Pop()
	if got != "old-low" {
		t.Fatalf("老化未生效：期望 old-low 优先，实际 %s", got)
	}

	// 老化加成有上限：等待再久，有效优先级也不会超过 maxAgingBoost + 基础优先级。
	ancient := &queueItem{priority: 0, enqueuedAt: time.Now().Add(-1000 * agingInterval)}
	if got := ancient.effectivePriority(time.Now()); got != maxAgingBoost {
		t.Fatalf("老化加成应封顶在 %d，实际 %d", maxAgingBoost, got)
	}
	// 封顶后与同分的高优任务比较时，仍按入队先后（FIFO）决定，不会反转。
	q2 := newPriorityQueue(16)
	if err := q2.push("ancient", 0, time.Now().Add(-1000*agingInterval), false); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if err := q2.push("later", maxAgingBoost, time.Now(), false); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if got2, _ := q2.Pop(); got2 != "ancient" {
		t.Fatalf("同分应按入队先后：期望 ancient，实际 %s", got2)
	}
}

// 优先级越界值被夹紧，不破坏排序。
func TestClampPriority(t *testing.T) {
	if got := clampPriority(-5); got != priorityMin {
		t.Fatalf("期望夹紧到 %d，实际 %d", priorityMin, got)
	}
	if got := clampPriority(999); got != priorityMax {
		t.Fatalf("期望夹紧到 %d，实际 %d", priorityMax, got)
	}
}

func TestPriorityQueueCapacityAndClose(t *testing.T) {
	q := newPriorityQueue(1)
	if err := q.Push("a", 1); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if err := q.Push("b", 1); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("队列满应返回 ErrQueueFull，实际 %v", err)
	}
	if got, _ := q.Pop(); got != "a" {
		t.Fatalf("期望取出 a，实际 %s", got)
	}
	q.Close()
	if err := q.Push("c", 1); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("已关闭队列应返回 ErrQueueClosed，实际 %v", err)
	}
	if _, ok := q.Pop(); ok {
		t.Fatal("已关闭且排空的队列 Pop 应返回 false")
	}
}

func TestPriorityQueueDrain(t *testing.T) {
	q := newPriorityQueue(8)
	for _, id := range []string{"a", "b", "c"} {
		_ = q.Push(id, 1)
	}
	got := q.Drain()
	if len(got) != 3 {
		t.Fatalf("Drain 应取出 3 条，实际 %d", len(got))
	}
	if q.Len() != 0 {
		t.Fatalf("Drain 后队列应为空，实际 %d", q.Len())
	}
}

// Pop 在空队列上阻塞，Close 后唤醒并返回 false（worker 退出信号）。
func TestPriorityQueuePopWakesOnClose(t *testing.T) {
	q := newPriorityQueue(4)
	done := make(chan bool, 1)
	go func() {
		_, ok := q.Pop()
		done <- ok
	}()
	time.Sleep(50 * time.Millisecond)
	q.Close()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("Close 后 Pop 应返回 false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close 未唤醒阻塞的 Pop")
	}
}

// 并发入队/出队不丢不重（配合 -race 检查数据竞争）。
func TestPriorityQueueConcurrent(t *testing.T) {
	q := newPriorityQueue(512)
	var wg sync.WaitGroup
	const n = 200
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = q.Push(string(rune('a'+i%26))+itoa(i), i%10)
		}(i)
	}
	wg.Wait()
	if q.Len() != n {
		t.Fatalf("入队数不符：期望 %d，实际 %d", n, q.Len())
	}
	seen := map[string]int{}
	for {
		id, ok := q.Pop()
		if !ok {
			break
		}
		seen[id]++
		if len(seen) == n {
			break
		}
	}
	if len(seen) != n {
		t.Fatalf("出队数不符：期望 %d，实际 %d", n, len(seen))
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("任务 %s 被重复出队 %d 次", id, c)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	buf := []byte{}
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	return string(buf)
}
