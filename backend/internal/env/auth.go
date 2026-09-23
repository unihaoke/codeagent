package env

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/store"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// DefaultAdminPassword 未配置 AdminPasswordHash 时使用的默认控制台密码。
const DefaultAdminPassword = "admin123"

// DefaultJWTTTL 未配置 JWTTTLHours 时的默认令牌有效期。
const DefaultJWTTTL = 12 * time.Hour

// API Key 明文前缀与随机串长度。
const (
	apiKeyPrefix    = "ca_live_"
	apiKeyRandomLen = 32
	apiKeyPrefixLen = 12
)

// 作用域常量。
const (
	// ScopeAdminAll 管理域全量权限（通配一切 scope）。
	ScopeAdminAll = "admin:all"
	// ScopeTaskWrite 提交/取消分析任务。
	ScopeTaskWrite = "task:write"
	// ScopeRepoRead 读取仓库索引与源码。
	ScopeRepoRead = "repo:read"
	// ScopeRepoWrite 注册/更新仓库。
	ScopeRepoWrite = "repo:write"
	// ScopeGroupRead 读取分组。
	ScopeGroupRead = "group:read"
	// ScopeGroupWrite 创建/更新分组。
	ScopeGroupWrite = "group:write"
)

// 包级错误：API 层用 errors.Is 判断后映射为 401/403/404。
var (
	// ErrUnauthorized 未认证：凭据缺失、格式非法、已吊销或已过期。
	ErrUnauthorized = errors.New("未认证或凭据无效")
	// ErrForbidden 无权限：作用域不足或越权访问。
	ErrForbidden = errors.New("无权访问该资源")
	// ErrNotFound 资源不存在。
	ErrNotFound = errors.New("资源不存在")
	// ErrCrossTenant 跨租户访问：对外表现为 403（契约要求跨租户返回 403），
	// 同时保留"资源确实存在、只是不属于当前租户"的语义。
	ErrCrossTenant = fmt.Errorf("%w: 资源不属于当前租户", ErrForbidden)
)

// base62Alphabet 接入密钥明文使用的字符集。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Auth 认证授权中心：API Key 校验、控制台 JWT、越权拦截。
type Auth struct {
	cfg    *config.Config
	st     store.Store
	secret []byte
	ttl    time.Duration

	// box 凭证加密箱：用于回调密钥的 Seal/Open（与 Git 凭证共用同一把加密密钥）。
	box domain.CredentialBox

	adminUser string
	adminHash string
}

// 编译期校验：Auth 必须满足 domain.Authorizer 契约。
var _ domain.Authorizer = (*Auth)(nil)

// NewAuth 创建认证授权中心。
func NewAuth(cfg *config.Config, st store.Store) *Auth {
	if cfg == nil {
		cfg = config.Default()
	}
	a := &Auth{
		cfg:       cfg,
		st:        st,
		ttl:       time.Duration(cfg.Auth.JWTTTLHours) * time.Hour,
		adminUser: strings.TrimSpace(cfg.Auth.AdminUsername),
		adminHash: strings.TrimSpace(cfg.Auth.AdminPasswordHash),
	}
	if a.ttl <= 0 {
		a.ttl = DefaultJWTTTL
	}
	secret := cfg.Auth.JWTSecret
	if secret == "" {
		secret = "codeagent-dev-secret-change-me"
	}
	a.secret = []byte(secret)
	if a.adminUser == "" {
		a.adminUser = "admin"
	}
	// 凭证加密箱：用于回调密钥的 Seal/Open。开发环境缺密钥时由 NewCredentialBox 兜底默认密钥。
	if b, err := NewCredentialBox(cfg.Security); err == nil {
		a.box = b
	}
	return a
}

// Seal 实现 credentialSealer：加密回调密钥明文（AES-GCM）。
func (a *Auth) Seal(plain string) (string, error) {
	if a == nil || a.box == nil {
		return "", errors.New("凭证加密箱未初始化")
	}
	return a.box.Seal(plain)
}

// Open 解密回调密钥密文（供引擎终态回调节度使用）。
func (a *Auth) Open(sealed string) (string, error) {
	if a == nil || a.box == nil {
		return "", errors.New("凭证加密箱未初始化")
	}
	return a.box.Open(sealed)
}

// Config 返回认证中心持有的配置（只读用途）。
func (a *Auth) Config() *config.Config { return a.cfg }

