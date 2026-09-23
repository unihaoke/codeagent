package engine

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 队列日志（WAL）
//
// 背景：队列原先是纯内存结构，进程重启会丢掉所有排队中的任务 ——
// 这些任务在存储里永远停在 queued，既不会被调度，也不会进入终态，
// 前端会一直显示"排队中"，而取消时后端却报"任务已终态/不存在"。
//
// 设计：
//  1. 入队追加 enqueue 记录，出队追加 dequeue 记录（追加写 + fsync，保证不丢）；
//  2. 启动时回放：只有 enqueue 没有 dequeue 的 run 视为"重启时仍在排队"，重新入队；
//  3. 回放完成后压缩（compact）日志，只保留仍未出队的记录，避免文件无限增长。
//
// 存储里的终态是权威判据：回放时会剔除已终态/已不存在的 run，避免重复执行。
// ---------------------------------------------------------------------------

// journalOp 日志操作类型。
type journalOp string

const (
	opEnqueue journalOp = "enqueue"
	opDequeue journalOp = "dequeue"
)

// journalRecord 一条日志记录（JSON Lines）。
type journalRecord struct {
	Op       journalOp `json:"op"`
	RunID    string    `json:"runId"`
	Priority int       `json:"priority,omitempty"`
	At       time.Time `json:"at"`
}

// restoredItem 回放得到的待恢复排队项。
type restoredItem struct {
	RunID      string
	Priority   int
	EnqueuedAt time.Time
}

// queueJournal 队列入队/出队日志。nil 表示不启用持久化。
type queueJournal struct {
	mu   sync.Mutex
	path string
	f    *os.File
	w    *bufio.Writer
}

// openQueueJournal 打开（不存在则创建）队列日志；path 为空返回 nil（不启用）。
func openQueueJournal(path string) (*queueJournal, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &queueJournal{path: path, f: f, w: bufio.NewWriter(f)}, nil
}

// Path 返回日志文件路径。
func (j *queueJournal) Path() string {
	if j == nil {
		return ""
	}
	return j.path
}

// Enqueue 追加一条入队记录并落盘。
func (j *queueJournal) Enqueue(runID string, priority int, at time.Time) error {
	return j.append(journalRecord{Op: opEnqueue, RunID: runID, Priority: priority, At: at})
}

// Dequeue 追加一条出队记录并落盘。
func (j *queueJournal) Dequeue(runID string) error {
	return j.append(journalRecord{Op: opDequeue, RunID: runID, At: time.Now()})
}

func (j *queueJournal) append(rec journalRecord) error {
	if j == nil {
		return nil
	}
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.w.Write(append(raw, '\n')); err != nil {
		return err
	}
	// 立即 flush + fsync：排队中的任务不能因为进程崩溃而丢失。
	if err := j.w.Flush(); err != nil {
		return err
	}
	return j.f.Sync()
}

// Replay 回放日志，返回"重启时仍在排队"的条目（按入队先后）。
func (j *queueJournal) Replay() ([]restoredItem, error) {
	if j == nil {
		return nil, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	f, err := os.Open(j.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	order := make([]string, 0, 64)
	pending := map[string]restoredItem{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec journalRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// 损坏行（如写入过程中断电造成的半行）跳过，不阻塞启动。
			continue
		}
		switch rec.Op {
		case opEnqueue:
			if rec.RunID == "" {
				continue
			}
			if _, ok := pending[rec.RunID]; !ok {
				order = append(order, rec.RunID)
			}
			at := rec.At
			if at.IsZero() {
				at = time.Now()
			}
			pending[rec.RunID] = restoredItem{RunID: rec.RunID, Priority: rec.Priority, EnqueuedAt: at}
		case opDequeue:
			delete(pending, rec.RunID)
		}
	}
	out := make([]restoredItem, 0, len(order))
	for _, id := range order {
		if it, ok := pending[id]; ok {
			out = append(out, it)
		}
	}
	return out, nil
}

// Compact 压缩日志：丢弃已出队记录，只保留 pending 中的入队记录。
//
// 回放后调用，避免长期运行下日志文件无限增长。
func (j *queueJournal) Compact(pending []restoredItem) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	tmp := j.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, it := range pending {
		rec := journalRecord{Op: opEnqueue, RunID: it.RunID, Priority: it.Priority, At: it.EnqueuedAt}
		raw, merr := json.Marshal(rec)
		if merr != nil {
			f.Close()
			os.Remove(tmp)
			return merr
		}
		if _, werr := w.Write(append(raw, '\n')); werr != nil {
			f.Close()
			os.Remove(tmp)
			return werr
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()

	// 先释放原句柄：Windows 不允许替换仍处于打开状态的文件
	// （否则 rename 返回 Access is denied）。
	if j.f != nil {
		_ = j.f.Close()
		j.f = nil
		j.w = nil
	}

	// 原子替换：压缩过程中崩溃也不会破坏原日志。
	if err := os.Rename(tmp, j.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// 重新打开句柄：原句柄指向已被替换的 inode（Unix），或已被关闭（Windows 前置关闭）。
	nf, err := os.OpenFile(j.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	j.f = nf
	j.w = bufio.NewWriter(nf)
	return nil
}

// Close 冲刷缓冲并关闭日志。
func (j *queueJournal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.w != nil {
		_ = j.w.Flush()
	}
	if j.f != nil {
		return j.f.Close()
	}
	return nil
}
