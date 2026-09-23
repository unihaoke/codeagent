// Package handler 实现接入层的各资源 HTTP 处理器（目录 internal/api，包名 handler）。
//
// 本包依赖 internal/domain、internal/store、internal/config、internal/platform/*
// 以及 internal/httpx（统一响应信封、上下文辅助、授权中间件）。
//
// 导入方向：internal/httpx → (无) ；internal/api → internal/httpx ；
// internal/app → { internal/httpx, internal/api }。服务装配位于 internal/app，
// 因此不存在 httpx ← api 的反向依赖，无导入环。
//
// 响应与错误统一定义在 internal/httpx：本包只做**薄委托**（见文件末尾"httpx 委托"段），
// 保证信封字段、错误码、HTTP 状态映射、context 键（请求 ID / 主体）在接入层内部
// 只有唯一实现，避免出现"跨包 context 键类型不同导致取不到主体"这类隐性缺陷。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/store"
)

// Deps 接入层 handler 依赖集合。
//
// 与 httpx.Deps（已随装配上移到 internal/app.Deps）的差别：Repos 已保证非 nil
// （由装配层完成适配），并额外携带 WebSocket Hub、版本号与启动时间。
type Deps struct {
	// Cfg 全系统配置（保证非 nil）。
	Cfg *config.Config
	// Store 数据访问层（保证非 nil）。
	Store store.Store
	// Auth 认证授权；为 nil 时按未装配处理。
	Auth domain.Authorizer
	// Engine Agent 核心调度层；为 nil 时任务写操作返回 503。
	Engine domain.TaskEngine
	// Repos 仓库分组索引（保证非 nil）。
	Repos domain.RepoIndex
	// Source 源码解析器；为 nil 时探测接口降级返回 503。
	Source domain.SourceResolver
	// Skills 技能注册中心。
	Skills domain.SkillRegistry
	// Runner 技能执行器（透传依赖，接入层不直接调用）。
	Runner domain.SkillRunner
	// MCP 模型管控层；为 nil 时返回空态。
	MCP domain.MCPGateway
	// Recorder 审计与轨迹记录器；为 nil 时回退 Store。
	Recorder domain.Recorder
	// Secrets 凭证加密箱；为 nil 时 AI 设置无法保存密钥（返回 503）。
	//
	// 用途：模型提供方的 API Key 明文入库前必须经 Seal 加密，
	// 装配到模型层之前再经 Open 还原，禁止明文落库。
	Secrets domain.CredentialBox
	// Bus 实时事件总线；为 nil 时 SSE 降级为一次性 JSON。
	Bus domain.EventBus
	// Log 结构化日志器（保证非 nil）。
	Log *logx.Logger
	// Hub WebSocket 广播中心。
	Hub WebSocketHub
	// Version 服务版本号。
	Version string
	// Started 服务启动时间。
	Started time.Time

	// data 数据访问抽象：优先 Engine/Recorder，缺失时回退 Store（惰性初始化）。
	data dataProvider
}

// WebSocketHub WebSocket 广播能力（由 *httpx.Hub 实现）。
//
// 抽成接口是为了让 handler 包不直接依赖 Hub 的具体实现细节，
// 同时保留 *httpx.Hub 作为唯一实现。
type WebSocketHub interface {
	// HandleWS 处理一次 WebSocket 会话。
	HandleWS(w http.ResponseWriter, r *http.Request, tenantID string, runID string)
}

// provider 返回数据访问抽象（惰性初始化，Engine/Recorder 缺失时回退 Store）。
//
// 说明：Deps 在装配期完成初始化后只读访问，故此处不做加锁（避免热路径锁竞争）。
func (d *Deps) provider() dataProvider {
	if d.data == nil {
		if d.Engine != nil && !isNilInterface(d.Engine) {
			d.data = &engineProvider{d: d}
		} else {
			d.data = &storeProvider{st: d.Store}
		}
	}
	return d.data
}