// ---------------------------------------------------------------------------
// API Key
// ---------------------------------------------------------------------------

// GenerateAPIKey 生成接入密钥：明文仅返回一次，存储只保留 SHA-256 哈希与前缀。
//
// ttl == 0 表示永不过期；ttl < 0 表示立即过期（用于测试与紧急失效）。
func (a *Auth) GenerateAPIKey(tenantID, name string, scopes []string, ttl time.Duration) (string, *domain.APIKey, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return "", nil, fmt.Errorf("租户 ID 不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	if len(scopes) == 0 {
		scopes = []string{ScopeTaskWrite, ScopeRepoRead, ScopeGroupRead}
	}

	plain, err := newAPIKeyPlain()
	if err != nil {
		return "", nil, err
	}
	now := time.Now()
	key := &domain.APIKey{
		ID:        "key-" + uuid.NewString(),
		TenantID:  tenantID,
		Name:      name,
		KeyHash:   HashAPIKey(plain),
		KeyPrefix: prefixOf(plain),
		Scopes:    append([]string{}, scopes...),
		CreatedAt: now,
	}
	if ttl != 0 {
		key.ExpiresAt = now.Add(ttl)
	}
	if a.st != nil {
		if err := a.st.CreateAPIKey(key); err != nil {
			return "", nil, fmt.Errorf("保存接入密钥失败: %w", err)
		}
	}
	return plain, key, nil
}

// RevokeAPIKey 吊销接入密钥（租户隔离）。
func (a *Auth) RevokeAPIKey(tenantID, keyID string) error {
	if a.st == nil {
		return ErrNotFound
	}
	return a.st.RevokeAPIKey(tenantID, keyID)
}

// ListAPIKeys 列出租户接入密钥（不含哈希明文，KeyHash 由存储层原样带出，API 层需自行裁剪）。
func (a *Auth) ListAPIKeys(tenantID string) []domain.APIKey {
	if a.st == nil {
		return nil
	}
	return a.st.ListAPIKeys(tenantID)
}

// HashAPIKey 计算接入密钥明文的 SHA-256 十六进制哈希（落库与查询的唯一依据）。
func HashAPIKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// newAPIKeyPlain 生成 ca_live_ + 32 位 base62 随机明文。
func newAPIKeyPlain() (string, error) {
	buf := make([]byte, apiKeyRandomLen)
	out := make([]byte, apiKeyRandomLen)
	for i := 0; i < apiKeyRandomLen; {
		if _, err := rand.Read(buf[i : i+1]); err != nil {
			return "", fmt.Errorf("生成随机密钥失败: %w", err)
		}
		b := buf[i]
		// 拒绝采样，避免取模引入分布偏差（248 = 4 * 62）。
		if b >= 248 {
			continue
		}
		out[i] = base62Alphabet[int(b)%len(base62Alphabet)]
		i++
	}
	return apiKeyPrefix + string(out), nil
}

// prefixOf 取明文前 12 位作为可读前缀。
func prefixOf(plain string) string {
	if len(plain) <= apiKeyPrefixLen {
		return plain
	}
	return plain[:apiKeyPrefixLen]
}

// Authenticate 校验接入层 API Key 并还原调用主体。
func (a *Auth) Authenticate(ctx context.Context, apiKey string) (*domain.Subject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plain := strings.TrimSpace(apiKey)
	if plain == "" {
		return nil, fmt.Errorf("%w: 缺少接入密钥", ErrUnauthorized)
	}
	if a.st == nil {
		return nil, fmt.Errorf("%w: 存储未就绪", ErrUnauthorized)
	}
	key, ok := a.st.FindAPIKeyByHash(HashAPIKey(plain))
	if !ok {
		return nil, fmt.Errorf("%w: 接入密钥不存在或已吊销", ErrUnauthorized)
	}
	if key.Revoked {
		return nil, fmt.Errorf("%w: 接入密钥已吊销", ErrUnauthorized)
	}
	if !key.ExpiresAt.IsZero() && time.Now().After(key.ExpiresAt) {
		return nil, fmt.Errorf("%w: 接入密钥已过期", ErrUnauthorized)
	}
	tenant, ok := a.st.GetTenant(key.TenantID)
	if !ok {
		return nil, fmt.Errorf("%w: 接入密钥所属租户不存在", ErrUnauthorized)
	}
	if tenant.Status != domain.TenantActive {
		return nil, fmt.Errorf("%w: 租户已停用", ErrForbidden)
	}
	a.st.TouchAPIKey(key.ID)

	return &domain.Subject{
		TenantID:   tenant.ID,
		TenantName: tenant.Name,
		APIKeyID:   key.ID,
		KeyPrefix:  key.KeyPrefix,
		Scopes:     append([]string{}, key.Scopes...),
		Admin:      hasScope(key.Scopes, ScopeAdminAll),
	}, nil
}

