package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/codeagent/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// 共享存储实现（SQL）
//
// 背景：MemStore 以进程内存为唯一真源，无法多实例部署（多副本状态互不可见，
// 任务会重复执行、控制台列表随实例漂移）。SQLStore 把状态落到共享数据库，
// 多实例访问同一份数据，从而支持水平扩展。
//
// 存储模型：索引列 + payload(JSON)。
//   - 索引列承载查询/排序/过滤（tenant_id、state、priority、created_ms …），
//     保证列表、统计、排队扫描都能走索引；
//   - 完整领域对象序列化进 payload，避免为每个字段建列带来的模型耦合，
//     领域模型演进（新增字段）无需改表。
//
// 方言兼容：PostgreSQL / MySQL / SQLite 三方言，差异集中在占位符与 payload 类型上。
// ---------------------------------------------------------------------------

// SQLStore 基于 database/sql 的共享存储实现。
type SQLStore struct {
	db      *sql.DB
	dialect string
	// ownDB 是否由本实现打开的连接（决定 Close 是否关闭底层连接池）。
	ownDB bool
}

// 编译期校验：SQLStore 必须完整实现 Store 契约。
var _ Store = (*SQLStore)(nil)

// 编译期校验：SQLStore 支持排队任务的原子认领（多实例调度）。
var _ QueueClaimer = (*SQLStore)(nil)

// QueueClaimer 可选扩展：支持"排队任务原子认领"的存储实现。
//
// 多实例部署时，各实例通过认领把 queued 任务抢占到自己名下，
// 保证同一任务不会被两个实例同时执行。
type QueueClaimer interface {
	// ClaimQueued 原子认领至多 limit 个排队任务：把它们的 state 从 queued 迁移到
	// analyzing 并返回认领到的运行。返回空切片表示没有可认领的任务。
	ClaimQueued(limit int) []domain.TaskRun
}

// OpenSQL 打开共享存储（驱动需由调用方注册后传入 driverName）。
//
// driverName 例如 "postgres" / "mysql" / "sqlite"；dialect 为空时按驱动名推断。
func OpenSQL(driverName, dsn, dialect string) (Store, error) {
	if strings.TrimSpace(driverName) == "" {
		return nil, errors.New("store: SQL 驱动名不能为空")
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败：%w", err)
	}
	st := &SQLStore{db: db, dialect: normalizeDialect(dialect, driverName), ownDB: true}
	if err := st.EnsureSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

// NewSQL 基于已有 *sql.DB 构造共享存储（连接池由调用方管理）。
func NewSQL(db *sql.DB, dialect string) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("store: *sql.DB 不能为空")
	}
	st := &SQLStore{db: db, dialect: normalizeDialect(dialect, ""), ownDB: false}
	if err := st.EnsureSchema(context.Background()); err != nil {
		return nil, err
	}
	return st, nil
}

// DB 返回底层连接池（供健康检查与运维使用）。
func (s *SQLStore) DB() *sql.DB { return s.db }

func normalizeDialect(dialect, driver string) string {
	d := strings.ToLower(strings.TrimSpace(dialect))
	if d == "" {
		d = strings.ToLower(strings.TrimSpace(driver))
	}
	switch {
	case strings.Contains(d, "postgres"), strings.Contains(d, "pgx"):
		return "postgres"
	case strings.Contains(d, "mysql"), strings.Contains(d, "mariadb"):
		return "mysql"
	default:
		return "sqlite"
	}
}

// payloadType 返回 payload 列类型：MySQL 的 TEXT 仅 64KB，需 MEDIUMTEXT 承载运行上下文。
func (s *SQLStore) payloadType() string {
	if s.dialect == "mysql" {
		return "MEDIUMTEXT"
	}
	return "TEXT"
}

// ph 占位符归一化：PostgreSQL 需要 $1/$2 形式，其余方言使用 ?。
func (s *SQLStore) ph(query string) string {
	if s.dialect != "postgres" {
		return query
	}
	var b strings.Builder
	i := 0
	for _, r := range query {
		if r == '?' {
			i++
			b.WriteString("$")
			b.WriteString(strconv.Itoa(i))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *SQLStore) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.ph(query), args...)
}