// log 返回日志器（nil 安全）。
func (d *Deps) log() *logx.Logger {
	if d.Log == nil {
		return logx.Nop()
	}
	return d.Log
}

// maxBody 返回请求体上限。
func (d *Deps) maxBody() int64 {
	if d.Cfg != nil && d.Cfg.Server.MaxBodyBytes > 0 {
		return d.Cfg.Server.MaxBodyBytes
	}
	return 4 << 20
}

// isNilInterface 判断接口值是否为 nil（含类型化 nil 指针）。
func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	switch t := v.(type) {
	case domain.Authorizer:
		return t == nil
	case domain.TaskEngine:
		return t == nil
	case domain.RepoIndex:
		return t == nil
	case domain.SourceResolver:
		return t == nil
	case domain.SkillRegistry:
		return t == nil
	case domain.MCPGateway:
		return t == nil
	case domain.Recorder:
		return t == nil
	case domain.EventBus:
		return t == nil
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 仓库索引适配器：store.Store → domain.RepoIndex
// ---------------------------------------------------------------------------

// NewStoreRepoIndex 把 store.Store 适配为 domain.RepoIndex。
//
// 适配原因（签名差异，两方均不可修改）：
//
//	| domain.RepoIndex                                  | store.Store                                  |
//	|---------------------------------------------------|-----------------------------------------------|
//	| GetRepo(ctx, tenantID, id) (*Repository, error)    | GetRepo(tenantID, id) (*Repository, bool)     |
//	| ListRepos(ctx, tenantID, q PageQuery) (Page, err)  | ListRepos(tenantID) []Repository              |
//	| GroupMembers(ctx, tenant, group) ([]View, error)   | GroupMemberViews(tenant, group) []View        |
//	| 所有方法都带 context                                | 全部不带 context                               |
//
// 即 store 缺少 context 参数、以 `(值, bool)` 代替 error 返回、列表方法没有
// context 且不分页、成员视图命名不同。适配器负责：
//   - 校验 ctx 取消（ctx.Err() 直接返回，尊重调用方超时）；
//   - 把 `(值, false)` 映射为 store.ErrNotFound；
//   - 为列表方法补齐关键字过滤与分页语义（与 store.ListTasks 行为一致）；
//   - 区分"分组不存在"与"分组为空"（GroupMembers 空结果时回查分组）。
func NewStoreRepoIndex(st store.Store) domain.RepoIndex {
	return &repoIndexAdapter{st: st}
}

// repoIndexAdapter 见 NewStoreRepoIndex 的说明。
type repoIndexAdapter struct{ st store.Store }

// ListRepos 实现 domain.RepoIndex：关键字过滤 + 分页。
func (a *repoIndexAdapter) ListRepos(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.Repository], error) {
	if err := ctxErr(ctx); err != nil {
		return domain.Page[domain.Repository]{}, err
	}
	q.Normalize()
	all := a.st.ListRepos(tenantID)
	filtered := make([]domain.Repository, 0, len(all))
	for _, r := range all {
		if q.Keyword != "" && !containsFold(r.Name, q.Keyword) && !containsFold(r.Key, q.Keyword) &&
			!containsFold(r.URL, q.Keyword) {
			continue
		}
		if q.State != "" && string(r.Status) != q.State && string(r.Layer) != q.State {
			continue
		}
		filtered = append(filtered, r)
	}
	return paginate(filtered, q), nil
}

// GetRepo 实现 domain.RepoIndex。
func (a *repoIndexAdapter) GetRepo(ctx context.Context, tenantID, repoID string) (*domain.Repository, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	r, ok := a.st.GetRepo(tenantID, repoID)
	if !ok {
		return nil, store.ErrNotFound
	}
	return r, nil
}

// GetRepoByKey 实现 domain.RepoIndex。
func (a *repoIndexAdapter) GetRepoByKey(ctx context.Context, tenantID, key string) (*domain.Repository, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	r, ok := a.st.GetRepoByKey(tenantID, key)
	if !ok {
		return nil, store.ErrNotFound
	}
	return r, nil
}