// ---------------------------------------------------------------------------
// 控制台 JWT（HS256，自实现）
// ---------------------------------------------------------------------------

// jwtHeader HS256 固定头部。
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// jwtClaims 控制台令牌载荷。
type jwtClaims struct {
	Sub    string   `json:"sub"`   // 主体（租户 ID）
	Tid    string   `json:"tid"`   // 租户 ID
	Name   string   `json:"name"`  // 登录用户名
	TName  string   `json:"tname"` // 租户名（便于前端直接展示）
	Scopes []string `json:"scopes"`
	Exp    int64    `json:"exp"`
	Iat    int64    `json:"iat"`
	Jti    string   `json:"jti"`
}

// Login 控制台登录：校验租户、用户名与密码，签发 HS256 JWT。
//
// 用户名为配置的 AdminUsername 时按管理员校验；
// 为兼容控制台的多租户入口，也允许用租户名/租户 ID 作为用户名宽松匹配。
// AdminPasswordHash 为空时回退到默认密码 DefaultAdminPassword（首次启动由 main 注入 hash）。
func (a *Auth) Login(ctx context.Context, tenantKey, username, password string) (string, *domain.Subject, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if a.st == nil {
		return "", nil, fmt.Errorf("%w: 存储未就绪", ErrUnauthorized)
	}
	key := strings.TrimSpace(tenantKey)
	if key == "" {
		return "", nil, fmt.Errorf("%w: 缺少租户标识", ErrUnauthorized)
	}
	tenant, ok := a.st.GetTenantByKey(key)
	if !ok {
		return "", nil, fmt.Errorf("%w: 租户或账号信息错误", ErrUnauthorized)
	}
	user := strings.TrimSpace(username)
	if !a.matchUsername(tenant, user) {
		return "", nil, fmt.Errorf("%w: 租户或账号信息错误", ErrUnauthorized)
	}
	if err := a.checkPassword(password); err != nil {
		return "", nil, err
	}
	if tenant.Status != domain.TenantActive {
		return "", nil, fmt.Errorf("%w: 租户已停用", ErrForbidden)
	}
	if user == "" {
		user = a.adminUser
	}
	sub := &domain.Subject{
		TenantID:   tenant.ID,
		TenantName: tenant.Name,
		Username:   user,
		Admin:      true,
		Scopes:     []string{ScopeAdminAll},
	}
	token, err := a.IssueToken(sub, a.ttl)
	if err != nil {
		return "", nil, err
	}
	return token, sub, nil
}

// matchUsername 判断用户名是否合法：管理员账号，或与租户名/租户 ID 宽松匹配。
func (a *Auth) matchUsername(tenant *domain.Tenant, username string) bool {
	if username == "" {
		return true // 空用户名按管理员处理，由密码校验兜底
	}
	if strings.EqualFold(username, a.adminUser) {
		return true
	}
	if tenant == nil {
		return false
	}
	return strings.EqualFold(username, tenant.Name) || strings.EqualFold(username, tenant.ID)
}

// checkPassword 校验控制台密码。
func (a *Auth) checkPassword(password string) error {
	bad := fmt.Errorf("%w: 租户或账号信息错误", ErrUnauthorized)
	if a.adminHash == "" {
		// 未注入 hash：使用默认开发密码。
		if subtle.ConstantTimeCompare([]byte(password), []byte(DefaultAdminPassword)) == 1 {
			return nil
		}
		return bad
	}
	if strings.HasPrefix(a.adminHash, "$2") {
		// 标准 bcrypt 哈希。
		if bcrypt.CompareHashAndPassword([]byte(a.adminHash), []byte(password)) == nil {
			return nil
		}
		return bad
	}
	// 兼容以明文写入配置的本地开发场景（生产必须配置 bcrypt 哈希）。
	if subtle.ConstantTimeCompare([]byte(password), []byte(a.adminHash)) == 1 {
		return nil
	}
	return bad
}