func (s *SQLStore) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.ph(query), args...)
}

func (s *SQLStore) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.ph(query), args...)
}

// ---------------------------------------------------------------------------
// 建表
// ---------------------------------------------------------------------------

// schema 建表语句（IF NOT EXISTS，重复启动幂等）。
func (s *SQLStore) schema() []string {
	p := s.payloadType()
	return []string{
		`CREATE TABLE IF NOT EXISTS ca_tenants (
			id VARCHAR(190) PRIMARY KEY, name VARCHAR(255), created_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_api_keys (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), key_hash VARCHAR(255),
			revoked INTEGER DEFAULT 0, created_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_credentials (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), created_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_repositories (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), repo_key VARCHAR(255),
			created_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_groups (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), group_key VARCHAR(255),
			created_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_group_members (
			group_id VARCHAR(190), repository_id VARCHAR(190), order_idx INTEGER,
			created_ms INTEGER, payload ` + p + `, PRIMARY KEY (group_id, repository_id))`,
		`CREATE TABLE IF NOT EXISTS ca_tasks (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), idempotency_key VARCHAR(190),
			updated_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_runs (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), task_id VARCHAR(190),
			state VARCHAR(64), priority INTEGER DEFAULT 0, idempotency_key VARCHAR(190),
			created_ms INTEGER, updated_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_reports (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), run_id VARCHAR(190),
			created_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_model_providers (
			name VARCHAR(190) PRIMARY KEY, updated_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_audits (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), run_id VARCHAR(190),
			category VARCHAR(64), action VARCHAR(128), level VARCHAR(16),
			at_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_skill_calls (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), run_id VARCHAR(190),
			skill VARCHAR(128), status VARCHAR(32), at_ms INTEGER, payload ` + p + `)`,
		`CREATE TABLE IF NOT EXISTS ca_model_calls (
			id VARCHAR(190) PRIMARY KEY, tenant_id VARCHAR(190), run_id VARCHAR(190),
			model VARCHAR(128), status VARCHAR(32), at_ms INTEGER, payload ` + p + `)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_tenant_state ON ca_runs (tenant_id, state)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_state ON ca_runs (state, priority DESC, created_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_idem ON ca_runs (tenant_id, idempotency_key)`,
		`CREATE INDEX IF NOT EXISTS idx_reports_run ON ca_reports (tenant_id, run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audits_tenant ON ca_audits (tenant_id, at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_skill_calls ON ca_skill_calls (tenant_id, run_id, at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_model_calls ON ca_model_calls (tenant_id, run_id, at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_repos_tenant ON ca_repositories (tenant_id)`,
		`CREATE INDEX IF NOT EXISTS idx_groups_tenant ON ca_groups (tenant_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_tenant ON ca_tasks (tenant_id, updated_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON ca_api_keys (tenant_id)`,
		`CREATE INDEX IF NOT EXISTS idx_creds_tenant ON ca_credentials (tenant_id)`,
	}
}

