package store

import "github.com/codeagent/backend/internal/domain"

// ---------------------------------------------------------------------------
// 数据访问层契约
//
// 背景：原先各层直接依赖 store.Store（内存 map 实现），进程内存是唯一真源，
// 因此无法多实例部署 —— 多副本各自持有一份互不可见的状态，任务会在不同实例上
// 重复执行，控制台看到的列表也随实例漂移。
//
// 本文件把数据访问层抽象为接口：
//   - MemStore：内存索引 + JSON 快照（零依赖，单机默认）；
//   - SQLStore：基于 database/sql 的共享存储（PostgreSQL / MySQL / SQLite），
//     多实例共享同一份数据，配合排队任务的原子认领即可水平扩展。
//
// 各层只依赖 Store 接口，替换实现无需改动上层代码。
// ---------------------------------------------------------------------------

// Store 统一数据访问契约。
//
// 实现约定：
//  1. 所有 Get/List 返回对象副本，调用方修改不得污染存储；
//  2. 所有写操作对并发调用安全；
//  3. 未经事务封装的"读-改-写"由调用方保证串行（如引擎内单个 run 的状态迁移）。
type Store interface {
	// ---------------------------------------------------------------- 租户
	CreateTenant(t *domain.Tenant) error
	GetTenant(id string) (*domain.Tenant, bool)
	GetTenantByKey(key string) (*domain.Tenant, bool)
	UpdateTenant(t *domain.Tenant) error
	ListTenants() []domain.Tenant

	// ---------------------------------------------------- 模型提供方配置
	ListModelProviders() []domain.ModelProviderConfig
	GetModelProvider(name string) (*domain.ModelProviderConfig, bool)
	SaveModelProvider(c *domain.ModelProviderConfig) error
	ReplaceModelProviders(list []domain.ModelProviderConfig) error
	DeleteModelProvider(name string) error

	// ------------------------------------------------------------ API Key
	GetAPIKey(id string) (*domain.APIKey, bool)
	CreateAPIKey(k *domain.APIKey) error
	FindAPIKeyByHash(hash string) (*domain.APIKey, bool)
	ListAPIKeys(tenantID string) []domain.APIKey
	TouchAPIKey(id string)
	RevokeAPIKey(tenantID, id string) error

	// -------------------------------------------------------------- 凭证
	CreateCredential(c *domain.Credential) error
	GetCredential(tenantID, id string) (*domain.Credential, bool)
	ListCredentials(tenantID string) []domain.Credential
	UpdateCredential(c *domain.Credential) error
	DeleteCredential(tenantID, id string) error

	// -------------------------------------------------------------- 仓库
	CreateRepo(r *domain.Repository) error
	GetRepo(tenantID, id string) (*domain.Repository, bool)
	GetRepoByKey(tenantID, key string) (*domain.Repository, bool)
	UpdateRepo(r *domain.Repository) error
	DeleteRepo(tenantID, id string) error
	ListRepos(tenantID string) []domain.Repository

	// -------------------------------------------------------------- 分组
	CreateGroup(g *domain.RepositoryGroup, members []domain.GroupMember) error
	GetGroup(tenantID, id string) (*domain.RepositoryGroup, bool)
	UpdateGroup(g *domain.RepositoryGroup, members []domain.GroupMember) error
	DeleteGroup(tenantID, id string) error
	ListGroups(tenantID string) []domain.RepositoryGroup
	GroupMemberViews(tenantID, groupID string) []domain.GroupMemberView
	GroupMemberRepoIDs(groupID string) []string
	GroupsOfRepo(tenantID, repoID string) []domain.RepositoryGroup

	// ---------------------------------------------------------- 任务与运行
	CreateTask(t *domain.Task) error
	UpdateTask(t *domain.Task) error
	GetTask(tenantID, id string) (*domain.Task, bool)
	ListTasks(tenantID string, q domain.PageQuery) domain.Page[domain.Task]

	CreateRun(r *domain.TaskRun) error
	UpdateRun(r *domain.TaskRun) error
	GetRun(tenantID, id string) (*domain.TaskRun, bool)
	// GetRunRaw 无租户校验查询（内部调度使用）。
	GetRunRaw(id string) (*domain.TaskRun, bool)
	FindRunByIdempotencyKey(tenantID, key string) (*domain.TaskRun, bool)
	ListRuns(tenantID string, q domain.PageQuery) domain.Page[domain.TaskRun]
	AllRuns(tenantID string) []domain.TaskRun
	CountRunsByState(tenantID string) map[domain.TaskState]int
	// ListQueued 列出仍处于排队中的运行（按入队先后），供重启恢复与多实例认领。
	ListQueued(limit int) []domain.TaskRun

	// -------------------------------------------------------------- 报告
	CreateReport(r *domain.Report) error
	GetReport(tenantID, id string) (*domain.Report, bool)
	GetReportByRun(tenantID, runID string) (*domain.Report, bool)
	ListReports(tenantID string, q domain.PageQuery) domain.Page[domain.Report]

	// ---------------------------------------------------- 审计与调用日志
	AppendAudit(ev domain.AuditEvent)
	AppendSkillCall(c domain.SkillCall)
	AppendModelCall(c domain.ModelCall)
	ListAudits(tenantID string, q domain.PageQuery) domain.Page[domain.AuditEvent]
	ListSkillCalls(tenantID, runID string, q domain.PageQuery) domain.Page[domain.SkillCall]
	ListModelCalls(tenantID, runID string, q domain.PageQuery) domain.Page[domain.ModelCall]
	AllSkillCalls(tenantID string) []domain.SkillCall
	AllModelCalls(tenantID string) []domain.ModelCall

	// ------------------------------------------------------------ 生命周期
	// Flush 立即落盘（内存实现写快照；共享存储实现为空操作）。
	Flush() error
	// Close 释放资源并做最后一次落盘。
	Close() error
}