// IssueToken 按给定有效期签发 HS256 JWT。
//
// ttl == 0 时使用配置默认有效期；ttl < 0 表示已过期（用于测试与强制失效场景）。
func (a *Auth) IssueToken(sub *domain.Subject, ttl time.Duration) (string, error) {
	if sub == nil || sub.TenantID == "" {
		return "", fmt.Errorf("%w: 缺少租户主体", ErrUnauthorized)
	}
	if ttl == 0 {
		ttl = a.ttl
	}
	now := time.Now()
	scopes := append([]string{}, sub.Scopes...)
	if len(scopes) == 0 {
		scopes = []string{ScopeAdminAll}
	}
	claims := jwtClaims{
		Sub:    sub.TenantID,
		Tid:    sub.TenantID,
		Name:   sub.Username,
		TName:  sub.TenantName,
		Scopes: scopes,
		Exp:    now.Add(ttl).Unix(),
		Iat:    now.Unix(),
		Jti:    uuid.NewString(),
	}
	headerJSON, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("序列化 JWT 头失败: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("序列化 JWT 载荷失败: %w", err)
	}
	signing := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	return signing + "." + a.sign(signing), nil
}

// sign 计算 HS256 签名（base64url 无填充）。
func (a *Auth) sign(signing string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(signing))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyToken 校验 JWT 签名与有效期，并还原调用主体。
func (a *Auth) VerifyToken(ctx context.Context, token string) (*domain.Subject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	claims, err := a.parseToken(token)
	if err != nil {
		return nil, err
	}
	if a.st != nil {
		if tenant, ok := a.st.GetTenant(claims.Tid); ok {
			if tenant.Status != domain.TenantActive {
				return nil, fmt.Errorf("%w: 租户已停用", ErrForbidden)
			}
		} else {
			return nil, fmt.Errorf("%w: 令牌所属租户不存在", ErrUnauthorized)
		}
	}
	sub := &domain.Subject{
		TenantID:   claims.Tid,
		TenantName: claims.TName,
		Username:   claims.Name,
		Scopes:     claims.Scopes,
		Admin:      hasScope(claims.Scopes, ScopeAdminAll),
	}
	if sub.TenantID == "" {
		sub.TenantID = claims.Sub
	}
	return sub, nil
}

// parseToken 解析并校验 JWT，返回载荷。
func (a *Auth) parseToken(token string) (*jwtClaims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: 令牌格式非法", ErrUnauthorized)
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: 令牌头部解码失败", ErrUnauthorized)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, fmt.Errorf("%w: 令牌头部解析失败", ErrUnauthorized)
	}
	// 固定算法，拒绝 alg=none 等降级攻击。
	if !strings.EqualFold(header.Alg, "HS256") {
		return nil, fmt.Errorf("%w: 不支持的签名算法 %q", ErrUnauthorized, header.Alg)
	}
	expected := a.sign(parts[0] + "." + parts[1])
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return nil, fmt.Errorf("%w: 令牌签名校验失败", ErrUnauthorized)
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: 令牌载荷解码失败", ErrUnauthorized)
	}
	var claims jwtClaims
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		return nil, fmt.Errorf("%w: 令牌载荷解析失败", ErrUnauthorized)
	}
	if claims.Exp > 0 && time.Now().Unix() >= claims.Exp {
		return nil, fmt.Errorf("%w: 令牌已过期", ErrUnauthorized)
	}
	if claims.Tid == "" && claims.Sub == "" {
		return nil, fmt.Errorf("%w: 令牌缺少租户标识", ErrUnauthorized)
	}
	return &claims, nil
}

// ---------------------------------------------------------------------------
// 授权：越权拦截
// ---------------------------------------------------------------------------

// RequireScope 校验主体是否具备指定作用域（admin:all 通配）。
func (a *Auth) RequireScope(sub *domain.Subject, scope string) error {
	if sub == nil || sub.TenantID == "" {
		return fmt.Errorf("%w: 缺少认证主体", ErrUnauthorized)
	}
	if !sub.Has(scope) {
		return fmt.Errorf("%w: 缺少作用域 %s", ErrForbidden, scope)
	}
	return nil
}

// CanSubmitTask 校验主体是否可提交分析任务。
func (a *Auth) CanSubmitTask(sub *domain.Subject) error {
	return a.RequireScope(sub, ScopeTaskWrite)
}