// CreateRepo 实现 domain.RepoIndex。
func (a *repoIndexAdapter) CreateRepo(ctx context.Context, repo *domain.Repository) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	return a.st.CreateRepo(repo)
}

// UpdateRepo 实现 domain.RepoIndex。
func (a *repoIndexAdapter) UpdateRepo(ctx context.Context, repo *domain.Repository) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	return a.st.UpdateRepo(repo)
}

// DeleteRepo 实现 domain.RepoIndex。
func (a *repoIndexAdapter) DeleteRepo(ctx context.Context, tenantID, repoID string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	return a.st.DeleteRepo(tenantID, repoID)
}

// ListGroups 实现 domain.RepoIndex：关键字过滤 + 分页。
func (a *repoIndexAdapter) ListGroups(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.RepositoryGroup], error) {
	if err := ctxErr(ctx); err != nil {
		return domain.Page[domain.RepositoryGroup]{}, err
	}
	q.Normalize()
	all := a.st.ListGroups(tenantID)
	filtered := make([]domain.RepositoryGroup, 0, len(all))
	for _, g := range all {
		if q.Keyword != "" && !containsFold(g.Name, q.Keyword) && !containsFold(g.Key, q.Keyword) &&
			!containsFold(g.Description, q.Keyword) {
			continue
		}
		if q.State != "" && string(g.Status) != q.State {
			continue
		}
		filtered = append(filtered, g)
	}
	return paginate(filtered, q), nil
}

// GetGroup 实现 domain.RepoIndex。
func (a *repoIndexAdapter) GetGroup(ctx context.Context, tenantID, groupID string) (*domain.RepositoryGroup, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	g, ok := a.st.GetGroup(tenantID, groupID)
	if !ok {
		return nil, store.ErrNotFound
	}
	return g, nil
}

// CreateGroup 实现 domain.RepoIndex。
func (a *repoIndexAdapter) CreateGroup(ctx context.Context, g *domain.RepositoryGroup, members []domain.GroupMember) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	return a.st.CreateGroup(g, members)
}

// UpdateGroup 实现 domain.RepoIndex。
func (a *repoIndexAdapter) UpdateGroup(ctx context.Context, g *domain.RepositoryGroup, members []domain.GroupMember) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	return a.st.UpdateGroup(g, members)
}

// DeleteGroup 实现 domain.RepoIndex。
func (a *repoIndexAdapter) DeleteGroup(ctx context.Context, tenantID, groupID string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	return a.st.DeleteGroup(tenantID, groupID)
}

// GroupMembers 实现 domain.RepoIndex（桥接 store.GroupMemberViews）。
func (a *repoIndexAdapter) GroupMembers(ctx context.Context, tenantID, groupID string) ([]domain.GroupMemberView, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	views := a.st.GroupMemberViews(tenantID, groupID)
	if len(views) == 0 {
		// 区分"分组不存在"与"分组为空"，避免越权探测。
		if _, ok := a.st.GetGroup(tenantID, groupID); !ok {
			return nil, store.ErrNotFound
		}
	}
	return views, nil
}

// GroupsOfRepo 实现 domain.RepoIndex。
func (a *repoIndexAdapter) GroupsOfRepo(ctx context.Context, tenantID, repoID string) ([]domain.RepositoryGroup, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	return a.st.GroupsOfRepo(tenantID, repoID), nil
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func paginate[T any](all []T, q domain.PageQuery) domain.Page[T] {
	q.Normalize()
	total := len(all)
	start := q.Offset()
	if start > total {
		start = total
	}
	end := start + q.PageSize
	if end > total {
		end = total
	}
	items := all[start:end]
	if items == nil {
		items = []T{}
	}
	return domain.Page[T]{Items: items, Total: total, Page: q.Page, PageSize: q.PageSize}
}

// ---------------------------------------------------------------------------
// 数据访问抽象（Engine/Recorder 优先，缺失时回退 Store）
// ---------------------------------------------------------------------------

// dataProvider 接入层需要的数据访问能力。
//
// 之所以抽象一层：Store 与 Engine/Recorder 的能力有重叠（运行查询、列表、统计），
// 但 Engine 才是权威来源（例如运行详情含最新上下文），因此优先走 Engine。
type dataProvider interface {
	GetRun(ctx context.Context, tenantID, runID string) (*domain.TaskRun, error)
	ListTasks(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.Task], error)
	ListRuns(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.TaskRun], error)
	ListReports(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.Report], error)
	ListAudits(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.AuditEvent], error)
	ListSkillCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.SkillCall], error)
	ListModelCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.ModelCall], error)
	Observability(ctx context.Context, tenantID string) (*domain.ObservabilitySummary, error)
}

