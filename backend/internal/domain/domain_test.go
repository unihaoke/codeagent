// Package domain 的核心枚举与状态机单元测试。
package domain

import (
	"testing"
	"time"
)

func TestTaskStateTransitions(t *testing.T) {
	cases := []struct {
		from TaskState
		to   TaskState
		want bool
	}{
		{StateQueued, StateAnalyzing, true},
		{StateQueued, StateCancelled, true},
		{StateQueued, StateVerifying, false},
		{StateAnalyzing, StateRepairing, true},
		{StateAnalyzing, StateDegraded, true},
		{StateAnalyzing, StateQueued, false},
		{StateRepairing, StateVerifying, true},
		{StateRepairing, StateAnalyzing, false},
		{StateVerifying, StateSucceeded, true},
		{StateVerifying, StateNeedsReview, true},
		{StateVerifying, StateDegraded, true},
		{StateSucceeded, StateAnalyzing, false},
		{StateFailed, StateAnalyzing, false},
		{StateCancelled, StateRepairing, false},
		{StateDegraded, StateVerifying, false},
	}
	for _, c := range cases {
		if got := c.from.CanTransitionTo(c.to); got != c.want {
			t.Errorf("%s → %s 期望 %v 实际 %v", c.from, c.to, c.want, got)
		}
	}
}

func TestTerminalAndRunning(t *testing.T) {
	for _, s := range TerminalStates() {
		if !s.IsTerminal() {
			t.Errorf("%s 应为终态", s)
		}
	}
	if StateAnalyzing.IsTerminal() {
		t.Error("analyzing 不应为终态")
	}
	if !StateRepairing.IsRunning() || StateQueued.IsRunning() || StateSucceeded.IsRunning() {
		t.Error("IsRunning 判定错误")
	}
}

func TestNormalizeSeverity(t *testing.T) {
	cases := map[string]Severity{
		"P0": SeverityBlocker, "fatal": SeverityBlocker,
		"P1": SeverityCritical, "ERROR": SeverityCritical, "严重": SeverityCritical,
		"warning": SeverityMajor, "p2": SeverityMajor,
		"info": SeverityMinor, "": SeverityInfo, "乱码": SeverityInfo,
	}
	for in, want := range cases {
		if got := NormalizeSeverity(in); got != want {
			t.Errorf("NormalizeSeverity(%q) 期望 %s 实际 %s", in, want, got)
		}
	}
}

func TestAutoVerifyEnabledAndPageNormalize(t *testing.T) {
	req := &CreateTaskRequest{}
	if !req.AutoVerifyEnabled() {
		t.Error("AutoVerify 缺省应为 true")
	}
	no := false
	req.AutoVerify = &no
	if req.AutoVerifyEnabled() {
		t.Error("AutoVerify=false 应返回 false")
	}

	q := PageQuery{Page: 0, PageSize: 0}
	q.Normalize()
	if q.Page != 1 || q.PageSize != 20 || q.Offset() != 0 {
		t.Errorf("分页归一化错误: %+v", q)
	}
	q2 := PageQuery{Page: 3, PageSize: 500}
	q2.Normalize()
	if q2.PageSize != 200 || q2.Offset() != 400 {
		t.Errorf("分页上限与偏移错误: %+v offset=%d", q2, q2.Offset())
	}
}

func TestSubjectHas(t *testing.T) {
	s := &Subject{Scopes: []string{"task:write", "repo:read"}}
	if !s.Has("task:write") || s.Has("admin:all") {
		t.Error("scope 判定错误")
	}
	admin := &Subject{Scopes: []string{"admin:all"}}
	if !admin.Has("repo:read") {
		t.Error("admin:all 应通配所有 scope")
	}
	var nilSub *Subject
	if nilSub.Has("task:write") {
		t.Error("nil 主体不应具备任何 scope")
	}
}

func TestElapsedMS(t *testing.T) {
	run := &TaskRun{}
	if run.ElapsedMS() != 0 {
		t.Error("未开始的任务耗时应为 0")
	}
	run = &TaskRun{StartedAt: time.Now().Add(-1500 * time.Millisecond)}
	ms := run.ElapsedMS()
	if ms < 1400 || ms > 4000 {
		t.Errorf("耗时计算异常: %d", ms)
	}
}
