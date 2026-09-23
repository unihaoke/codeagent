package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/domain"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New()
}

func TestTenantCRUD(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	tn := &domain.Tenant{ID: "t1", Name: "租户一", Status: domain.TenantActive, Quota: domain.DefaultQuota(), CreatedAt: now, UpdatedAt: now}
	if err := s.CreateTenant(tn); err != nil {
		t.Fatalf("创建租户失败: %v", err)
	}
	got, ok := s.GetTenant("t1")
	if !ok || got.Name != "租户一" {
		t.Fatalf("查询租户失败: %+v ok=%v", got, ok)
	}
	if g, ok := s.GetTenantByKey("租户一"); !ok || g.ID != "t1" {
		t.Fatalf("按名称查询租户失败")
	}
	got.Name = "租户一改"
	if err := s.UpdateTenant(got); err != nil {
		t.Fatalf("更新租户失败: %v", err)
	}
	if g, _ := s.GetTenant("t1"); g.Name != "租户一改" {
		t.Fatalf("更新未生效")
	}
	if list := s.ListTenants(); len(list) != 1 {
		t.Fatalf("租户列表数量错误: %d", len(list))
	}
	if err := s.UpdateTenant(&domain.Tenant{ID: "missing"}); err != ErrNotFound {
		t.Fatalf("更新不存在租户应返回 ErrNotFound，实际 %v", err)
	}
}

func TestRepoIsolationAndUniqueKey(t *testing.T) {
	s := newTestStore(t)
	base := domain.Repository{TenantID: "t1", Key: "order-service", Name: "订单服务", Status: domain.RepoActive}
	if err := s.CreateRepo(&base); err != nil {
		t.Fatalf("创建仓库失败: %v", err)
	}
	// 同租户同 Key 冲突
	dup := domain.Repository{TenantID: "t1", Key: "ORDER-SERVICE", Name: "重复"}
	if err := s.CreateRepo(&dup); err != ErrConflict {
		t.Fatalf("同租户重复 Key 应冲突，实际 %v", err)
	}
	// 跨租户同 Key 允许
	other := domain.Repository{TenantID: "t2", Key: "order-service", Name: "其它租户"}
	if err := s.CreateRepo(&other); err != nil {
		t.Fatalf("跨租户重复 Key 应允许: %v", err)
	}
	// 跨租户不可见
	if _, ok := s.GetRepo("t2", base.ID); ok {
		t.Fatalf("跨租户读取仓库必须失败")
	}
	if got := s.ListRepos("t2"); len(got) != 1 || got[0].ID != other.ID {
		t.Fatalf("租户隔离失效: %+v", got)
	}
}

func TestGroupMembersAndRepoIsolation(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	r1 := &domain.Repository{ID: "r1", TenantID: "t1", Key: "web-mall", CreatedAt: now}
	r2 := &domain.Repository{ID: "r2", TenantID: "t1", Key: "api-gateway", CreatedAt: now}
	r3 := &domain.Repository{ID: "r3", TenantID: "t1", Key: "order-service", CreatedAt: now}
	for _, r := range []*domain.Repository{r1, r2, r3} {
		if err := s.CreateRepo(r); err != nil {
			t.Fatal(err)
		}
	}
	g := &domain.RepositoryGroup{ID: "g1", TenantID: "t1", Key: "mall-core", Name: "商城核心", EntryRepositoryIDs: []string{"r1"}, CreatedAt: now}
	members := []domain.GroupMember{
		{RepositoryID: "r3", Order: 3},
		{RepositoryID: "r1", Order: 1},
		{RepositoryID: "r2", Order: 2},
	}
	if err := s.CreateGroup(g, members); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}
	views := s.GroupMemberViews("t1", "g1")
	if len(views) != 3 {
		t.Fatalf("成员数量错误: %d", len(views))
	}
	if views[0].Repo == nil || views[0].Repo.Key != "web-mall" || views[2].Repo.Key != "order-service" {
		t.Fatalf("成员未按 Order 升序或仓库未聚合: %+v", views[0])
	}
	if ids := s.GroupMemberRepoIDs("g1"); len(ids) != 3 {
		t.Fatalf("成员 ID 数量错误: %v", ids)
	}
	if gs := s.GroupsOfRepo("t1", "r3"); len(gs) != 1 || gs[0].ID != "g1" {
		t.Fatalf("反查分组失败: %+v", gs)
	}
	// 删除仓库应同时移除分组成员关系
	if err := s.DeleteRepo("t1", "r3"); err != nil {
		t.Fatalf("删除仓库失败: %v", err)
	}
	if views := s.GroupMemberViews("t1", "g1"); len(views) != 2 {
		t.Fatalf("删除仓库后成员关系未清理: %d", len(views))
	}
	// 更新分组时替换成员
	if err := s.UpdateGroup(g, []domain.GroupMember{{RepositoryID: "r1", Order: 1}}); err != nil {
		t.Fatalf("更新分组失败: %v", err)
	}
	if views := s.GroupMemberViews("t1", "g1"); len(views) != 1 {
		t.Fatalf("成员替换失败: %d", len(views))
	}
}