// engineProvider 以 Engine 为主、Store 兜底的数据访问实现。
type engineProvider struct{ d *Deps }

func (p *engineProvider) GetRun(ctx context.Context, tenantID, runID string) (*domain.TaskRun, error) {
	run, err := p.d.Engine.GetRun(ctx, tenantID, runID)
	if err == nil && run != nil {
		return run, nil
	}
	if st, ok := p.d.Store.GetRun(tenantID, runID); ok {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, store.ErrNotFound
}

func (p *engineProvider) ListTasks(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.Task], error) {
	if lister, ok := p.d.Engine.(interface {
		ListTasks(context.Context, string, domain.PageQuery) (domain.Page[domain.Task], error)
	}); ok {
		if page, err := lister.ListTasks(ctx, tenantID, q); err == nil {
			return nonNilPage(page), nil
		}
	}
	return nonNilPage(p.d.Store.ListTasks(tenantID, q)), nil
}

func (p *engineProvider) ListRuns(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.TaskRun], error) {
	if lister, ok := p.d.Engine.(interface {
		ListRuns(context.Context, string, domain.PageQuery) (domain.Page[domain.TaskRun], error)
	}); ok {
		if page, err := lister.ListRuns(ctx, tenantID, q); err == nil {
			return nonNilPage(page), nil
		}
	}
	return nonNilPage(p.d.Store.ListRuns(tenantID, q)), nil
}

func (p *engineProvider) ListReports(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.Report], error) {
	return nonNilPage(p.d.Store.ListReports(tenantID, q)), nil
}

func (p *engineProvider) ListAudits(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.AuditEvent], error) {
	if p.d.Recorder != nil && !isNilInterface(p.d.Recorder) {
		if page, err := p.d.Recorder.ListAudits(ctx, tenantID, q); err == nil {
			return nonNilPage(page), nil
		}
	}
	return nonNilPage(p.d.Store.ListAudits(tenantID, q)), nil
}

func (p *engineProvider) ListSkillCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.SkillCall], error) {
	if p.d.Recorder != nil && !isNilInterface(p.d.Recorder) {
		if page, err := p.d.Recorder.ListSkillCalls(ctx, tenantID, runID, q); err == nil {
			return nonNilPage(page), nil
		}
	}
	return nonNilPage(p.d.Store.ListSkillCalls(tenantID, runID, q)), nil
}

func (p *engineProvider) ListModelCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.ModelCall], error) {
	if p.d.Recorder != nil && !isNilInterface(p.d.Recorder) {
		if page, err := p.d.Recorder.ListModelCalls(ctx, tenantID, runID, q); err == nil {
			return nonNilPage(page), nil
		}
	}
	return nonNilPage(p.d.Store.ListModelCalls(tenantID, runID, q)), nil
}

func (p *engineProvider) Observability(ctx context.Context, tenantID string) (*domain.ObservabilitySummary, error) {
	if p.d.Recorder != nil && !isNilInterface(p.d.Recorder) {
		if sum, err := p.d.Recorder.Observability(ctx, tenantID); err == nil && sum != nil {
			return sum, nil
		}
	}
	return aggregateObservability(p.d.Store, tenantID), nil
}

