package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/codeagent/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// 任务与运行（共享存储实现）
// ---------------------------------------------------------------------------

func (s *SQLStore) CreateTask(t *domain.Task) error {
	if t == nil {
		return errors.New("store: 任务不能为空")
	}
	return s.put(context.Background(), "ca_tasks", "id", newID(t.ID),
		[]kv{{"tenant_id", t.TenantID}, {"idempotency_key", t.IdempotencyKey}, {"updated_ms", ms(t.UpdatedAt)}}, t)
}

func (s *SQLStore) UpdateTask(t *domain.Task) error {
	if t == nil {
		return errors.New("store: 任务不能为空")
	}
	ok, err := s.exists(context.Background(), "ca_tasks", "id", t.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return s.CreateTask(t)
}

func (s *SQLStore) GetTask(tenantID, id string) (*domain.Task, bool) {
	var v domain.Task
	ok, err := s.get(context.Background(), "ca_tasks", "id", id, &v)
	if err != nil || !ok || v.TenantID != tenantID {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) ListTasks(tenantID string, q domain.PageQuery) domain.Page[domain.Task] {
	q.Normalize()
	where, args := "tenant_id=?", []any{tenantID}
	clause, kwArgs := keywordClause("payload", q.Keyword)
	if clause != "" {
		where += " AND " + clause
		args = append(args, kwArgs...)
	}
	rows, err := s.payloads(context.Background(), "ca_tasks", where, "updated_ms DESC", args, 0)
	if err != nil {
		return domain.Page[domain.Task]{Items: []domain.Task{}, Page: q.Page, PageSize: q.PageSize}
	}
	all := make([]domain.Task, 0, len(rows))
	for _, raw := range rows {
		var t domain.Task
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		if q.Keyword != "" && !containsFold(t.Title, q.Keyword) && !containsFold(t.ID, q.Keyword) {
			continue
		}
		all = append(all, t)
	}
	return paginate(all, q)
}

// ---------------------------------------------------------------------------
// 运行
// ---------------------------------------------------------------------------

func (s *SQLStore) CreateRun(r *domain.TaskRun) error {
	if r == nil {
		return errors.New("store: 运行记录不能为空")
	}
	return s.putRun(r)
}

func (s *SQLStore) UpdateRun(r *domain.TaskRun) error {
	if r == nil {
		return errors.New("store: 运行记录不能为空")
	}
	ok, err := s.exists(context.Background(), "ca_runs", "id", r.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	r.UpdatedAt = time.Now()
	return s.putRun(r)
}

// putRun 落库一条运行：索引列承载调度相关的查询维度，payload 承载完整上下文。
func (s *SQLStore) putRun(r *domain.TaskRun) error {
	return s.put(context.Background(), "ca_runs", "id", newID(r.ID), []kv{
		{"tenant_id", r.TenantID},
		{"task_id", r.TaskID},
		{"state", string(r.State)},
		{"priority", r.Priority},
		{"idempotency_key", r.IdempotencyKey},
		{"created_ms", ms(r.CreatedAt)},
		{"updated_ms", ms(r.UpdatedAt)},
	}, r)
}

func (s *SQLStore) GetRun(tenantID, id string) (*domain.TaskRun, bool) {
	run, ok := s.GetRunRaw(id)
	if !ok || run.TenantID != tenantID {
		return nil, false
	}
	return run, true
}

func (s *SQLStore) GetRunRaw(id string) (*domain.TaskRun, bool) {
	var v domain.TaskRun
	ok, err := s.get(context.Background(), "ca_runs", "id", id, &v)
	if err != nil || !ok {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) FindRunByIdempotencyKey(tenantID, key string) (*domain.TaskRun, bool) {
	if key == "" {
		return nil, false
	}
	rows, err := s.payloads(context.Background(), "ca_runs", "tenant_id=? AND idempotency_key=?", "created_ms DESC", []any{tenantID, key}, 1)
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	var v domain.TaskRun
	if err := json.Unmarshal(rows[0], &v); err != nil {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) ListRuns(tenantID string, q domain.PageQuery) domain.Page[domain.TaskRun] {
	q.Normalize()
	where, args := "tenant_id=?", []any{tenantID}
	if q.State != "" {
		where += " AND state=?"
		args = append(args, q.State)
	}
	if q.Keyword != "" {
		where += " AND LOWER(payload) LIKE LOWER(?) ESCAPE '\\'"
		args = append(args, "%"+likeEscape(q.Keyword)+"%")
	}
	rows, err := s.payloads(context.Background(), "ca_runs", where, "created_ms DESC", args, 0)
	if err != nil {
		return domain.Page[domain.TaskRun]{Items: []domain.TaskRun{}, Page: q.Page, PageSize: q.PageSize}
	}
	all := make([]domain.TaskRun, 0, len(rows))
	for _, raw := range rows {
		var r domain.TaskRun
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		if q.Keyword != "" && !containsFold(r.Title, q.Keyword) && !containsFold(r.ID, q.Keyword) &&
			!containsFold(r.Stacktrace, q.Keyword) {
			continue
		}
		all = append(all, r)
	}
	return paginate(all, q)
}

func (s *SQLStore) AllRuns(tenantID string) []domain.TaskRun {
	rows, err := s.payloads(context.Background(), "ca_runs", "tenant_id=?", "created_ms DESC", []any{tenantID}, 0)
	if err != nil {
		return []domain.TaskRun{}
	}
	out := make([]domain.TaskRun, 0, len(rows))
	for _, raw := range rows {
		var r domain.TaskRun
		if err := json.Unmarshal(raw, &r); err == nil {
			out = append(out, r)
		}
	}
	return out
}

func (s *SQLStore) CountRunsByState(tenantID string) map[domain.TaskState]int {
	out := map[domain.TaskState]int{}
	rows, err := s.query(context.Background(), "SELECT state, COUNT(*) FROM ca_runs WHERE tenant_id=? GROUP BY state", tenantID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			continue
		}
		out[domain.TaskState(state)] = n
	}
	return out
}

// ListQueued 列出排队中的运行：按调度顺序（优先级降序、入队时间升序）。
func (s *SQLStore) ListQueued(limit int) []domain.TaskRun {
	rows, err := s.payloads(context.Background(), "ca_runs", "state=?", "priority DESC, created_ms ASC",
		[]any{string(domain.StateQueued)}, limit)
	if err != nil {
		return []domain.TaskRun{}
	}
	out := make([]domain.TaskRun, 0, len(rows))
	for _, raw := range rows {
		var r domain.TaskRun
		if err := json.Unmarshal(raw, &r); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// ClaimQueued 原子认领排队任务（多实例部署的关键：同一任务只会被一个实例认领）。
//
// 实现：先按调度顺序取候选 ID，再对每条执行带条件的 UPDATE
// （state 必须仍为 queued），RowsAffected==1 才视为认领成功。
// 条件更新保证并发实例不会重复认领同一条运行。
func (s *SQLStore) ClaimQueued(limit int) []domain.TaskRun {
	if limit <= 0 {
		limit = 32
	}
	ctx := context.Background()
	candidates, err := s.payloads(ctx, "ca_runs", "state=?", "priority DESC, created_ms ASC",
		[]any{string(domain.StateQueued)}, limit)
	if err != nil || len(candidates) == 0 {
		return []domain.TaskRun{}
	}
	out := make([]domain.TaskRun, 0, len(candidates))
	for _, raw := range candidates {
		var r domain.TaskRun
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		res, err := s.exec(ctx, "UPDATE ca_runs SET state=? WHERE id=? AND state=?",
			string(domain.StateAnalyzing), r.ID, string(domain.StateQueued))
		if err != nil {
			continue
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			continue // 已被其他实例认领或状态已变
		}
		claimed, ok := s.GetRunRaw(r.ID)
		if !ok {
			continue
		}
		out = append(out, *claimed)
	}
	return out
}

// ---------------------------------------------------------------------------
// 报告
// ---------------------------------------------------------------------------

func (s *SQLStore) CreateReport(r *domain.Report) error {
	if r == nil {
		return errors.New("store: 报告不能为空")
	}
	return s.put(context.Background(), "ca_reports", "id", newID(r.ID),
		[]kv{{"tenant_id", r.TenantID}, {"run_id", r.RunID}, {"created_ms", ms(r.CreatedAt)}}, r)
}

func (s *SQLStore) GetReport(tenantID, id string) (*domain.Report, bool) {
	var v domain.Report
	ok, err := s.get(context.Background(), "ca_reports", "id", id, &v)
	if err != nil || !ok || v.TenantID != tenantID {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) GetReportByRun(tenantID, runID string) (*domain.Report, bool) {
	rows, err := s.payloads(context.Background(), "ca_reports", "tenant_id=? AND run_id=?", "created_ms DESC", []any{tenantID, runID}, 1)
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	var v domain.Report
	if err := json.Unmarshal(rows[0], &v); err != nil {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) ListReports(tenantID string, q domain.PageQuery) domain.Page[domain.Report] {
	q.Normalize()
	where, args := "tenant_id=?", []any{tenantID}
	if q.Keyword != "" {
		where += " AND LOWER(payload) LIKE LOWER(?) ESCAPE '\\'"
		args = append(args, "%"+likeEscape(q.Keyword)+"%")
	}
	raws, total := s.pageLog("ca_reports", q, where, args, "created_ms DESC")
	items := make([]domain.Report, 0, len(raws))
	for _, raw := range raws {
		var r domain.Report
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			continue
		}
		if q.Keyword != "" && !containsFold(r.Title, q.Keyword) && !containsFold(r.Summary, q.Keyword) {
			continue
		}
		items = append(items, r)
	}
	if len(items) > q.PageSize {
		items = items[:q.PageSize]
	}
	if items == nil {
		items = []domain.Report{}
	}
	return domain.Page[domain.Report]{Items: items, Total: total, Page: q.Page, PageSize: q.PageSize}
}

// ---------------------------------------------------------------------------
// 审计与调用日志
// ---------------------------------------------------------------------------

func (s *SQLStore) AppendAudit(ev domain.AuditEvent) {
	if ev.ID == "" {
		ev.ID = newID("")
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	_ = s.put(context.Background(), "ca_audits", "id", ev.ID, []kv{
		{"tenant_id", ev.TenantID}, {"run_id", ev.RunID}, {"category", ev.Category},
		{"action", ev.Action}, {"level", ev.Level}, {"at_ms", ms(ev.At)},
	}, ev)
}

func (s *SQLStore) AppendSkillCall(c domain.SkillCall) {
	if c.ID == "" {
		c.ID = newID("")
	}
	if c.StartedAt.IsZero() {
		c.StartedAt = time.Now()
	}
	_ = s.put(context.Background(), "ca_skill_calls", "id", c.ID, []kv{
		{"tenant_id", c.TenantID}, {"run_id", c.RunID}, {"skill", c.Skill},
		{"status", string(c.Status)}, {"at_ms", ms(c.StartedAt)},
	}, c)
}

func (s *SQLStore) AppendModelCall(c domain.ModelCall) {
	if c.ID == "" {
		c.ID = newID("")
	}
	if c.StartedAt.IsZero() {
		c.StartedAt = time.Now()
	}
	_ = s.put(context.Background(), "ca_model_calls", "id", c.ID, []kv{
		{"tenant_id", c.TenantID}, {"run_id", c.RunID}, {"model", c.Model},
		{"status", string(c.Status)}, {"at_ms", ms(c.StartedAt)},
	}, c)
}

// pageLog 日志类查询：SQL 侧完成过滤与分页（日志量大，避免全量拉到内存）。
func (s *SQLStore) pageLog(table string, q domain.PageQuery, where string, args []any, order string) ([]string, int) {
	total, err := s.count(context.Background(), table, where, args...)
	if err != nil {
		total = 0
	}
	pageArgs := append(append([]any{}, args...), q.PageSize, q.Offset())
	q2 := "SELECT payload FROM " + table
	if where != "" {
		q2 += " WHERE " + where
	}
	if order != "" {
		q2 += " ORDER BY " + order
	}
	q2 += " LIMIT ? OFFSET ?"
	rows, err := s.query(context.Background(), q2, pageArgs...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		out = append(out, string(raw))
	}
	return out, total
}

func (s *SQLStore) ListAudits(tenantID string, q domain.PageQuery) domain.Page[domain.AuditEvent] {
	q.Normalize()
	where, args := "tenant_id=?", []any{tenantID}
	if q.State != "" {
		where += " AND category=?"
		args = append(args, q.State)
	}
	if q.Keyword != "" {
		where += " AND LOWER(payload) LIKE LOWER(?) ESCAPE '\\'"
		args = append(args, "%"+likeEscape(q.Keyword)+"%")
	}
	raws, total := s.pageLog("ca_audits", q, where, args, "at_ms DESC")
	items := make([]domain.AuditEvent, 0, len(raws))
	for _, raw := range raws {
		var v domain.AuditEvent
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			continue
		}
		if q.Keyword != "" && !containsFold(v.Message, q.Keyword) && !containsFold(v.Action, q.Keyword) {
			continue
		}
		items = append(items, v)
	}
	return domain.Page[domain.AuditEvent]{Items: items, Total: total, Page: q.Page, PageSize: q.PageSize}
}

func (s *SQLStore) ListSkillCalls(tenantID, runID string, q domain.PageQuery) domain.Page[domain.SkillCall] {
	q.Normalize()
	where, args := "tenant_id=?", []any{tenantID}
	if runID != "" {
		where += " AND run_id=?"
		args = append(args, runID)
	}
	if q.State != "" {
		where += " AND status=?"
		args = append(args, q.State)
	}
	if q.Keyword != "" {
		where += " AND LOWER(payload) LIKE LOWER(?) ESCAPE '\\'"
		args = append(args, "%"+likeEscape(q.Keyword)+"%")
	}
	raws, total := s.pageLog("ca_skill_calls", q, where, args, "at_ms DESC")
	items := make([]domain.SkillCall, 0, len(raws))
	for _, raw := range raws {
		var v domain.SkillCall
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			continue
		}
		if q.Keyword != "" && !containsFold(v.Skill, q.Keyword) {
			continue
		}
		items = append(items, v)
	}
	return domain.Page[domain.SkillCall]{Items: items, Total: total, Page: q.Page, PageSize: q.PageSize}
}

func (s *SQLStore) ListModelCalls(tenantID, runID string, q domain.PageQuery) domain.Page[domain.ModelCall] {
	q.Normalize()
	where, args := "tenant_id=?", []any{tenantID}
	if runID != "" {
		where += " AND run_id=?"
		args = append(args, runID)
	}
	if q.State != "" {
		where += " AND status=?"
		args = append(args, q.State)
	}
	if q.Keyword != "" {
		where += " AND LOWER(payload) LIKE LOWER(?) ESCAPE '\\'"
		args = append(args, "%"+likeEscape(q.Keyword)+"%")
	}
	raws, total := s.pageLog("ca_model_calls", q, where, args, "at_ms DESC")
	items := make([]domain.ModelCall, 0, len(raws))
	for _, raw := range raws {
		var v domain.ModelCall
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			continue
		}
		if q.Keyword != "" && !containsFold(v.Model, q.Keyword) && !containsFold(v.Stage, q.Keyword) {
			continue
		}
		items = append(items, v)
	}
	return domain.Page[domain.ModelCall]{Items: items, Total: total, Page: q.Page, PageSize: q.PageSize}
}

func (s *SQLStore) AllSkillCalls(tenantID string) []domain.SkillCall {
	rows, err := s.payloads(context.Background(), "ca_skill_calls", "tenant_id=?", "at_ms ASC", []any{tenantID}, 0)
	if err != nil {
		return []domain.SkillCall{}
	}
	out := make([]domain.SkillCall, 0, len(rows))
	for _, raw := range rows {
		var v domain.SkillCall
		if err := json.Unmarshal(raw, &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func (s *SQLStore) AllModelCalls(tenantID string) []domain.ModelCall {
	rows, err := s.payloads(context.Background(), "ca_model_calls", "tenant_id=?", "at_ms ASC", []any{tenantID}, 0)
	if err != nil {
		return []domain.ModelCall{}
	}
	out := make([]domain.ModelCall, 0, len(rows))
	for _, raw := range rows {
		var v domain.ModelCall
		if err := json.Unmarshal(raw, &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// Flush 共享存储每次写操作即落库，此处为空操作（保留接口一致性）。
func (s *SQLStore) Flush() error { return nil }

// Close 关闭连接池（仅关闭由 OpenSQL 打开的连接）。
func (s *SQLStore) Close() error {
	if s.ownDB && s.db != nil {
		return s.db.Close()
	}
	return nil
}

// Ping 探活，供启动自检使用。
func (s *SQLStore) Ping(ctx context.Context) error {
	if s.db == nil {
		return errors.New("store: 数据库连接未初始化")
	}
	return s.db.PingContext(ctxOrBackground(ctx))
}

// Dialect 返回方言标识（postgres / mysql / sqlite）。
func (s *SQLStore) Dialect() string { return s.dialect }
