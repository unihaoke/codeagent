// Package store 提供全系统统一的数据访问层。
//
// 默认提供"内存索引 + JSON 快照"实现：单机部署零依赖即可运行，
// 同时通过 Store 结构体暴露的方法集，可平滑替换为 PostgreSQL 等实现
// （只需保持方法签名不变，各层通过 store.Store 依赖注入）。
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codeagent/backend/internal/domain"
)

// MemStore 内存索引存储 + 可选 JSON 快照持久化（Store 的默认实现）。
type MemStore struct {
	mu sync.RWMutex

	tenants  map[string]*domain.Tenant
	apiKeys  map[string]*domain.APIKey
	creds    map[string]*domain.Credential
	repos    map[string]*domain.Repository
	groups   map[string]*domain.RepositoryGroup
	members  []domain.GroupMember
	tasks    map[string]*domain.Task
	runs     map[string]*domain.TaskRun
	reports  map[string]*domain.Report
	audits   []domain.AuditEvent
	skillLog []domain.SkillCall
	modelLog []domain.ModelCall
	// modelProviders AI 设置：模型提供方配置，按 provider name 唯一。
	modelProviders map[string]*domain.ModelProviderConfig
	// idemIndex 幂等键索引： tenantID + "|" + key → runID
	idemIndex map[string]string

	// 持久化
	dataFile string
	dirty    bool
	stopCh   chan struct{}
	stopped  bool
	snapMu   sync.Mutex
}

// 编译期校验：MemStore 必须完整实现 Store 契约。
var _ Store = (*MemStore)(nil)

// New 创建内存存储。
func New() Store {
	return &MemStore{
		tenants:        map[string]*domain.Tenant{},
		apiKeys:        map[string]*domain.APIKey{},
		creds:          map[string]*domain.Credential{},
		repos:          map[string]*domain.Repository{},
		groups:         map[string]*domain.RepositoryGroup{},
		members:        []domain.GroupMember{},
		tasks:          map[string]*domain.Task{},
		runs:           map[string]*domain.TaskRun{},
		reports:        map[string]*domain.Report{},
		audits:         []domain.AuditEvent{},
		skillLog:       []domain.SkillCall{},
		modelLog:       []domain.ModelCall{},
		modelProviders: map[string]*domain.ModelProviderConfig{},
		idemIndex:      map[string]string{},
	}
}

// ---------------------------------------------------------------------------
// 快照持久化
// ---------------------------------------------------------------------------

type snapshot struct {
	Version  int                       `json:"version"`
	SavedAt  time.Time                 `json:"savedAt"`
	Tenants  []*domain.Tenant          `json:"tenants"`
	APIKeys  []*domain.APIKey          `json:"apiKeys"`
	Creds    []*domain.Credential      `json:"credentials"`
	Repos    []*domain.Repository      `json:"repositories"`
	Groups   []*domain.RepositoryGroup `json:"groups"`
	Members  []domain.GroupMember      `json:"groupMembers"`
	Tasks    []*domain.Task            `json:"tasks"`
	Runs     []*domain.TaskRun         `json:"runs"`
	Reports  []*domain.Report          `json:"reports"`
	Audits   []domain.AuditEvent       `json:"audits,omitempty"`
	SkillLog []domain.SkillCall        `json:"skillCalls,omitempty"`
	ModelLog []domain.ModelCall        `json:"modelCalls,omitempty"`
	// ModelProviders AI 设置：控制台维护的模型提供方配置（密钥为密文）。
	ModelProviders []*domain.ModelProviderConfig `json:"modelProviders,omitempty"`
}