func TestIdempotencyIndexAndRunIsolation(t *testing.T) {
	s := newTestStore(t)
	run := &domain.TaskRun{
		ID: "run1", TenantID: "t1", TaskID: "task1", State: domain.StateQueued,
		IdempotencyKey: "abc123", PinnedCommits: map[string]string{"r1": "c0ffee"},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	got, ok := s.FindRunByIdempotencyKey("t1", "abc123")
	if !ok || got.ID != "run1" {
		t.Fatalf("幂等键索引失败: %+v ok=%v", got, ok)
	}
	if _, ok := s.FindRunByIdempotencyKey("t2", "abc123"); ok {
		t.Fatalf("幂等键必须按租户隔离")
	}
	if _, ok := s.GetRun("t2", "run1"); ok {
		t.Fatalf("跨租户读取运行必须失败")
	}
	if _, ok := s.GetRunRaw("run1"); !ok {
		t.Fatalf("GetRunRaw 应忽略租户校验")
	}

	// 更新时必须深拷贝 PinnedCommits，避免外部修改污染存储
	run.PinnedCommits["r1"] = "mutated"
	if err := s.UpdateRun(run); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRun("t1", "run1")
	if got.PinnedCommits["r1"] != "mutated" {
		t.Fatalf("更新未生效")
	}
	got.PinnedCommits["r1"] = "external"
	again, _ := s.GetRun("t1", "run1")
	if again.PinnedCommits["r1"] != "mutated" {
		t.Fatalf("返回值未深拷贝，外部修改污染了存储")
	}
}

func TestListRunsFilterAndPaging(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	for i := 0; i < 25; i++ {
		st := domain.StateSucceeded
		if i%5 == 0 {
			st = domain.StateFailed
		}
		run := &domain.TaskRun{
			ID:        "run-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			TenantID:  "t1",
			TaskID:    "task1",
			Title:     "订单详情 NPE",
			State:     st,
			CreatedAt: now.Add(time.Duration(i) * time.Second),
			UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}
		if err := s.CreateRun(run); err != nil {
			t.Fatal(err)
		}
	}
	page := s.ListRuns("t1", domain.PageQuery{Page: 1, PageSize: 10})
	if page.Total != 25 || len(page.Items) != 10 {
		t.Fatalf("分页错误: total=%d len=%d", page.Total, len(page.Items))
	}
	// 第一页应为最新创建的记录
	if !page.Items[0].CreatedAt.After(page.Items[1].CreatedAt) {
		t.Fatalf("未按创建时间倒序")
	}
	failed := s.ListRuns("t1", domain.PageQuery{Page: 1, PageSize: 100, State: string(domain.StateFailed)})
	if failed.Total != 5 {
		t.Fatalf("状态过滤错误: %d", failed.Total)
	}
	kw := s.ListRuns("t1", domain.PageQuery{Page: 1, PageSize: 100, Keyword: "npe"})
	if kw.Total != 25 {
		t.Fatalf("关键字过滤（大小写不敏感）错误: %d", kw.Total)
	}
	if other := s.ListRuns("t2", domain.PageQuery{}); other.Total != 0 {
		t.Fatalf("租户隔离失效: %d", other.Total)
	}
	if dist := s.CountRunsByState("t1"); dist[domain.StateFailed] != 5 || dist[domain.StateSucceeded] != 20 {
		t.Fatalf("状态统计错误: %+v", dist)
	}
}

func TestAuditAndCallLogs(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		s.AppendAudit(domain.AuditEvent{
			ID: "a" + string(rune('0'+i)), TenantID: "t1", Category: "skill", Action: "invoke",
			Level: "info", Message: "调用技能", At: time.Now(),
		})
	}
	s.AppendAudit(domain.AuditEvent{ID: "x", TenantID: "t2", Category: "task", Action: "submit", Level: "info", Message: "其它租户", At: time.Now()})
	s.AppendSkillCall(domain.SkillCall{ID: "s1", TenantID: "t1", RunID: "run1", Skill: "stacktrace_parse", Status: domain.CallOK, StartedAt: time.Now()})
	s.AppendModelCall(domain.ModelCall{ID: "m1", TenantID: "t1", RunID: "run1", Model: "mock-reasoner-v1", Status: domain.CallOK, StartedAt: time.Now()})

	if got := s.ListAudits("t1", domain.PageQuery{Page: 1, PageSize: 10}); got.Total != 5 {
		t.Fatalf("审计租户隔离失败: %d", got.Total)
	}
	if got := s.ListAudits("t1", domain.PageQuery{Page: 1, PageSize: 10, State: "skill"}); got.Total != 5 {
		t.Fatalf("审计分类过滤失败: %d", got.Total)
	}
	if got := s.ListSkillCalls("t1", "run1", domain.PageQuery{Page: 1, PageSize: 10}); got.Total != 1 {
		t.Fatalf("技能调用查询失败: %d", got.Total)
	}
	if got := s.ListModelCalls("t1", "", domain.PageQuery{Page: 1, PageSize: 10}); got.Total != 1 {
		t.Fatalf("模型调用查询失败: %d", got.Total)
	}
	if len(s.AllSkillCalls("t1")) != 1 || len(s.AllModelCalls("t1")) != 1 {
		t.Fatalf("全量调用记录查询失败")
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s1, err := OpenFile(path, 0)
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	now := time.Now()
	if err := s1.CreateTenant(&domain.Tenant{ID: "t1", Name: "demo", Status: domain.TenantActive, Quota: domain.DefaultQuota(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s1.CreateRepo(&domain.Repository{ID: "r1", TenantID: "t1", Key: "order-service", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s1.CreateRun(&domain.TaskRun{ID: "run1", TenantID: "t1", State: domain.StateSucceeded, IdempotencyKey: "k1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s1.CreateReport(&domain.Report{ID: "rep1", TenantID: "t1", RunID: "run1", Title: "报告", Markdown: "# 报告", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s1.AppendAudit(domain.AuditEvent{ID: "a1", TenantID: "t1", Message: "审计", At: now})
	if err := s1.Flush(); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}

	s2, err := OpenFile(path, 0)
	if err != nil {
		t.Fatalf("重新打开存储失败: %v", err)
	}
	if _, ok := s2.GetTenant("t1"); !ok {
		t.Fatalf("租户未持久化")
	}
	if _, ok := s2.GetRepo("t1", "r1"); !ok {
		t.Fatalf("仓库未持久化")
	}
	if got, ok := s2.FindRunByIdempotencyKey("t1", "k1"); !ok || got.ID != "run1" {
		t.Fatalf("运行与幂等索引未持久化")
	}
	if _, ok := s2.GetReport("t1", "rep1"); !ok {
		t.Fatalf("报告未持久化")
	}
	if got := s2.ListAudits("t1", domain.PageQuery{Page: 1, PageSize: 10}); got.Total != 1 {
		t.Fatalf("审计未持久化: %d", got.Total)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := newTestStore(t)
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				id := "run-" + string(rune('a'+n)) + string(rune('0'+j%10))
				_ = s.CreateRun(&domain.TaskRun{ID: id, TenantID: "t1", State: domain.StateQueued, CreatedAt: time.Now(), UpdatedAt: time.Now()})
				_ = s.ListRuns("t1", domain.PageQuery{Page: 1, PageSize: 5})
				s.AppendAudit(domain.AuditEvent{ID: id, TenantID: "t1", Message: "并发写入", At: time.Now()})
			}
		}(i)
	}
	for i := 0; i < 16; i++ {
		<-done
	}
	if got := len(s.AllRuns("t1")); got == 0 {
		t.Fatalf("并发写入后无数据")
	}
}