// CanAccessRepo 校验主体是否有权访问指定仓库。
//
// 判定顺序：认证 → 作用域（repo:read 或 admin:all）→ 租户归属 → AccessPolicy 白名单。
// 跨租户访问返回 ErrCrossTenant（errors.Is(err, ErrForbidden) 为真）；
// 本租户内确实不存在的仓库返回 ErrNotFound。
func (a *Auth) CanAccessRepo(ctx context.Context, sub *domain.Subject, repoID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.RequireScope(sub, ScopeRepoRead); err != nil {
		return err
	}
	id := strings.TrimSpace(repoID)
	if id == "" {
		return fmt.Errorf("%w: 仓库 ID 不能为空", ErrNotFound)
	}
	if a.st == nil {
		return fmt.Errorf("%w: 存储未就绪", ErrNotFound)
	}
	repo, ok := a.st.GetRepo(sub.TenantID, id)
	if !ok {
		if a.repoExistsElsewhere(sub.TenantID, id) {
			return ErrCrossTenant
		}
		return fmt.Errorf("%w: 仓库 %s", ErrNotFound, id)
	}
	if err := checkAccessPolicy(repo.AccessPolicy, sub); err != nil {
		return err
	}
	return nil
}

// CanAccessGroup 校验主体是否有权访问指定分组（作用域 group:read）。
func (a *Auth) CanAccessGroup(ctx context.Context, sub *domain.Subject, groupID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.RequireScope(sub, ScopeGroupRead); err != nil {
		return err
	}
	id := strings.TrimSpace(groupID)
	if id == "" {
		return fmt.Errorf("%w: 分组 ID 不能为空", ErrNotFound)
	}
	if a.st == nil {
		return fmt.Errorf("%w: 存储未就绪", ErrNotFound)
	}
	if _, ok := a.st.GetGroup(sub.TenantID, id); ok {
		return nil
	}
	if a.groupExistsElsewhere(sub.TenantID, id) {
		return ErrCrossTenant
	}
	return fmt.Errorf("%w: 分组 %s", ErrNotFound, id)
}

// checkAccessPolicy 校验仓库访问白名单：为空表示租户内全量可见。
func checkAccessPolicy(policy []string, sub *domain.Subject) error {
	if len(policy) == 0 || sub.Has(ScopeAdminAll) {
		return nil
	}
	for _, allow := range policy {
		if strings.EqualFold(strings.TrimSpace(allow), sub.TenantID) {
			return nil
		}
	}
	return fmt.Errorf("%w: 仓库访问白名单未包含租户 %s", ErrForbidden, sub.TenantID)
}

// repoExistsElsewhere 判断仓库是否存在于其他租户（用于区分 403 与 404）。
func (a *Auth) repoExistsElsewhere(tenantID, repoID string) bool {
	for _, t := range a.st.ListTenants() {
		if t.ID == tenantID {
			continue
		}
		if _, ok := a.st.GetRepo(t.ID, repoID); ok {
			return true
		}
	}
	return false
}

// groupExistsElsewhere 判断分组是否存在于其他租户。
func (a *Auth) groupExistsElsewhere(tenantID, groupID string) bool {
	for _, t := range a.st.ListTenants() {
		if t.ID == tenantID {
			continue
		}
		if _, ok := a.st.GetGroup(t.ID, groupID); ok {
			return true
		}
	}
	return false
}

// hasScope 判断作用域集合是否包含目标（admin:all 通配）。
func hasScope(scopes []string, scope string) bool {
	for _, sc := range scopes {
		if sc == scope || sc == ScopeAdminAll {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 上下文辅助（env 包不依赖 httpx，故自行实现）
// ---------------------------------------------------------------------------

type ctxKey int

const ctxKeySubject ctxKey = iota

// ContextWithSubject 将认证主体注入 context。
func ContextWithSubject(ctx context.Context, sub *domain.Subject) context.Context {
	return context.WithValue(ctx, ctxKeySubject, sub)
}

// SubjectFromContext 从 context 读取认证主体。
func SubjectFromContext(ctx context.Context) (*domain.Subject, bool) {
	if ctx == nil {
		return nil, false
	}
	sub, ok := ctx.Value(ctxKeySubject).(*domain.Subject)
	return sub, ok && sub != nil
}