// storeProvider 纯 Store 数据访问实现（Engine/Recorder 均缺失时使用）。
type storeProvider struct{ st store.Store }

func (p *storeProvider) GetRun(ctx context.Context, tenantID, runID string) (*domain.TaskRun, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	run, ok := p.st.GetRun(tenantID, runID)
	if !ok {
		return nil, store.ErrNotFound
	}
	return run, nil
}

func (p *storeProvider) ListTasks(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.Task], error) {
	return nonNilPage(p.st.ListTasks(tenantID, q)), nil
}

func (p *storeProvider) ListRuns(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.TaskRun], error) {
	return nonNilPage(p.st.ListRuns(tenantID, q)), nil
}

func (p *storeProvider) ListReports(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.Report], error) {
	return nonNilPage(p.st.ListReports(tenantID, q)), nil
}

func (p *storeProvider) ListAudits(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.AuditEvent], error) {
	return nonNilPage(p.st.ListAudits(tenantID, q)), nil
}

func (p *storeProvider) ListSkillCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.SkillCall], error) {
	return nonNilPage(p.st.ListSkillCalls(tenantID, runID, q)), nil
}

func (p *storeProvider) ListModelCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.ModelCall], error) {
	return nonNilPage(p.st.ListModelCalls(tenantID, runID, q)), nil
}

func (p *storeProvider) Observability(ctx context.Context, tenantID string) (*domain.ObservabilitySummary, error) {
	return aggregateObservability(p.st, tenantID), nil
}

// nonNilPage 保证分页信封的 items 为数组（而非 null），便于前端渲染空态。
func nonNilPage[T any](p domain.Page[T]) domain.Page[T] {
	if p.Items == nil {
		p.Items = []T{}
	}
	if p.Page <= 0 {
		p.Page = 1
	}
	if p.PageSize <= 0 {
		p.PageSize = 20
	}
	return p
}

// aggregateObservability 在无 Recorder 时基于 Store 聚合可观测汇总。
func aggregateObservability(st store.Store, tenantID string) *domain.ObservabilitySummary {
	runs := st.AllRuns(tenantID)
	skillCalls := st.AllSkillCalls(tenantID)
	modelCalls := st.AllModelCalls(tenantID)
	tasks := st.ListTasks(tenantID, domain.PageQuery{Page: 1, PageSize: 1})

	sum := &domain.ObservabilitySummary{
		TasksTotal:   tasks.Total,
		RunsTotal:    len(runs),
		SkillCalls:   len(skillCalls),
		ModelCalls:   len(modelCalls),
		StateDist:    map[string]int{},
		CategoryDist: map[string]int{},
	}
	skillAgg := map[string]*domain.SkillMetric{}
	modelAgg := map[string]*domain.ModelMetric{}
	var elapsedTotal int64
	for _, r := range runs {
		sum.StateDist[string(r.State)]++
		switch r.State {
		case domain.StateSucceeded:
			sum.RunsSucceeded++
		case domain.StateNeedsReview:
			sum.RunsNeedsReview++
		case domain.StateFailed:
			sum.RunsFailed++
		case domain.StateDegraded:
			sum.RunsDegraded++
		}
		if r.RootCause != nil {
			sum.CategoryDist[r.RootCause.Category]++
		}
		elapsedTotal += r.ElapsedMS()
		sum.CacheHits += r.Usage.CacheHits
		sum.ModelTokens += r.Usage.TotalTokens
		sum.ModelFallbacks += r.Usage.ModelFallbacks
		// 每次运行锁定的仓库数近似反映"仓库切换"规模。
		sum.RepoSwitches += len(r.Resolution)
	}
	if sum.RunsTotal > 0 {
		sum.FixRate = round4(float64(sum.RunsSucceeded) / float64(sum.RunsTotal))
		sum.AvgElapsedMS = elapsedTotal / int64(sum.RunsTotal)
	}
	for _, c := range skillCalls {
		m := skillAgg[c.Skill]
		if m == nil {
			m = &domain.SkillMetric{Skill: c.Skill}
			skillAgg[c.Skill] = m
		}
		m.Calls++
		m.AvgLatencyMS += float64(c.DurationMS)
		if c.Status != domain.CallOK {
			m.Failures++
			sum.SkillFailures++
		}
	}
	for _, m := range skillAgg {
		if m.Calls > 0 {
			m.AvgLatencyMS = round4(m.AvgLatencyMS / float64(m.Calls))
		}
		sum.TopSkills = append(sum.TopSkills, *m)
	}
	sortSkillMetrics(sum.TopSkills)
	if sum.SkillCalls > 0 {
		sum.SkillFailureRate = round4(float64(sum.SkillFailures) / float64(sum.SkillCalls))
	}
	for _, c := range modelCalls {
		key := c.Model
		if key == "" {
			key = c.Provider
		}
		m := modelAgg[key]
		if m == nil {
			m = &domain.ModelMetric{Model: key}
			modelAgg[key] = m
		}
		m.Calls++
		m.AvgLatencyMS += float64(c.DurationMS)
		m.TotalTokens += c.TotalTokens
	}
	for _, m := range modelAgg {
		if m.Calls > 0 {
			m.AvgLatencyMS = round4(m.AvgLatencyMS / float64(m.Calls))
		}
		sum.TopModels = append(sum.TopModels, *m)
	}
	sortModelMetrics(sum.TopModels)

	// 最近失败运行（最多 5 条）。
	page := st.ListRuns(tenantID, domain.PageQuery{Page: 1, PageSize: 50})
	for _, r := range page.Items {
		if r.State != domain.StateFailed && r.State != domain.StateNeedsReview && r.State != domain.StateDegraded {
			continue
		}
		sum.RecentFailures = append(sum.RecentFailures, domain.RunBrief{
			RunID: r.ID, Title: r.Title, State: r.State, Severity: r.Severity,
			Summary: r.Error, At: r.CreatedAt,
		})
		if len(sum.RecentFailures) >= 5 {
			break
		}
	}
	return sum
}

