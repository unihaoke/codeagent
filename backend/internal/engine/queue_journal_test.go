package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 日志回放：只有 enqueue 没有 dequeue 的 run 视为"仍在排队"。
func TestQueueJournalReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.journal")
	j, err := openQueueJournal(path)
	if err != nil {
		t.Fatalf("打开队列日志失败: %v", err)
	}
	defer j.Close()

	now := time.Now()
	if err := j.Enqueue("r1", 5, now); err != nil {
		t.Fatalf("写入入队记录失败: %v", err)
	}
	if err := j.Enqueue("r2", 1, now); err != nil {
		t.Fatalf("写入入队记录失败: %v", err)
	}
	if err := j.Dequeue("r1"); err != nil {
		t.Fatalf("写入出队记录失败: %v", err)
	}

	pending, err := j.Replay()
	if err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	if len(pending) != 1 || pending[0].RunID != "r2" {
		t.Fatalf("回放结果不符：%+v", pending)
	}
	if pending[0].Priority != 1 {
		t.Fatalf("回放应保留优先级，实际 %d", pending[0].Priority)
	}
}

// 压缩：只保留仍未出队的记录，日志体积不再无限增长。
func TestQueueJournalCompact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.journal")
	j, err := openQueueJournal(path)
	if err != nil {
		t.Fatalf("打开队列日志失败: %v", err)
	}
	defer j.Close()

	now := time.Now()
	for i := 0; i < 50; i++ {
		id := "run-" + itoa(i)
		if err := j.Enqueue(id, i%10, now); err != nil {
			t.Fatalf("入队记录写入失败: %v", err)
		}
		if i%2 == 0 { // 一半立即出队
			if err := j.Dequeue(id); err != nil {
				t.Fatalf("出队记录写入失败: %v", err)
			}
		}
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}

	pending, err := j.Replay()
	if err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	if len(pending) != 25 {
		t.Fatalf("待恢复数量不符：期望 25，实际 %d", len(pending))
	}
	if err := j.Compact(pending); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	if after.Size() >= before.Size() {
		t.Fatalf("压缩后文件应变小：before=%d after=%d", before.Size(), after.Size())
	}

	// 压缩后再次回放：待恢复项保持不变（重启恢复可重复执行）。
	again, err := j.Replay()
	if err != nil {
		t.Fatalf("二次回放失败: %v", err)
	}
	if len(again) != len(pending) {
		t.Fatalf("压缩后回放数量不符：期望 %d，实际 %d", len(pending), len(again))
	}
}

// 队列 + 日志联动：入队写盘、出队写盘，重启（新建队列 + 回放）后能恢复未出队任务。
func TestPriorityQueueWithJournalRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.journal")
	j, err := openQueueJournal(path)
	if err != nil {
		t.Fatalf("打开队列日志失败: %v", err)
	}

	q1 := newPriorityQueue(16)
	q1.SetJournal(j)
	if err := q1.Push("keep-1", 7); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if err := q1.Push("keep-2", 2); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// 出队一条：日志应记录已出队，重启后不再恢复。
	if got, ok := q1.Pop(); !ok || got != "keep-1" {
		t.Fatalf("出队结果不符：%s ok=%v", got, ok)
	}
	_ = j.Close()

	// 模拟重启：新队列 + 回放日志。
	j2, err := openQueueJournal(path)
	if err != nil {
		t.Fatalf("重新打开队列日志失败: %v", err)
	}
	defer j2.Close()
	pending, err := j2.Replay()
	if err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	if len(pending) != 1 || pending[0].RunID != "keep-2" {
		t.Fatalf("重启后待恢复任务不符：%+v", pending)
	}
	q2 := newPriorityQueue(16)
	q2.SetJournal(j2)
	for _, it := range pending {
		if err := q2.PushRestored(it.RunID, it.Priority, it.EnqueuedAt); err != nil {
			t.Fatalf("恢复入队失败: %v", err)
		}
	}
	if got, ok := q2.Pop(); !ok || got != "keep-2" {
		t.Fatalf("恢复后出队失败：%s ok=%v", got, ok)
	}
	if err := j2.Compact(nil); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
}