// OpenFile 以文件快照模式打开存储（文件不存在则新建）。
//
// 返回 Store 接口：调用方无需关心底层是内存还是共享存储，便于平滑切换。
func OpenFile(path string, snapshotInterval time.Duration) (Store, error) {
	s := New().(*MemStore)
	s.dataFile = path
	if path != "" {
		raw, err := os.ReadFile(path)
		if err == nil && len(raw) > 0 {
			var snap snapshot
			if err := json.Unmarshal(raw, &snap); err != nil {
				return nil, err
			}
			s.loadSnapshot(&snap)
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if path != "" && snapshotInterval > 0 {
		s.stopCh = make(chan struct{})
		go s.loop(snapshotInterval)
	}
	return s, nil
}

func (s *MemStore) loadSnapshot(snap *snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range snap.Tenants {
		s.tenants[v.ID] = v
	}
	for _, v := range snap.APIKeys {
		s.apiKeys[v.ID] = v
	}
	for _, v := range snap.Creds {
		s.creds[v.ID] = v
	}
	for _, v := range snap.Repos {
		s.repos[v.ID] = v
	}
	for _, v := range snap.Groups {
		s.groups[v.ID] = v
	}
	s.members = snap.Members
	for _, v := range snap.Tasks {
		s.tasks[v.ID] = v
	}
	for _, v := range snap.Runs {
		s.runs[v.ID] = v
		if v.IdempotencyKey != "" {
			s.idemIndex[v.TenantID+"|"+v.IdempotencyKey] = v.ID
		}
	}
	for _, v := range snap.Reports {
		s.reports[v.ID] = v
	}
	s.audits = snap.Audits
	s.skillLog = snap.SkillLog
	s.modelLog = snap.ModelLog
	for _, v := range snap.ModelProviders {
		s.modelProviders[v.Name] = v
	}
}

func (s *MemStore) loop(d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			_ = s.Flush()
		}
	}
}

// Flush 立即落盘（若配置了数据文件且存在未持久化变更）。
func (s *MemStore) Flush() error {
	s.mu.Lock()
	if s.dataFile == "" || !s.dirty {
		s.mu.Unlock()
		return nil
	}
	snap := s.snapshotLocked()
	s.dirty = false
	s.mu.Unlock()

	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.dataFile), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.dataFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.dataFile)
}

// Close 停止后台落盘并做最后一次 Flush。
func (s *MemStore) Close() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	if s.stopCh != nil {
		close(s.stopCh)
	}
	s.mu.Unlock()
	return s.Flush()
}

func (s *MemStore) snapshotLocked() *snapshot {
	snap := &snapshot{Version: 1, SavedAt: time.Now()}
	for _, v := range s.tenants {
		snap.Tenants = append(snap.Tenants, v)
	}
	for _, v := range s.apiKeys {
		snap.APIKeys = append(snap.APIKeys, v)
	}
	for _, v := range s.creds {
		snap.Creds = append(snap.Creds, v)
	}
	for _, v := range s.repos {
		snap.Repos = append(snap.Repos, v)
	}
	for _, v := range s.groups {
		snap.Groups = append(snap.Groups, v)
	}
	snap.Members = s.members
	for _, v := range s.tasks {
		snap.Tasks = append(snap.Tasks, v)
	}
	for _, v := range s.runs {
		snap.Runs = append(snap.Runs, v)
	}
	for _, v := range s.reports {
		snap.Reports = append(snap.Reports, v)
	}
	snap.Audits = tailAudits(s.audits, 8000)
	snap.SkillLog = tailSkillCalls(s.skillLog, 8000)
	snap.ModelLog = tailModelCalls(s.modelLog, 8000)
	for _, v := range s.modelProviders {
		snap.ModelProviders = append(snap.ModelProviders, v)
	}
	sort.Slice(snap.ModelProviders, func(i, j int) bool {
		return snap.ModelProviders[i].Name < snap.ModelProviders[j].Name
	})
	return snap
}