func sortSkillMetrics(in []domain.SkillMetric) {
	sort.Slice(in, func(i, j int) bool { return in[i].Calls > in[j].Calls })
}

func sortModelMetrics(in []domain.ModelMetric) {
	sort.Slice(in, func(i, j int) bool { return in[i].Calls > in[j].Calls })
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

// ---------------------------------------------------------------------------
// httpx 委托（统一响应信封 / 错误构造 / 上下文与分页辅助）
// ---------------------------------------------------------------------------

// 本段是 handler 与 internal/httpx 之间的唯一桥接层：
// 信封字段、错误码、HTTP 状态映射、context 键（请求 ID / 主体）全部来自 httpx，
// 本包不再自行实现，避免跨包语义漂移。
//
// 特别说明（曾出现的隐性缺陷）：context 键是**具名类型**，两个包各自定义
// `type ctxKey int` 时 `httpx.ctxKey(1) != handler.ctxKey(1)`，会导致
// handler 读不到中间件注入的主体而误报 401。因此主体与请求 ID 必须统一用
// httpx.SubjectFrom / httpx.RequestIDFrom 读取。

// 错误快捷构造：直接转发 httpx，保证业务码与状态映射唯一。
var (
	errBadRequest    = httpx.ErrBadRequest
	errUnauthorized  = httpx.ErrUnauthorized
	errForbidden     = httpx.ErrForbidden
	errNotFoundFn    = httpx.ErrNotFoundFn
	errConflictFn    = httpx.ErrConflictFn
	errUnprocessable = httpx.ErrUnprocessable
	errRateLimited   = httpx.ErrRateLimited
	errInternalFn    = httpx.ErrInternalFn
	errUnavailable   = httpx.ErrUnavailable
)

// newAPIError 构造带业务码的错误（转发 httpx.NewError）。
func newAPIError(code int, msg string, err error) *httpx.APIError {
	return httpx.NewError(code, msg, err)
}

// writeError 把任意层的错误映射为契约错误码后输出。
//
// httpx.WriteError（已冻结）只识别 *httpx.APIError，其余一律折叠为 500，
// 因此本包按任务要求补齐错误映射：
//   - 已是 *httpx.APIError（含被包装）→ 原样输出，保留业务码；
//   - store.ErrNotFound → 404；store.ErrConflict → 409；
//   - 超时 / 取消 → 503；限流配额关键词 → 429；越权禁止关键词 → 403；
//   - 未认证 → 401；冲突关键词 → 409；校验/Schema 非法关键词 → 422；
//   - 其余 → 500（消息脱敏后回显）。
//
// 说明：env / engine 等目标包尚未定稿，其导出错误变量在本包编译期不可依赖，
// 因此保留"错误文本兜底"分支；待目标包定稿后可平滑替换为 errors.Is 精确匹配。
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	// 配额/限流类错误统一带 Retry-After，让调用方按建议间隔退避重试，
	// 而不是立刻疯狂重试把额度打满（此类错误不会产生运行记录）。
	if isQuotaLimited(err) {
		w.Header().Set("Retry-After", retryAfterSeconds)
	}
	var apiErr *httpx.APIError
	if errors.As(err, &apiErr) {
		httpx.WriteError(w, r, apiErr)
		return
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, r, errNotFoundFn("资源不存在"))
	case errors.Is(err, store.ErrConflict):
		httpx.WriteError(w, r, errConflictFn("资源已存在或存在唯一键冲突"))
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		httpx.WriteError(w, r, errUnavailable("上游依赖超时或被取消", err))
	default:
		httpx.WriteError(w, r, mapByText(err))
	}
}