// EnsureSchema 建表与索引（幂等）。
func (s *SQLStore) EnsureSchema(ctx context.Context) error {
	for _, ddl := range s.schema() {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("建表失败：%w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 通用读写助手
// ---------------------------------------------------------------------------

// kv 一个索引列的值。
type kv struct {
	k string
	v any
}

// ms 时间 → 毫秒时间戳（零值记 0，排序时排在最前）。
func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// put 写入（存在则更新，不存在则插入）一行。
func (s *SQLStore) put(ctx context.Context, table, idCol, id string, cols []kv, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化 %s 失败：%w", table, err)
	}
	sets := make([]string, 0, len(cols)+1)
	args := make([]any, 0, len(cols)+2)
	for _, c := range cols {
		sets = append(sets, c.k+"=?")
		args = append(args, c.v)
	}
	sets = append(sets, "payload=?")
	args = append(args, raw)
	args = append(args, id)

	res, err := s.exec(ctx, "UPDATE "+table+" SET "+strings.Join(sets, ",")+" WHERE "+idCol+"=?", args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	names := []string{idCol}
	holders := []string{"?"}
	ins := []any{id}
	for _, c := range cols {
		names = append(names, c.k)
		holders = append(holders, "?")
		ins = append(ins, c.v)
	}
	names = append(names, "payload")
	holders = append(holders, "?")
	ins = append(ins, raw)
	_, err = s.exec(ctx, "INSERT INTO "+table+" ("+strings.Join(names, ",")+") VALUES ("+strings.Join(holders, ",")+")", ins...)
	return err
}

// get 按主键读取 payload 并反序列化。
func (s *SQLStore) get(ctx context.Context, table, idCol, id string, out any) (bool, error) {
	if id == "" {
		return false, nil
	}
	var raw []byte
	err := s.queryRow(ctx, "SELECT payload FROM "+table+" WHERE "+idCol+"=?", id).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("反序列化 %s 失败：%w", table, err)
	}
	return true, nil
}

// del 按主键删除。
func (s *SQLStore) del(ctx context.Context, table, idCol, id string) error {
	_, err := s.exec(ctx, "DELETE FROM "+table+" WHERE "+idCol+"=?", id)
	return err
}

// exists 判断主键是否存在。
func (s *SQLStore) exists(ctx context.Context, table, idCol, id string) (bool, error) {
	var one int
	err := s.queryRow(ctx, "SELECT 1 FROM "+table+" WHERE "+idCol+"=?", id).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// count 统计符合条件的行数。
func (s *SQLStore) count(ctx context.Context, table, where string, args ...any) (int, error) {
	q := "SELECT COUNT(*) FROM " + table
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	if err := s.queryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// payloads 查询多行 payload；orderBy 为空表示不排序。
func (s *SQLStore) payloads(ctx context.Context, table, where, orderBy string, args []any, limit int) ([][]byte, error) {
	q := "SELECT payload FROM " + table
	if where != "" {
		q += " WHERE " + where
	}
	if orderBy != "" {
		q += " ORDER BY " + orderBy
	}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		cp := append([]byte{}, raw...)
		out = append(out, cp)
	}
	return out, rows.Err()
}

// likeEscape 转义 LIKE 通配符，避免关键字中的 % _ 被当作模式。
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// keywordClause 生成关键字过滤条件：SQL 侧用 LIKE 粗筛（大小写不敏感），
// 调用方再用 containsFold 精筛，兼顾跨方言与语义一致。
func keywordClause(col, keyword string) (string, []any) {
	if strings.TrimSpace(keyword) == "" {
		return "", nil
	}
	pattern := "%" + likeEscape(keyword) + "%"
	return "LOWER(" + col + ") LIKE LOWER(?) ESCAPE '\\'", []any{pattern}
}

func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// newID 生成主键（领域对象未带 ID 时兜底）。
func newID(v string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return uuid.NewString()
}

// ---------------------------------------------------------------------------
// 租户
// ---------------------------------------------------------------------------

func (s *SQLStore) CreateTenant(t *domain.Tenant) error {
	if t == nil {
		return errors.New("store: 租户不能为空")
	}
	return s.put(context.Background(), "ca_tenants", "id", newID(t.ID),
		[]kv{{"name", t.Name}, {"created_ms", ms(t.CreatedAt)}}, t)
}

func (s *SQLStore) GetTenant(id string) (*domain.Tenant, bool) {
	var out domain.Tenant
	ok, err := s.get(context.Background(), "ca_tenants", "id", id, &out)
	if err != nil || !ok {
		return nil, false
	}
	return &out, true
}

func (s *SQLStore) GetTenantByKey(key string) (*domain.Tenant, bool) {
	if key == "" {
		return nil, false
	}
	if t, ok := s.GetTenant(key); ok {
		return t, true
	}
	rows, err := s.payloads(context.Background(), "ca_tenants", "", "", nil, 0)
	if err != nil {
		return nil, false
	}
	for _, raw := range rows {
		var t domain.Tenant
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		if strings.EqualFold(t.Name, key) {
			return &t, true
		}
	}
	return nil, false
}

func (s *SQLStore) UpdateTenant(t *domain.Tenant) error {
	if t == nil {
		return errors.New("store: 租户不能为空")
	}
	ok, err := s.exists(context.Background(), "ca_tenants", "id", t.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return s.put(context.Background(), "ca_tenants", "id", t.ID,
		[]kv{{"name", t.Name}, {"created_ms", ms(t.CreatedAt)}}, t)
}

func (s *SQLStore) ListTenants() []domain.Tenant {
	rows, err := s.payloads(context.Background(), "ca_tenants", "", "created_ms ASC", nil, 0)
	if err != nil {
		return []domain.Tenant{}
	}
	out := make([]domain.Tenant, 0, len(rows))
	for _, raw := range rows {
		var t domain.Tenant
		if err := json.Unmarshal(raw, &t); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 模型提供方配置
// ---------------------------------------------------------------------------

func (s *SQLStore) ListModelProviders() []domain.ModelProviderConfig {
	rows, err := s.payloads(context.Background(), "ca_model_providers", "", "name ASC", nil, 0)
	if err != nil {
		return []domain.ModelProviderConfig{}
	}
	out := make([]domain.ModelProviderConfig, 0, len(rows))
	for _, raw := range rows {
		var v domain.ModelProviderConfig
		if err := json.Unmarshal(raw, &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func (s *SQLStore) GetModelProvider(name string) (*domain.ModelProviderConfig, bool) {
	var v domain.ModelProviderConfig
	ok, err := s.get(context.Background(), "ca_model_providers", "name", strings.TrimSpace(name), &v)
	if err != nil || !ok {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) SaveModelProvider(c *domain.ModelProviderConfig) error {
	if c == nil {
		return errors.New("store: 模型提供方配置不能为空")
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return errors.New("store: 模型提供方名称不能为空")
	}
	cp := *c
	cp.Name = name
	cp.UpdatedAt = time.Now()
	return s.put(context.Background(), "ca_model_providers", "name", name,
		[]kv{{"updated_ms", ms(cp.UpdatedAt)}}, cp)
}

func (s *SQLStore) ReplaceModelProviders(list []domain.ModelProviderConfig) error {
	ctx := context.Background()
	if _, err := s.exec(ctx, "DELETE FROM ca_model_providers"); err != nil {
		return err
	}
	now := time.Now()
	for i := range list {
		name := strings.TrimSpace(list[i].Name)
		if name == "" {
			return errors.New("store: 模型提供方名称不能为空")
		}
		cp := list[i]
		cp.Name = name
		cp.UpdatedAt = now
		if err := s.put(ctx, "ca_model_providers", "name", name, []kv{{"updated_ms", ms(now)}}, cp); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLStore) DeleteModelProvider(name string) error {
	name = strings.TrimSpace(name)
	ok, err := s.exists(context.Background(), "ca_model_providers", "name", name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return s.del(context.Background(), "ca_model_providers", "name", name)
}

// ---------------------------------------------------------------------------
// API Key
// ---------------------------------------------------------------------------

func (s *SQLStore) GetAPIKey(id string) (*domain.APIKey, bool) {
	var v domain.APIKey
	ok, err := s.get(context.Background(), "ca_api_keys", "id", id, &v)
	if err != nil || !ok {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) CreateAPIKey(k *domain.APIKey) error {
	if k == nil {
		return errors.New("store: 接入密钥不能为空")
	}
	return s.put(context.Background(), "ca_api_keys", "id", newID(k.ID),
		[]kv{{"tenant_id", k.TenantID}, {"key_hash", k.KeyHash}, {"revoked", boolInt(k.Revoked)}, {"created_ms", ms(k.CreatedAt)}}, k)
}

func (s *SQLStore) FindAPIKeyByHash(hash string) (*domain.APIKey, bool) {
	rows, err := s.payloads(context.Background(), "ca_api_keys", "key_hash=?", "", []any{hash}, 0)
	if err != nil {
		return nil, false
	}
	for _, raw := range rows {
		var k domain.APIKey
		if err := json.Unmarshal(raw, &k); err != nil {
			continue
		}
		if k.KeyHash == hash && !k.Revoked {
			return &k, true
		}
	}
	return nil, false
}

func (s *SQLStore) ListAPIKeys(tenantID string) []domain.APIKey {
	rows, err := s.payloads(context.Background(), "ca_api_keys", "tenant_id=?", "created_ms DESC", []any{tenantID}, 0)
	if err != nil {
		return []domain.APIKey{}
	}
	out := make([]domain.APIKey, 0, len(rows))
	for _, raw := range rows {
		var k domain.APIKey
		if err := json.Unmarshal(raw, &k); err == nil {
			out = append(out, k)
		}
	}
	return out
}

func (s *SQLStore) TouchAPIKey(id string) {
	k, ok := s.GetAPIKey(id)
	if !ok {
		return
	}
	k.LastUsedAt = time.Now()
	_ = s.CreateAPIKey(k)
}

func (s *SQLStore) RevokeAPIKey(tenantID, id string) error {
	k, ok := s.GetAPIKey(id)
	if !ok || k.TenantID != tenantID {
		return ErrNotFound
	}
	k.Revoked = true
	return s.CreateAPIKey(k)
}

// ---------------------------------------------------------------------------
// 凭证
// ---------------------------------------------------------------------------

func (s *SQLStore) CreateCredential(c *domain.Credential) error {
	if c == nil {
		return errors.New("store: 凭证不能为空")
	}
	return s.put(context.Background(), "ca_credentials", "id", newID(c.ID),
		[]kv{{"tenant_id", c.TenantID}, {"created_ms", ms(c.CreatedAt)}}, c)
}

func (s *SQLStore) GetCredential(tenantID, id string) (*domain.Credential, bool) {
	var v domain.Credential
	ok, err := s.get(context.Background(), "ca_credentials", "id", id, &v)
	if err != nil || !ok || v.TenantID != tenantID {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) ListCredentials(tenantID string) []domain.Credential {
	rows, err := s.payloads(context.Background(), "ca_credentials", "tenant_id=?", "created_ms ASC", []any{tenantID}, 0)
	if err != nil {
		return []domain.Credential{}
	}
	out := make([]domain.Credential, 0, len(rows))
	for _, raw := range rows {
		var c domain.Credential
		if err := json.Unmarshal(raw, &c); err == nil {
			c.SecretEnc = "" // 密文绝不出存储层
			out = append(out, c)
		}
	}
	return out
}

func (s *SQLStore) UpdateCredential(c *domain.Credential) error {
	if c == nil {
		return errors.New("store: 凭证不能为空")
	}
	ok, err := s.exists(context.Background(), "ca_credentials", "id", c.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return s.CreateCredential(c)
}

func (s *SQLStore) DeleteCredential(tenantID, id string) error {
	c, ok := s.GetCredential(tenantID, id)
	if !ok {
		return ErrNotFound
	}
	_ = c
	return s.del(context.Background(), "ca_credentials", "id", id)
}

// ---------------------------------------------------------------------------
// 仓库
// ---------------------------------------------------------------------------

func (s *SQLStore) CreateRepo(r *domain.Repository) error {
	if r == nil {
		return errors.New("store: 仓库不能为空")
	}
	// 业务键唯一性（同租户）：与内存实现一致的冲突语义。
	rows, err := s.payloads(context.Background(), "ca_repositories", "tenant_id=?", "", []any{r.TenantID}, 0)
	if err != nil {
		return err
	}
	for _, raw := range rows {
		var e domain.Repository
		if err := json.Unmarshal(raw, &e); err == nil {
			if strings.EqualFold(e.Key, r.Key) {
				return ErrConflict
			}
		}
	}
	return s.put(context.Background(), "ca_repositories", "id", newID(r.ID),
		[]kv{{"tenant_id", r.TenantID}, {"repo_key", r.Key}, {"created_ms", ms(r.CreatedAt)}}, r)
}

func (s *SQLStore) GetRepo(tenantID, id string) (*domain.Repository, bool) {
	var v domain.Repository
	ok, err := s.get(context.Background(), "ca_repositories", "id", id, &v)
	if err != nil || !ok || v.TenantID != tenantID {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) GetRepoByKey(tenantID, key string) (*domain.Repository, bool) {
	rows, err := s.payloads(context.Background(), "ca_repositories", "tenant_id=?", "", []any{tenantID}, 0)
	if err != nil {
		return nil, false
	}
	for _, raw := range rows {
		var r domain.Repository
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		if strings.EqualFold(r.Key, key) || strings.EqualFold(r.Name, key) {
			return &r, true
		}
	}
	return nil, false
}

func (s *SQLStore) UpdateRepo(r *domain.Repository) error {
	if r == nil {
		return errors.New("store: 仓库不能为空")
	}
	ok, err := s.exists(context.Background(), "ca_repositories", "id", r.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return s.put(context.Background(), "ca_repositories", "id", r.ID,
		[]kv{{"tenant_id", r.TenantID}, {"repo_key", r.Key}, {"created_ms", ms(r.CreatedAt)}}, r)
}

func (s *SQLStore) DeleteRepo(tenantID, id string) error {
	r, ok := s.GetRepo(tenantID, id)
	if !ok {
		return ErrNotFound
	}
	_ = r
	if err := s.del(context.Background(), "ca_repositories", "id", id); err != nil {
		return err
	}
	return s.del(context.Background(), "ca_group_members", "repository_id", id)
}

func (s *SQLStore) ListRepos(tenantID string) []domain.Repository {
	rows, err := s.payloads(context.Background(), "ca_repositories", "tenant_id=?", "created_ms ASC", []any{tenantID}, 0)
	if err != nil {
		return []domain.Repository{}
	}
	out := make([]domain.Repository, 0, len(rows))
	for _, raw := range rows {
		var r domain.Repository
		if err := json.Unmarshal(raw, &r); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 分组
// ---------------------------------------------------------------------------

func (s *SQLStore) CreateGroup(g *domain.RepositoryGroup, members []domain.GroupMember) error {
	if g == nil {
		return errors.New("store: 分组不能为空")
	}
	ctx := context.Background()
	rows, err := s.payloads(ctx, "ca_groups", "tenant_id=?", "", []any{g.TenantID}, 0)
	if err != nil {
		return err
	}
	for _, raw := range rows {
		var e domain.RepositoryGroup
		if err := json.Unmarshal(raw, &e); err == nil {
			if strings.EqualFold(e.Key, g.Key) {
				return ErrConflict
			}
		}
	}
	if err := s.put(ctx, "ca_groups", "id", newID(g.ID),
		[]kv{{"tenant_id", g.TenantID}, {"group_key", g.Key}, {"created_ms", ms(g.CreatedAt)}}, g); err != nil {
		return err
	}
	for _, m := range members {
		if _, ok := s.GetRepo(g.TenantID, m.RepositoryID); !ok {
			return ErrNotFound
		}
		mm := m
		mm.GroupID = g.ID
		if mm.CreatedAt.IsZero() {
			mm.CreatedAt = time.Now()
		}
		if err := s.putMember(ctx, mm); err != nil {
			return err
		}
	}
	return nil
}

// putMember 写入分组成员（复合主键 group_id + repository_id，不能走通用 put）。
func (s *SQLStore) putMember(ctx context.Context, m domain.GroupMember) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("序列化分组成员失败：%w", err)
	}
	res, err := s.exec(ctx, "UPDATE ca_group_members SET order_idx=?, created_ms=?, payload=? WHERE group_id=? AND repository_id=?",
		m.Order, ms(m.CreatedAt), raw, m.GroupID, m.RepositoryID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = s.exec(ctx, "INSERT INTO ca_group_members (group_id, repository_id, order_idx, created_ms, payload) VALUES (?,?,?,?,?)",
		m.GroupID, m.RepositoryID, m.Order, ms(m.CreatedAt), raw)
	return err
}

func (s *SQLStore) GetGroup(tenantID, id string) (*domain.RepositoryGroup, bool) {
	var v domain.RepositoryGroup
	ok, err := s.get(context.Background(), "ca_groups", "id", id, &v)
	if err != nil || !ok || v.TenantID != tenantID {
		return nil, false
	}
	return &v, true
}

func (s *SQLStore) UpdateGroup(g *domain.RepositoryGroup, members []domain.GroupMember) error {
	if g == nil {
		return errors.New("store: 分组不能为空")
	}
	ctx := context.Background()
	ok, err := s.exists(ctx, "ca_groups", "id", g.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if err := s.put(ctx, "ca_groups", "id", g.ID,
		[]kv{{"tenant_id", g.TenantID}, {"group_key", g.Key}, {"created_ms", ms(g.CreatedAt)}}, g); err != nil {
		return err
	}
	if members == nil {
		return nil
	}
	if _, err := s.exec(ctx, "DELETE FROM ca_group_members WHERE group_id=?", g.ID); err != nil {
		return err
	}
	for _, m := range members {
		if _, ok := s.GetRepo(g.TenantID, m.RepositoryID); !ok {
			return ErrNotFound
		}
		mm := m
		mm.GroupID = g.ID
		if mm.CreatedAt.IsZero() {
			mm.CreatedAt = time.Now()
		}
		if err := s.putMember(ctx, mm); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLStore) DeleteGroup(tenantID, id string) error {
	g, ok := s.GetGroup(tenantID, id)
	if !ok {
		return ErrNotFound
	}
	_ = g
	if err := s.del(context.Background(), "ca_group_members", "group_id", id); err != nil {
		return err
	}
	return s.del(context.Background(), "ca_groups", "id", id)
}

func (s *SQLStore) ListGroups(tenantID string) []domain.RepositoryGroup {
	rows, err := s.payloads(context.Background(), "ca_groups", "tenant_id=?", "created_ms ASC", []any{tenantID}, 0)
	if err != nil {
		return []domain.RepositoryGroup{}
	}
	out := make([]domain.RepositoryGroup, 0, len(rows))
	for _, raw := range rows {
		var g domain.RepositoryGroup
		if err := json.Unmarshal(raw, &g); err == nil {
			out = append(out, g)
		}
	}
	return out
}

// groupMembers 读取分组内成员（按 Order 升序）。
func (s *SQLStore) groupMembers(ctx context.Context, groupID string) []domain.GroupMember {
	rows, err := s.payloads(ctx, "ca_group_members", "group_id=?", "order_idx ASC", []any{groupID}, 0)
	if err != nil {
		return nil
	}
	out := make([]domain.GroupMember, 0, len(rows))
	for _, raw := range rows {
		var m domain.GroupMember
		if err := json.Unmarshal(raw, &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func (s *SQLStore) GroupMemberViews(tenantID, groupID string) []domain.GroupMemberView {
	out := []domain.GroupMemberView{}
	for _, m := range s.groupMembers(context.Background(), groupID) {
		v := domain.GroupMemberView{GroupMember: m}
		if r, ok := s.GetRepo(tenantID, m.RepositoryID); ok {
			cp := *r
			v.Repo = &cp
		}
		out = append(out, v)
	}
	return out
}

func (s *SQLStore) GroupMemberRepoIDs(groupID string) []string {
	out := []string{}
	for _, m := range s.groupMembers(context.Background(), groupID) {
		out = append(out, m.RepositoryID)
	}
	return out
}

func (s *SQLStore) GroupsOfRepo(tenantID, repoID string) []domain.RepositoryGroup {
	rows, err := s.payloads(context.Background(), "ca_group_members", "repository_id=?", "", []any{repoID}, 0)
	if err != nil {
		return []domain.RepositoryGroup{}
	}
	out := []domain.RepositoryGroup{}
	for _, raw := range rows {
		var m domain.GroupMember
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if g, ok := s.GetGroup(tenantID, m.GroupID); ok {
			out = append(out, *g)
		}
	}
	return out
}

// boolInt 布尔 → 0/1（跨方言：SQLite/MySQL/Postgres 均以整数承载布尔）。
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