func tailAudits(in []domain.AuditEvent, n int) []domain.AuditEvent {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func tailSkillCalls(in []domain.SkillCall, n int) []domain.SkillCall {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func tailModelCalls(in []domain.ModelCall, n int) []domain.ModelCall {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func (s *MemStore) markDirty() { s.dirty = true }

// ---------------------------------------------------------------------------
// 租户
// ---------------------------------------------------------------------------

// CreateTenant 创建租户。
func (s *MemStore) CreateTenant(t *domain.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *t
	s.tenants[cp.ID] = &cp
	s.markDirty()
	return nil
}

// GetTenant 查询租户。
func (s *MemStore) GetTenant(id string) (*domain.Tenant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tenants[id]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// GetTenantByKey 按名称/ID 关键字查询租户（用于登录）。
func (s *MemStore) GetTenantByKey(key string) (*domain.Tenant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tenants {
		if t.ID == key || strings.EqualFold(t.Name, key) {
			cp := *t
			return &cp, true
		}
	}
	return nil, false
}

// UpdateTenant 更新租户。
func (s *MemStore) UpdateTenant(t *domain.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tenants[t.ID]; !ok {
		return ErrNotFound
	}
	cp := *t
	s.tenants[cp.ID] = &cp
	s.markDirty()
	return nil
}

// ListTenants 列出全部租户。
func (s *MemStore) ListTenants() []domain.Tenant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.Tenant, 0, len(s.tenants))
	for _, t := range s.tenants {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ---------------------------------------------------------------------------
// 模型提供方配置（AI 设置）
//
// 独立于配置文件：控制台在线维护的模型配置落在这里，随快照持久化，
// 启动时优先于配置文件中的 mcp.providers（见 cmd/server/main.go）。
// ---------------------------------------------------------------------------

// ListModelProviders 返回全部模型提供方配置（按名称升序）。
func (s *MemStore) ListModelProviders() []domain.ModelProviderConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.ModelProviderConfig, 0, len(s.modelProviders))
	for _, v := range s.modelProviders {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GetModelProvider 按名称读取模型提供方配置。
func (s *MemStore) GetModelProvider(name string) (*domain.ModelProviderConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.modelProviders[strings.TrimSpace(name)]
	if !ok {
		return nil, false
	}
	cp := *v
	return &cp, true
}

// SaveModelProvider 新增或更新模型提供方配置（按 Name 唯一）。
func (s *MemStore) SaveModelProvider(c *domain.ModelProviderConfig) error {
	if c == nil {
		return errors.New("store: 模型提供方配置不能为空")
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return errors.New("store: 模型提供方名称不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	cp.Name = name
	cp.UpdatedAt = time.Now()
	s.modelProviders[name] = &cp
	s.markDirty()
	return nil
}

// ReplaceModelProviders 全量替换模型提供方配置。
//
// AI 设置以提交列表为唯一真源：列表中不存在的旧配置会被删除。
// 空列表表示清空（模型层随之变为"未配置"状态）。
func (s *MemStore) ReplaceModelProviders(list []domain.ModelProviderConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]*domain.ModelProviderConfig, len(list))
	now := time.Now()
	for i := range list {
		name := strings.TrimSpace(list[i].Name)
		if name == "" {
			return errors.New("store: 模型提供方名称不能为空")
		}
		cp := list[i]
		cp.Name = name
		cp.UpdatedAt = now
		next[name] = &cp
	}
	s.modelProviders = next
	s.markDirty()
	return nil
}

// DeleteModelProvider 删除模型提供方配置；不存在返回 ErrNotFound。
func (s *MemStore) DeleteModelProvider(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if _, ok := s.modelProviders[name]; !ok {
		return ErrNotFound
	}
	delete(s.modelProviders, name)
	s.markDirty()
	return nil
}

// ---------------------------------------------------------------------------
// API Key
// ---------------------------------------------------------------------------

// GetAPIKey 按 ID 读取接入密钥。
//
// 用途：吊销时先判断"是否存在"，再判断"是否属于当前租户"，
// 使 404（资源不存在）与 403（越权）语义分离，避免排障时误判。
func (s *MemStore) GetAPIKey(id string) (*domain.APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.apiKeys[id]
	if !ok {
		return nil, false
	}
	cp := *v
	return &cp, true
}

// CreateAPIKey 创建接入密钥。
func (s *MemStore) CreateAPIKey(k *domain.APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *k
	s.apiKeys[cp.ID] = &cp
	s.markDirty()
	return nil
}

// FindAPIKeyByHash 按哈希查找密钥。
func (s *MemStore) FindAPIKeyByHash(hash string) (*domain.APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.apiKeys {
		if k.KeyHash == hash && !k.Revoked {
			cp := *k
			return &cp, true
		}
	}
	return nil, false
}

// ListAPIKeys 列出某租户密钥。
func (s *MemStore) ListAPIKeys(tenantID string) []domain.APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.APIKey{}
	for _, k := range s.apiKeys {
		if k.TenantID == tenantID {
			out = append(out, *k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// TouchAPIKey 记录密钥最近使用时间。
func (s *MemStore) TouchAPIKey(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.apiKeys[id]; ok {
		k.LastUsedAt = time.Now()
	}
}

// RevokeAPIKey 吊销密钥。
func (s *MemStore) RevokeAPIKey(tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.apiKeys[id]
	if !ok || k.TenantID != tenantID {
		return ErrNotFound
	}
	k.Revoked = true
	s.markDirty()
	return nil
}

// ---------------------------------------------------------------------------
// 凭证
// ---------------------------------------------------------------------------

// CreateCredential 创建凭证。
func (s *MemStore) CreateCredential(c *domain.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	s.creds[cp.ID] = &cp
	s.markDirty()
	return nil
}

// GetCredential 查询凭证。
func (s *MemStore) GetCredential(tenantID, id string) (*domain.Credential, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.creds[id]
	if !ok || c.TenantID != tenantID {
		return nil, false
	}
	cp := *c
	return &cp, true
}

// ListCredentials 列出凭证。
func (s *MemStore) ListCredentials(tenantID string) []domain.Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.Credential{}
	for _, c := range s.creds {
		if c.TenantID == tenantID {
			cp := *c
			cp.SecretEnc = "" // 密文绝不出存储层
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// UpdateCredential 更新凭证。
func (s *MemStore) UpdateCredential(c *domain.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.creds[c.ID]; !ok {
		return ErrNotFound
	}
	cp := *c
	s.creds[cp.ID] = &cp
	s.markDirty()
	return nil
}

// DeleteCredential 删除凭证。
func (s *MemStore) DeleteCredential(tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.creds[id]
	if !ok || c.TenantID != tenantID {
		return ErrNotFound
	}
	delete(s.creds, id)
	s.markDirty()
	return nil
}

// ---------------------------------------------------------------------------
// 仓库
// ---------------------------------------------------------------------------

// CreateRepo 创建仓库索引。
func (s *MemStore) CreateRepo(r *domain.Repository) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.repos {
		if e.TenantID == r.TenantID && strings.EqualFold(e.Key, r.Key) {
			return ErrConflict
		}
	}
	cp := *r
	s.repos[cp.ID] = &cp
	s.markDirty()
	return nil
}

// GetRepo 查询仓库。
func (s *MemStore) GetRepo(tenantID, id string) (*domain.Repository, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[id]
	if !ok || r.TenantID != tenantID {
		return nil, false
	}
	cp := *r
	return &cp, true
}

// GetRepoByKey 按业务键查询仓库。
func (s *MemStore) GetRepoByKey(tenantID, key string) (*domain.Repository, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.repos {
		if r.TenantID == tenantID && (strings.EqualFold(r.Key, key) || strings.EqualFold(r.Name, key)) {
			cp := *r
			return &cp, true
		}
	}
	return nil, false
}

// UpdateRepo 更新仓库。
func (s *MemStore) UpdateRepo(r *domain.Repository) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[r.ID]; !ok {
		return ErrNotFound
	}
	cp := *r
	s.repos[cp.ID] = &cp
	s.markDirty()
	return nil
}

// DeleteRepo 删除仓库及其分组成员关系。
func (s *MemStore) DeleteRepo(tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[id]
	if !ok || r.TenantID != tenantID {
		return ErrNotFound
	}
	delete(s.repos, id)
	kept := s.members[:0]
	for _, m := range s.members {
		if m.RepositoryID != id {
			kept = append(kept, m)
		}
	}
	s.members = append([]domain.GroupMember{}, kept...)
	s.markDirty()
	return nil
}

// ListRepos 列出仓库。
func (s *MemStore) ListRepos(tenantID string) []domain.Repository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.Repository{}
	for _, r := range s.repos {
		if r.TenantID == tenantID {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ---------------------------------------------------------------------------
// 分组
// ---------------------------------------------------------------------------

// CreateGroup 创建分组及成员关系。
func (s *MemStore) CreateGroup(g *domain.RepositoryGroup, members []domain.GroupMember) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.groups {
		if e.TenantID == g.TenantID && strings.EqualFold(e.Key, g.Key) {
			return ErrConflict
		}
	}
	cp := *g
	s.groups[cp.ID] = &cp
	for _, m := range members {
		if _, ok := s.repos[m.RepositoryID]; !ok {
			return ErrNotFound
		}
		m.GroupID = g.ID
		if m.CreatedAt.IsZero() {
			m.CreatedAt = time.Now()
		}
		s.members = append(s.members, m)
	}
	s.markDirty()
	return nil
}

// GetGroup 查询分组。
func (s *MemStore) GetGroup(tenantID, id string) (*domain.RepositoryGroup, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok || g.TenantID != tenantID {
		return nil, false
	}
	cp := *g
	return &cp, true
}

// UpdateGroup 更新分组与成员。
func (s *MemStore) UpdateGroup(g *domain.RepositoryGroup, members []domain.GroupMember) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[g.ID]; !ok {
		return ErrNotFound
	}
	cp := *g
	s.groups[cp.ID] = &cp
	if members != nil {
		kept := make([]domain.GroupMember, 0, len(s.members))
		for _, m := range s.members {
			if m.GroupID != g.ID {
				kept = append(kept, m)
			}
		}
		for _, m := range members {
			if _, ok := s.repos[m.RepositoryID]; !ok {
				return ErrNotFound
			}
			m.GroupID = g.ID
			if m.CreatedAt.IsZero() {
				m.CreatedAt = time.Now()
			}
			kept = append(kept, m)
		}
		s.members = kept
	}
	s.markDirty()
	return nil
}

// DeleteGroup 删除分组及成员关系。
func (s *MemStore) DeleteGroup(tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok || g.TenantID != tenantID {
		return ErrNotFound
	}
	delete(s.groups, id)
	kept := make([]domain.GroupMember, 0, len(s.members))
	for _, m := range s.members {
		if m.GroupID != id {
			kept = append(kept, m)
		}
	}
	s.members = kept
	s.markDirty()
	return nil
}

// ListGroups 列出分组。
func (s *MemStore) ListGroups(tenantID string) []domain.RepositoryGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.RepositoryGroup{}
	for _, g := range s.groups {
		if g.TenantID == tenantID {
			out = append(out, *g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// GroupMemberViews 返回分组成员聚合视图（按 Order 升序）。
func (s *MemStore) GroupMemberViews(tenantID, groupID string) []domain.GroupMemberView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.GroupMemberView{}
	for _, m := range s.members {
		if m.GroupID != groupID {
			continue
		}
		v := domain.GroupMemberView{GroupMember: m}
		if r, ok := s.repos[m.RepositoryID]; ok && r.TenantID == tenantID {
			cp := *r
			v.Repo = &cp
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// GroupMemberRepoIDs 返回分组内仓库 ID 列表。
func (s *MemStore) GroupMemberRepoIDs(groupID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []string{}
	for _, m := range s.members {
		if m.GroupID == groupID {
			out = append(out, m.RepositoryID)
		}
	}
	return out
}

// GroupsOfRepo 反查仓库所属分组。
func (s *MemStore) GroupsOfRepo(tenantID, repoID string) []domain.RepositoryGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.RepositoryGroup{}
	for _, m := range s.members {
		if m.RepositoryID != repoID {
			continue
		}
		if g, ok := s.groups[m.GroupID]; ok && g.TenantID == tenantID {
			out = append(out, *g)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 任务与运行
// ---------------------------------------------------------------------------

// CreateTask 创建逻辑任务。
func (s *MemStore) CreateTask(t *domain.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *t
	s.tasks[cp.ID] = &cp
	s.markDirty()
	return nil
}

// UpdateTask 更新逻辑任务。
func (s *MemStore) UpdateTask(t *domain.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[t.ID]; !ok {
		return ErrNotFound
	}
	cp := *t
	s.tasks[cp.ID] = &cp
	s.markDirty()
	return nil
}

// GetTask 查询任务。
func (s *MemStore) GetTask(tenantID, id string) (*domain.Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok || t.TenantID != tenantID {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// ListTasks 列出任务（按更新时间倒序）。
func (s *MemStore) ListTasks(tenantID string, q domain.PageQuery) domain.Page[domain.Task] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q.Normalize()
	all := []domain.Task{}
	for _, t := range s.tasks {
		if t.TenantID != tenantID {
			continue
		}
		if q.Keyword != "" && !containsFold(t.Title, q.Keyword) && !containsFold(t.ID, q.Keyword) {
			continue
		}
		all = append(all, *t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
	return paginate(all, q)
}

// CreateRun 创建执行记录。
func (s *MemStore) CreateRun(r *domain.TaskRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *r
	cp.PinnedCommits = cloneMap(r.PinnedCommits)
	s.runs[cp.ID] = &cp
	if cp.IdempotencyKey != "" {
		s.idemIndex[cp.TenantID+"|"+cp.IdempotencyKey] = cp.ID
	}
	s.markDirty()
	return nil
}

// UpdateRun 更新执行记录（写入最新的任务上下文快照）。
func (s *MemStore) UpdateRun(r *domain.TaskRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[r.ID]; !ok {
		return ErrNotFound
	}
	r.UpdatedAt = time.Now()
	cp := *r
	// 深拷贝切片/映射字段，防止外部修改污染存储。
	cp.PinnedCommits = cloneMap(r.PinnedCommits)
	cp.Resolution = append([]domain.RepoResolved{}, r.Resolution...)
	cp.Patches = append([]domain.Patch{}, r.Patches...)
	cp.Warnings = append([]string{}, r.Warnings...)
	cp.RepoIDs = append([]string{}, r.RepoIDs...)
	s.runs[cp.ID] = &cp
	s.markDirty()
	return nil
}

// GetRun 查询执行记录。
func (s *MemStore) GetRun(tenantID, id string) (*domain.TaskRun, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	if !ok || r.TenantID != tenantID {
		return nil, false
	}
	return cloneRun(r), true
}

// GetRunRaw 无租户校验查询（内部调度使用）。
func (s *MemStore) GetRunRaw(id string) (*domain.TaskRun, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, false
	}
	return cloneRun(r), true
}

// FindRunByIdempotencyKey 幂等键查找未完成/已完成的执行记录。
func (s *MemStore) FindRunByIdempotencyKey(tenantID, key string) (*domain.TaskRun, bool) {
	if key == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.idemIndex[tenantID+"|"+key]
	if !ok {
		return nil, false
	}
	r, ok := s.runs[id]
	if !ok {
		return nil, false
	}
	cp := *r
	return &cp, true
}

// ListRuns 列出执行记录。
func (s *MemStore) ListRuns(tenantID string, q domain.PageQuery) domain.Page[domain.TaskRun] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q.Normalize()
	all := []domain.TaskRun{}
	for _, r := range s.runs {
		if r.TenantID != tenantID {
			continue
		}
		if q.State != "" && string(r.State) != q.State {
			continue
		}
		if q.Keyword != "" && !containsFold(r.Title, q.Keyword) && !containsFold(r.ID, q.Keyword) &&
			!containsFold(r.Stacktrace, q.Keyword) {
			continue
		}
		all = append(all, *r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	return paginate(all, q)
}

// AllRuns 返回某租户全部执行记录（统计用）。
func (s *MemStore) AllRuns(tenantID string) []domain.TaskRun {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.TaskRun{}
	for _, r := range s.runs {
		if r.TenantID == tenantID {
			out = append(out, *r)
		}
	}
	return out
}

// ListQueued 列出仍处于排队中的运行（按入队先后）。
//
// 用途：进程重启后恢复排队任务、多实例部署时认领待执行任务。
func (s *MemStore) ListQueued(limit int) []domain.TaskRun {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.TaskRun{}
	for _, r := range s.runs {
		if r.State != domain.StateQueued {
			continue
		}
		out = append(out, *cloneRun(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// CountRunsByState 统计某租户各状态数量。
func (s *MemStore) CountRunsByState(tenantID string) map[domain.TaskState]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[domain.TaskState]int{}
	for _, r := range s.runs {
		if r.TenantID == tenantID {
			out[r.State]++
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 报告
// ---------------------------------------------------------------------------

// CreateReport 归档报告。
func (s *MemStore) CreateReport(r *domain.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *r
	s.reports[cp.ID] = &cp
	s.markDirty()
	return nil
}

// GetReport 查询报告。
func (s *MemStore) GetReport(tenantID, id string) (*domain.Report, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.reports[id]
	if !ok || r.TenantID != tenantID {
		return nil, false
	}
	cp := *r
	return &cp, true
}

// GetReportByRun 按运行 ID 查询报告。
func (s *MemStore) GetReportByRun(tenantID, runID string) (*domain.Report, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.reports {
		if r.TenantID == tenantID && r.RunID == runID {
			cp := *r
			return &cp, true
		}
	}
	return nil, false
}

// ListReports 列出报告。
func (s *MemStore) ListReports(tenantID string, q domain.PageQuery) domain.Page[domain.Report] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q.Normalize()
	all := []domain.Report{}
	for _, r := range s.reports {
		if r.TenantID != tenantID {
			continue
		}
		if q.Keyword != "" && !containsFold(r.Title, q.Keyword) && !containsFold(r.Summary, q.Keyword) {
			continue
		}
		all = append(all, *r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	return paginate(all, q)
}

// ---------------------------------------------------------------------------
// 审计与调用日志
// ---------------------------------------------------------------------------

// AppendAudit 追加审计事件（内存上限内保留）。
func (s *MemStore) AppendAudit(ev domain.AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, ev)
	if len(s.audits) > 20000 {
		s.audits = s.audits[len(s.audits)-20000:]
	}
	s.markDirty()
}

// AppendSkillCall 追加技能调用记录。
func (s *MemStore) AppendSkillCall(c domain.SkillCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skillLog = append(s.skillLog, c)
	if len(s.skillLog) > 20000 {
		s.skillLog = s.skillLog[len(s.skillLog)-20000:]
	}
	s.markDirty()
}

// AppendModelCall 追加模型调用记录。
func (s *MemStore) AppendModelCall(c domain.ModelCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelLog = append(s.modelLog, c)
	if len(s.modelLog) > 20000 {
		s.modelLog = s.modelLog[len(s.modelLog)-20000:]
	}
	s.markDirty()
}

// ListAudits 查询审计事件。
func (s *MemStore) ListAudits(tenantID string, q domain.PageQuery) domain.Page[domain.AuditEvent] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q.Normalize()
	all := []domain.AuditEvent{}
	for i := len(s.audits) - 1; i >= 0; i-- {
		v := s.audits[i]
		if v.TenantID != tenantID {
			continue
		}
		if q.State != "" && v.Category != q.State {
			continue
		}
		if q.Keyword != "" && !containsFold(v.Message, q.Keyword) && !containsFold(v.Action, q.Keyword) {
			continue
		}
		all = append(all, v)
	}
	return paginate(all, q)
}

// ListSkillCalls 查询技能调用记录。
func (s *MemStore) ListSkillCalls(tenantID, runID string, q domain.PageQuery) domain.Page[domain.SkillCall] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q.Normalize()
	all := []domain.SkillCall{}
	for i := len(s.skillLog) - 1; i >= 0; i-- {
		v := s.skillLog[i]
		if v.TenantID != tenantID {
			continue
		}
		if runID != "" && v.RunID != runID {
			continue
		}
		if q.State != "" && string(v.Status) != q.State {
			continue
		}
		if q.Keyword != "" && !containsFold(v.Skill, q.Keyword) {
			continue
		}
		all = append(all, v)
	}
	return paginate(all, q)
}

// ListModelCalls 查询模型调用记录。
func (s *MemStore) ListModelCalls(tenantID, runID string, q domain.PageQuery) domain.Page[domain.ModelCall] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q.Normalize()
	all := []domain.ModelCall{}
	for i := len(s.modelLog) - 1; i >= 0; i-- {
		v := s.modelLog[i]
		if v.TenantID != tenantID {
			continue
		}
		if runID != "" && v.RunID != runID {
			continue
		}
		if q.State != "" && string(v.Status) != q.State {
			continue
		}
		if q.Keyword != "" && !containsFold(v.Model, q.Keyword) && !containsFold(v.Stage, q.Keyword) {
			continue
		}
		all = append(all, v)
	}
	return paginate(all, q)
}

// AllSkillCalls 返回某租户全部技能调用（统计用）。
func (s *MemStore) AllSkillCalls(tenantID string) []domain.SkillCall {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.SkillCall{}
	for _, v := range s.skillLog {
		if v.TenantID == tenantID {
			out = append(out, v)
		}
	}
	return out
}

// AllModelCalls 返回某租户全部模型调用（统计用）。
func (s *MemStore) AllModelCalls(tenantID string) []domain.ModelCall {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.ModelCall{}
	for _, v := range s.modelLog {
		if v.TenantID == tenantID {
			out = append(out, v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// cloneRun 深拷贝执行记录的可变字段，避免调用方修改污染存储。
func cloneRun(r *domain.TaskRun) *domain.TaskRun {
	cp := *r
	cp.PinnedCommits = cloneMap(r.PinnedCommits)
	cp.Resolution = append([]domain.RepoResolved{}, r.Resolution...)
	cp.Patches = append([]domain.Patch{}, r.Patches...)
	cp.Warnings = append([]string{}, r.Warnings...)
	cp.RepoIDs = append([]string{}, r.RepoIDs...)
	cp.EntryFiles = append([]string{}, r.EntryFiles...)
	return &cp
}

func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func paginate[T any](all []T, q domain.PageQuery) domain.Page[T] {
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