// retryAfterSeconds 配额/限流错误建议的重试等待秒数（Retry-After 响应头）。
const retryAfterSeconds = "5"

// isQuotaLimited 判断错误是否属于配额/限流类，判定口径与 mapByText 的配额分支一致。
func isQuotaLimited(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return containsAny(low, "quota", "rate limit", "too many requests", "配额", "限流", "超过上限")
}

// mapByText 按错误文本兜底映射（目标包未导出错误变量时的降级策略）。
func mapByText(err error) *httpx.APIError {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case containsAny(low, "quota", "rate limit", "too many requests", "配额", "限流", "超过上限"):
		return errRateLimited(sanitizeText(msg))
	case containsAny(low, "forbidden", "permission denied", "not allowed", "越权", "无权", "禁止访问", "跨租户", "租户不匹配"):
		return errForbidden(sanitizeText(msg))
	case containsAny(low, "unauthorized", "invalid credential", "token expired", "未认证", "凭证无效"):
		return errUnauthorized(sanitizeText(msg))
	case containsAny(low, "not found", "no such", "不存在"):
		return errNotFoundFn(sanitizeText(msg))
	case containsAny(low, "conflict", "already exists", "duplicate", "冲突", "已存在"):
		return errConflictFn(sanitizeText(msg))
	case containsAny(low, "schema", "invalid argument", "invalid input", "validation", "校验", "参数非法"):
		return errUnprocessable(sanitizeText(msg))
	case containsAny(low, "context deadline", "timeout", "timed out", "取消", "超时"):
		return errUnavailable(sanitizeText(msg), err)
	default:
		if looksLikeAPIError(err) {
			return newAPIError(err.(interface{ HTTPCode() int }).HTTPCode(), sanitizeText(msg), err)
		}
		return errInternalFn("内部错误", err)
	}
}

// looksLikeAPIError 判断错误是否自带 HTTPCode()int（兼容其他层自定义错误类型）。
func looksLikeAPIError(err error) bool {
	_, ok := err.(interface{ HTTPCode() int })
	return ok
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// unavailable 构造 503 降级错误（附原始错误）。
func unavailable(msg string, err error) *httpx.APIError { return httpx.ErrUnavailable(msg, err) }

// decodeBody 解析请求体（严格模式：未知字段报 400；受 Cfg.Server.MaxBodyBytes 限制）。
func decodeBody(d *Deps, r *http.Request, dst any) error {
	return httpx.DecodeJSON(r, dst, d.maxBody())
}

// defaultAnonymousTenant 免认证路径的缺省租户（与 httpx 匿名模式保持一致）。
const defaultAnonymousTenant = "t-demo"

// subject 取当前主体。
//
// 认证中间件对受保护路由一定注入了主体；但 /healthz、/readyz、/metrics 属于
// 免认证公开路径（见 internal/httpx 的 publicPaths），此时 context 中没有主体。
// 对这类路径按 X-Tenant-Id 头（缺省 t-demo）回退，保证 /metrics 仍能按租户聚合，
// 同时不改变任何受保护路由的鉴权语义。
func subject(r *http.Request) *domain.Subject {
	if s, ok := httpx.SubjectFrom(r.Context()); ok {
		return s
	}
	tenant := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
	if tenant == "" {
		tenant = defaultAnonymousTenant
	}
	return &domain.Subject{TenantID: tenant, TenantName: tenant}
}

// requestID 读取请求 ID（httpx.RequestIDMiddleware 注入）。
func requestID(r *http.Request) string { return httpx.RequestIDFrom(r.Context()) }

// pageQuery 解析分页查询参数（转发 httpx.PageQueryFrom）。
func pageQuery(r *http.Request) domain.PageQuery { return httpx.PageQueryFrom(r) }

// pathValue 读取路径参数（转发 httpx.PathValue）。
func pathValue(r *http.Request, name string) string { return httpx.PathValue(r, name) }

// requireScope 授权辅助：要求主体具备指定 scope（admin:all 通配），否则 403（转发 httpx）。
func requireScope(scope string) func(http.Handler) http.Handler { return httpx.RequireScope(scope) }

// nowISO 返回 RFC3339 时间字符串。
func nowISO() string { return time.Now().Format(time.RFC3339) }

// jsonRaw 安全序列化为 JSON 字符串（用于 readyz 明细与审计 Data 展示）。
func jsonRaw(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// codeUnavailable 503 业务码（与 httpx.CodeUnavailable 等值，供 newAPIError 复用）。
const codeUnavailable = httpx.CodeUnavailable

// sensitivePatterns 用于响应前兜底脱敏（凭证 / Token / 私钥）。
var sensitivePatterns = []string{
	"ca_live_", "ca_test_", "sk-", "ghp_", "glpat-", "Bearer ", "AKIA",
}

// sanitizeText 对错误文本脱敏并限长，避免把凭证或超大堆栈回显给调用方。
func sanitizeText(msg string) string {
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	low := strings.ToLower(msg)
	for _, p := range sensitivePatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return "请求处理失败（原始信息已脱敏，请查看服务端日志）"
		}
	}
	return msg
}

// fatal 包装不可恢复错误（装配期使用）。
func fatal(format string, args ...any) error { return fmt.Errorf(format, args...) }

// formatFloat 统一浮点输出，避免出现 NaN/Inf 破坏 JSON/Metrics。
func formatFloat(v float64) float64 {
	if v != v || v > 1e308 || v < -1e308 {
		return 0
	}
	return v
}

// boolStr 布尔转 "true"/"false"（Prometheus 标签用）。
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// begin 记录 handler 起始 debug 日志，返回 reqID 以便结束时复用。
func begin(d *Deps, r *http.Request, action string) string {
	reqID := requestID(r)
	d.log().Debug("handler.begin",
		"action", action,
		"method", r.Method,
		"path", r.URL.Path,
		"req", reqID,
		"tenant", subject(r).TenantID,
	)
	return reqID
}

// end 记录 handler 结束 debug 日志。
func end(d *Deps, r *http.Request, reqID, action string, err error) {
	if err != nil {
		d.log().Debug("handler.end", "action", action, "req", reqID, "err", err.Error())
		return
	}
	d.log().Debug("handler.end", "action", action, "req", reqID)
}
