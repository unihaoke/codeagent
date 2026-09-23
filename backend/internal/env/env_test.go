package env

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// newTestBox 创建带自定义脱敏规则的加密箱。
func newTestBox(t *testing.T) *AESCredentialBox {
	t.Helper()
	box, err := NewCredentialBox(config.SecurityConfig{
		EncryptionKey: "unit-test-encryption-key",
		MaskEnabled:   true,
		MaskRules:     []string{`(?i)internal-token-[0-9a-f]{8}`},
	})
	if err != nil {
		t.Fatalf("创建加密箱失败: %v", err)
	}
	return box
}

// newTestAuth 创建认证中心与空存储。
func newTestAuth(t *testing.T) (*Auth, store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.JWTSecret = "unit-test-jwt-secret"
	cfg.Auth.JWTTTLHours = 12
	cfg.Auth.AdminUsername = "admin"
	cfg.Auth.AdminPasswordHash = "" // 回退默认密码 admin123
	st := store.New()
	return NewAuth(cfg, st), st
}

// seedTenant 写入一个活跃租户。
func seedTenant(t *testing.T, st store.Store, id, name string) {
	t.Helper()
	now := time.Now()
	err := st.CreateTenant(&domain.Tenant{
		ID: id, Name: name, Status: domain.TenantActive,
		Quota: domain.DefaultQuota(), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("创建租户 %s 失败: %v", id, err)
	}
}

// seedRepo 写入一个仓库。
func seedRepo(t *testing.T, st store.Store, id, tenantID, key string) {
	t.Helper()
	now := time.Now()
	err := st.CreateRepo(&domain.Repository{
		ID: id, TenantID: tenantID, Name: key, Key: key,
		URL: "https://git.acme.internal/mall/" + key + ".git", DefaultBranch: "main",
		Provider: domain.VCSGitLab, Layer: domain.LayerService, Language: "Java",
		Status: domain.RepoActive, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("创建仓库 %s 失败: %v", id, err)
	}
}

// ---------------------------------------------------------------------------
// 凭证加密箱
// ---------------------------------------------------------------------------

func TestCredentialBoxSealOpen(t *testing.T) {
	box := newTestBox(t)
	plain := "glpat-super-secret-value-0001"

	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}
	if sealed == plain {
		t.Fatal("密文不应等于明文")
	}
	if strings.Contains(sealed, plain) || strings.Contains(sealed, "glpat") {
		t.Fatalf("密文不应包含明文片段: %s", sealed)
	}
	if _, err := base64.RawURLEncoding.DecodeString(sealed); err != nil {
		t.Fatalf("密文应为 RawURL base64: %v", err)
	}

	got, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if got != plain {
		t.Fatalf("往返不一致: %q != %q", got, plain)
	}

	// 随机 nonce：同明文两次加密结果必须不同。
	sealed2, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("第二次 Seal 失败: %v", err)
	}
	if sealed2 == sealed {
		t.Fatal("相同明文两次加密应因随机 nonce 而不同")
	}

	// 换密钥不可解密。
	other, err := NewCredentialBox(config.SecurityConfig{EncryptionKey: "another-key"})
	if err != nil {
		t.Fatalf("创建另一个加密箱失败: %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("不同密钥不应解密成功")
	}
	// 非法密文。
	if _, err := box.Open("!!!not-base64!!!"); err == nil {
		t.Fatal("非法 base64 应返回错误")
	}
	if _, err := box.Open(""); err == nil {
		t.Fatal("空密文应返回错误")
	}

	// 未配置密钥时使用开发默认密钥，且可自洽往返。
	def, err := NewCredentialBox(config.SecurityConfig{})
	if err != nil {
		t.Fatalf("默认密钥创建失败: %v", err)
	}
	s, err := def.Seal("x")
	if err != nil {
		t.Fatalf("默认密钥加密失败: %v", err)
	}
	if v, err := def.Open(s); err != nil || v != "x" {
		t.Fatalf("默认密钥往返失败: %q %v", v, err)
	}

	// 非法自定义规则应直接报错。
	if _, err := NewCredentialBox(config.SecurityConfig{MaskRules: []string{"("}}); err == nil {
		t.Fatal("非法脱敏正则应返回错误")
	}
}

func TestMaskCoverageAndIdempotent(t *testing.T) {
	box := newTestBox(t)
	const privKey = "-----BEGIN RSA PRIVATE KEY-----\n" +
		"MIIEowIBAAKCAQEA1234567890abcdefghij\n" +
		"QklDS0VZQk9EWTIyMzQ1Njc4OTA=\n" +
		"-----END RSA PRIVATE KEY-----"

	cases := []struct {
		name           string
		in             string
		mustContain    []string
		mustNotContain []string
	}{
		{
			name: "password 键值", in: "db.password=abc123",
			mustContain: []string{"password=***"}, mustNotContain: []string{"abc123"},
		},
		{
			name: "passwd 与 pwd 键值", in: "passwd: hunter2\npwd=letmein",
			mustContain: []string{"passwd: ***", "pwd=***"}, mustNotContain: []string{"hunter2", "letmein"},
		},
		{
			name: "JSON 风格引号键值", in: `{"secretKey": "abc-def-ghi", "apiKey": "sk-abcdefghijklmnopqrstuvwxyz0123"}`,
			mustContain:    []string{`"secretKey": "***"`, `"apiKey": "***"`},
			mustNotContain: []string{"abc-def-ghi", "sk-abcdefghijklmnopqrstuvwxyz0123"},
		},
		{
			name: "Authorization Bearer", in: "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			mustContain: []string{"Authorization: Bearer ***"}, mustNotContain: []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"},
		},
		{
			name: "私钥块", in: privKey,
			mustContain: []string{"***"}, mustNotContain: []string{"MIIEowIBAAKCAQEA", "QklDS0VZQk9EWTIyMzQ1Njc4OTA=", "PRIVATE KEY"},
		},
		{
			name: "AWS AccessKeyId", in: "aws_access_key_id=AKIAIOSFODNN7EXAMPLE",
			mustContain: []string{"aws_access_key_id=***"}, mustNotContain: []string{"AKIAIOSFODNN7EXAMPLE"},
		},
		{
			name: "AWS 临时凭证 ASIA", in: "AccessKeyId: ASIAIOSFODNN7EXAMPLE",
			mustContain: []string{"AccessKeyId: ***"}, mustNotContain: []string{"ASIAIOSFODNN7EXAMPLE"},
		},
		{
			name: "PostgreSQL 连接串", in: "postgres://acme:pa55word@db.acme.internal:5432/mall",
			mustContain: []string{"postgres://acme:***@db.acme.internal:5432/mall"}, mustNotContain: []string{"pa55word"},
		},
		{
			name: "Redis 连接串", in: "redis://:s3cretpw@127.0.0.1:6379/0",
			mustContain: []string{"redis://:***@127.0.0.1:6379/0"}, mustNotContain: []string{"s3cretpw"},
		},
		{
			name: "JDBC/MySQL 连接串", in: "jdbc:mysql://root:r00tPass@mysql.acme.internal:3306/order?useSSL=false",
			mustContain: []string{"mysql://root:***@mysql.acme.internal:3306/order"}, mustNotContain: []string{"r00tPass"},
		},
		{
			name: "平台 Token 前缀",
			in: "token=ghp_abcdefghijklmnopqrstuvwxyz012345\n" +
				"glpat-abcdefghijklmnopqrst\n" +
				"xoxb_abcdefghijklmnopqrst",
			mustContain:    []string{"token=***"},
			mustNotContain: []string{"ghp_abcdefghijklmnopqrstuvwxyz012345", "glpat-abcdefghijklmnopqrst", "xoxb_abcdefghijklmnopqrst"},
		},
		{
			name: "自定义 MaskRules", in: "trace internal-token-deadbeef end",
			mustContain: []string{"trace *** end"}, mustNotContain: []string{"deadbeef"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			masked := box.Mask(tc.in)
			for _, want := range tc.mustContain {
				if !strings.Contains(masked, want) {
					t.Fatalf("脱敏结果缺少片段 %q，实际为:\n%s", want, masked)
				}
			}
			for _, bad := range tc.mustNotContain {
				if strings.Contains(masked, bad) {
					t.Fatalf("脱敏结果仍包含敏感片段 %q，实际为:\n%s", bad, masked)
				}
			}
			// 行数与大致结构不变（diff 定位依赖）。
			if got, want := strings.Count(masked, "\n"), strings.Count(tc.in, "\n"); got != want {
				t.Fatalf("脱敏改变了行数: %d != %d", got, want)
			}
			// 幂等。
			if again := box.Mask(masked); again != masked {
				t.Fatalf("脱敏不幂等:\n第一次: %q\n第二次: %q", masked, again)
			}
		})
	}
}

func TestMaskCodeIdempotent(t *testing.T) {
	box := newTestBox(t)
	code := "func main() {\n" +
		"\tpwd := \"hunter2\"\n" +
		"\tclient.SetToken(\"ghp_abcdefghijklmnopqrstuvwxyz0123456789\")\n" +
		"\tdsn := \"postgres://acme:pa55word@db.acme.internal:5432/mall\"\n" +
		"\tfmt.Println(pwd, dsn)\n" +
		"}\n"

	masked, changed := box.MaskCode(code)
	if !changed {
		t.Fatal("MaskCode 应报告发生了脱敏")
	}
	for _, bad := range []string{"hunter2", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "pa55word"} {
		if strings.Contains(masked, bad) {
			t.Fatalf("代码脱敏后仍包含 %q:\n%s", bad, masked)
		}
	}
	if strings.Count(masked, "\n") != strings.Count(code, "\n") {
		t.Fatal("代码脱敏不应改变行数")
	}

	masked2, changed2 := box.MaskCode(masked)
	if changed2 {
		t.Fatalf("对已脱敏代码再次调用应报告未变化:\n%s", masked2)
	}
	if masked2 != masked {
		t.Fatal("代码脱敏应幂等")
	}
}

// ---------------------------------------------------------------------------
// API Key
// ---------------------------------------------------------------------------

func TestAPIKeyLifecycle(t *testing.T) {
	auth, st := newTestAuth(t)
	seedTenant(t, st, "t-1", "tenant-one")
	ctx := context.Background()

	plain, key, err := auth.GenerateAPIKey("t-1", "integration", []string{ScopeTaskWrite, ScopeRepoRead}, time.Hour)
	if err != nil {
		t.Fatalf("生成接入密钥失败: %v", err)
	}
	if !strings.HasPrefix(plain, "ca_live_") {
		t.Fatalf("明文前缀错误: %s", plain)
	}
	if len(plain) != len("ca_live_")+32 {
		t.Fatalf("明文长度错误: %d", len(plain))
	}
	if key.KeyPrefix != plain[:12] || !strings.HasPrefix(plain, key.KeyPrefix) {
		t.Fatalf("KeyPrefix 应为明文前 12 位: %s vs %s", key.KeyPrefix, plain)
	}
	if key.KeyHash != HashAPIKey(plain) {
		t.Fatal("KeyHash 应为明文的 SHA-256 十六进制")
	}
	if strings.Contains(key.KeyHash, plain) || strings.Contains(plain, key.KeyHash) {
		t.Fatal("存储不得包含明文")
	}
	if key.ExpiresAt.IsZero() {
		t.Fatal("指定 ttl 后应有过期时间")
	}

	sub, err := auth.Authenticate(ctx, plain)
	if err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	if sub.TenantID != "t-1" || sub.APIKeyID != key.ID || sub.Admin {
		t.Fatalf("主体还原错误: %+v", sub)
	}
	if !sub.Has(ScopeTaskWrite) || !sub.Has(ScopeRepoRead) {
		t.Fatalf("作用域还原错误: %+v", sub.Scopes)
	}
	keys := st.ListAPIKeys("t-1")
	if len(keys) != 1 || keys[0].LastUsedAt.IsZero() {
		t.Fatal("认证成功应刷新 LastUsedAt")
	}

	// 未知密钥。
	if _, err := auth.Authenticate(ctx, "ca_live_unknown_key_value"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("未知密钥应返回 ErrUnauthorized，实际: %v", err)
	}
	if _, err := auth.Authenticate(ctx, "  "); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("空密钥应返回 ErrUnauthorized，实际: %v", err)
	}

	// 吊销后失效。
	if err := auth.RevokeAPIKey("t-1", key.ID); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	if _, err := auth.Authenticate(ctx, plain); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("吊销后应返回 ErrUnauthorized，实际: %v", err)
	}

	// 过期后失效。
	expiredPlain, _, err := auth.GenerateAPIKey("t-1", "expired", []string{ScopeTaskWrite}, -time.Minute)
	if err != nil {
		t.Fatalf("生成过期密钥失败: %v", err)
	}
	if _, err := auth.Authenticate(ctx, expiredPlain); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("过期后应返回 ErrUnauthorized，实际: %v", err)
	}

	// 永不过期（ttl = 0）。
	forever, keyForever, err := auth.GenerateAPIKey("t-1", "forever", []string{ScopeAdminAll}, 0)
	if err != nil {
		t.Fatalf("生成长期密钥失败: %v", err)
	}
	if !keyForever.ExpiresAt.IsZero() {
		t.Fatal("ttl=0 时不应设置过期时间")
	}
	foreverSub, err := auth.Authenticate(ctx, forever)
	if err != nil {
		t.Fatalf("长期密钥认证失败: %v", err)
	}
	if !foreverSub.Has(ScopeRepoRead) || !foreverSub.Has(ScopeGroupRead) {
		t.Fatal("admin:all 应通配一切作用域")
	}

	// 租户停用后拒绝。
	tenant, _ := st.GetTenant("t-1")
	tenant.Status = domain.TenantSuspended
	if err := st.UpdateTenant(tenant); err != nil {
		t.Fatalf("更新租户失败: %v", err)
	}
	if _, err := auth.Authenticate(ctx, forever); !errors.Is(err, ErrForbidden) {
		t.Fatalf("停用租户应返回 ErrForbidden，实际: %v", err)
	}
}

func TestCanAccessRepoAndGroupIsolation(t *testing.T) {
	auth, st := newTestAuth(t)
	seedTenant(t, st, "t-a", "tenant-a")
	seedTenant(t, st, "t-b", "tenant-b")
	seedRepo(t, st, "repo-1", "t-a", "order-service")
	now := time.Now()
	if err := st.CreateGroup(&domain.RepositoryGroup{
		ID: "grp-1", TenantID: "t-a", Name: "核心链路", Key: "core",
		Status: domain.GroupActive, CreatedAt: now, UpdatedAt: now,
	}, []domain.GroupMember{{GroupID: "grp-1", RepositoryID: "repo-1", Order: 1, CreatedAt: now}}); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}
	ctx := context.Background()

	subA := &domain.Subject{TenantID: "t-a", Scopes: []string{ScopeRepoRead, ScopeGroupRead}}
	if err := auth.CanAccessRepo(ctx, subA, "repo-1"); err != nil {
		t.Fatalf("本租户仓库应放行: %v", err)
	}
	if err := auth.CanAccessGroup(ctx, subA, "grp-1"); err != nil {
		t.Fatalf("本租户分组应放行: %v", err)
	}

	// 跨租户仓库访问：必须被拒（403 语义，兼容 404 判定）。
	subB := &domain.Subject{TenantID: "t-b", Scopes: []string{ScopeRepoRead, ScopeGroupRead}}
	err := auth.CanAccessRepo(ctx, subB, "repo-1")
	if !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户访问应被拒，实际: %v", err)
	}
	if err := auth.CanAccessGroup(ctx, subB, "grp-1"); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户分组访问应被拒，实际: %v", err)
	}

	// 本租户内不存在的资源 → 404。
	if err := auth.CanAccessRepo(ctx, subA, "repo-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的仓库应返回 ErrNotFound，实际: %v", err)
	}
	if err := auth.CanAccessGroup(ctx, subA, "grp-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的分组应返回 ErrNotFound，实际: %v", err)
	}

	// 作用域不足 → 403。
	weak := &domain.Subject{TenantID: "t-a", Scopes: []string{ScopeTaskWrite}}
	if err := auth.CanAccessRepo(ctx, weak, "repo-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("缺少 repo:read 应返回 ErrForbidden，实际: %v", err)
	}
	if err := auth.CanAccessGroup(ctx, weak, "grp-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("缺少 group:read 应返回 ErrForbidden，实际: %v", err)
	}

	// 未认证主体 → 401。
	if err := auth.CanAccessRepo(ctx, nil, "repo-1"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("空主体应返回 ErrUnauthorized，实际: %v", err)
	}

	// AccessPolicy 白名单：非空且不含本租户时拒绝，admin:all 可绕过。
	repo, _ := st.GetRepo("t-a", "repo-1")
	repo.AccessPolicy = []string{"t-b"}
	if err := st.UpdateRepo(repo); err != nil {
		t.Fatalf("更新仓库失败: %v", err)
	}
	if err := auth.CanAccessRepo(ctx, subA, "repo-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("白名单外租户应被拒绝，实际: %v", err)
	}
	admin := &domain.Subject{TenantID: "t-a", Scopes: []string{ScopeAdminAll}}
	if err := auth.CanAccessRepo(ctx, admin, "repo-1"); err != nil {
		t.Fatalf("admin:all 应绕过白名单: %v", err)
	}

	// 便捷方法。
	if err := auth.CanSubmitTask(weak); err != nil {
		t.Fatalf("含 task:write 的主体应可提交任务: %v", err)
	}
	if err := auth.CanSubmitTask(subA); !errors.Is(err, ErrForbidden) {
		t.Fatalf("缺少 task:write 应返回 ErrForbidden，实际: %v", err)
	}
	if err := auth.CanSubmitTask(admin); err != nil {
		t.Fatalf("admin:all 应可提交任务: %v", err)
	}
	if err := auth.CanSubmitTask(nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("空主体提交任务应返回 ErrUnauthorized，实际: %v", err)
	}
	if err := auth.RequireScope(subA, "quota:read"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("缺少作用域应返回 ErrForbidden，实际: %v", err)
	}
}

func TestSubjectContext(t *testing.T) {
	sub := &domain.Subject{TenantID: "t-1", Scopes: []string{ScopeTaskWrite}}
	ctx := ContextWithSubject(context.Background(), sub)
	got, ok := SubjectFromContext(ctx)
	if !ok || got.TenantID != "t-1" {
		t.Fatalf("上下文主体还原失败: %+v", got)
	}
	if _, ok := SubjectFromContext(context.Background()); ok {
		t.Fatal("空上下文不应返回主体")
	}
}

// ---------------------------------------------------------------------------
// JWT
// ---------------------------------------------------------------------------

func TestJWTLoginVerifyAndTamper(t *testing.T) {
	auth, st := newTestAuth(t)
	seedTenant(t, st, BootstrapTenantID, BootstrapTenantName)
	ctx := context.Background()

	// 默认密码（AdminPasswordHash 为空）。
	token, sub, err := auth.Login(ctx, "demo", "admin", DefaultAdminPassword)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if !sub.Admin || sub.TenantID != BootstrapTenantID || !sub.Has(ScopeAdminAll) {
		t.Fatalf("登录主体错误: %+v", sub)
	}
	if parts := strings.Split(token, "."); len(parts) != 3 {
		t.Fatalf("JWT 应为三段式: %s", token)
	}

	// 载荷字段齐全。
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatalf("JWT 载荷解码失败: %v", err)
	}
	for _, field := range []string{`"sub"`, `"tid"`, `"name"`, `"scopes"`, `"exp"`, `"iat"`, `"jti"`} {
		if !strings.Contains(string(payload), field) {
			t.Fatalf("JWT 载荷缺少字段 %s: %s", field, payload)
		}
	}

	verified, err := auth.VerifyToken(ctx, token)
	if err != nil {
		t.Fatalf("校验令牌失败: %v", err)
	}
	if !verified.Admin || verified.TenantID != BootstrapTenantID || verified.Username != "admin" {
		t.Fatalf("令牌主体还原错误: %+v", verified)
	}

	// 租户名可作为用户名宽松匹配。
	if _, _, err := auth.Login(ctx, BootstrapTenantName, BootstrapTenantName, DefaultAdminPassword); err != nil {
		t.Fatalf("租户名作为用户名应可登录: %v", err)
	}
	// 租户 ID 作为 tenantKey。
	if _, _, err := auth.Login(ctx, BootstrapTenantID, "admin", DefaultAdminPassword); err != nil {
		t.Fatalf("租户 ID 作为 tenantKey 应可登录: %v", err)
	}

	// 篡改签名。
	parts := strings.Split(token, ".")
	tampered := parts[0] + "." + parts[1] + "." + flipLastChar(parts[2])
	if _, err := auth.VerifyToken(ctx, tampered); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("篡改签名应校验失败，实际: %v", err)
	}
	// 篡改载荷（提权到别的租户）后签名同样失效。
	evilPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"t-evil","tid":"t-evil","name":"admin","scopes":["admin:all"],"exp":9999999999,"iat":1,"jti":"x"}`))
	if _, err := auth.VerifyToken(ctx, parts[0]+"."+evilPayload+"."+parts[2]); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("篡改载荷应校验失败，实际: %v", err)
	}
	// alg=none 降级攻击。
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	if _, err := auth.VerifyToken(ctx, noneHeader+"."+strings.Split(token, ".")[1]+"."); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("alg=none 应被拒绝，实际: %v", err)
	}

	// 过期令牌。
	expired, err := auth.IssueToken(sub, -time.Minute)
	if err != nil {
		t.Fatalf("签发过期令牌失败: %v", err)
	}
	if _, err := auth.VerifyToken(ctx, expired); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("过期令牌应校验失败，实际: %v", err)
	}

	// 错误密码 / 未知租户。
	if _, _, err := auth.Login(ctx, "demo", "admin", "wrong-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("错误密码应返回 ErrUnauthorized，实际: %v", err)
	}
	if _, _, err := auth.Login(ctx, "no-such-tenant", "admin", DefaultAdminPassword); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("未知租户应返回 ErrUnauthorized，实际: %v", err)
	}

	// 停用租户后令牌失效。
	tenant, _ := st.GetTenant(BootstrapTenantID)
	tenant.Status = domain.TenantSuspended
	if err := st.UpdateTenant(tenant); err != nil {
		t.Fatalf("更新租户失败: %v", err)
	}
	if _, err := auth.VerifyToken(ctx, token); !errors.Is(err, ErrForbidden) {
		t.Fatalf("停用租户令牌应返回 ErrForbidden，实际: %v", err)
	}
}

func TestJWTPasswordHashFromBcrypt(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.JWTSecret = "unit-test-jwt-secret"
	hash, err := bcrypt.GenerateFromPassword([]byte("S3cret!Pass"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("生成 bcrypt 哈希失败: %v", err)
	}
	cfg.Auth.AdminPasswordHash = string(hash)
	st := store.New()
	auth := NewAuth(cfg, st)
	seedTenant(t, st, BootstrapTenantID, BootstrapTenantName)

	if _, _, err := auth.Login(context.Background(), "demo", "admin", "S3cret!Pass"); err != nil {
		t.Fatalf("bcrypt 密码登录失败: %v", err)
	}
	if _, _, err := auth.Login(context.Background(), "demo", "admin", DefaultAdminPassword); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("配置了 hash 后默认密码应失效，实际: %v", err)
	}
}

// flipLastChar 修改最后一个字符，用于构造签名被篡改的令牌。
func flipLastChar(s string) string {
	if s == "" {
		return "A"
	}
	b := []byte(s)
	if b[len(b)-1] == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 限流与配额
// ---------------------------------------------------------------------------

func TestRateLimiterBurstAndRefill(t *testing.T) {
	rl := NewRateLimiter(6000, 3) // 100 个/秒，突发 3
	defer rl.Close()

	for i := 0; i < 3; i++ {
		if ok, _ := rl.Allow("t-1"); !ok {
			t.Fatalf("突发额度内第 %d 次应放行", i+1)
		}
	}
	ok, retry := rl.Allow("t-1")
	if ok {
		t.Fatal("突发耗尽后应拒绝")
	}
	if retry <= 0 || retry > 100*time.Millisecond {
		t.Fatalf("retryAfter 不合理: %v", retry)
	}
	// 其他租户互不影响。
	if ok, _ := rl.Allow("t-2"); !ok {
		t.Fatal("其他租户不应受影响")
	}
	if rl.Size() != 2 {
		t.Fatalf("应有 2 个活跃桶，实际 %d", rl.Size())
	}

	// 等待补充后可再次放行。
	time.Sleep(retry + 15*time.Millisecond)
	if ok, _ := rl.Allow("t-1"); !ok {
		t.Fatal("令牌补充后应放行")
	}

	// 空闲桶回收。
	if removed := rl.CleanupIdle(0); removed != 2 {
		t.Fatalf("应回收 2 个空闲桶，实际 %d", removed)
	}
	if rl.Size() != 0 {
		t.Fatalf("回收后不应有活跃桶，实际 %d", rl.Size())
	}
	rl.Close() // 幂等

	// 默认值兜底。
	def := NewRateLimiter(0, 0)
	defer def.Close()
	if def.PerMinute() != DefaultRateLimitPerMin || def.Burst() != DefaultRateLimitPerMin {
		t.Fatalf("默认限流参数错误: %d/%d", def.PerMinute(), def.Burst())
	}
}

func TestQuotaGuard(t *testing.T) {
	g := NewQuotaGuard()
	q := domain.DefaultQuota()

	if err := g.CheckConcurrency(q.MaxConcurrentTasks-1, q); err != nil {
		t.Fatalf("未达并发上限应放行: %v", err)
	}
	err := g.CheckConcurrency(q.MaxConcurrentTasks, q)
	if !IsQuotaError(err) {
		t.Fatalf("达到并发上限应返回 *QuotaError，实际: %v", err)
	}
	qe, ok := AsQuotaError(err)
	if !ok || qe.Kind != QuotaKindConcurrency || qe.Limit != q.MaxConcurrentTasks || qe.Actual != q.MaxConcurrentTasks {
		t.Fatalf("QuotaError 内容错误: %+v", qe)
	}
	if err.Error() == "" {
		t.Fatal("QuotaError 应可读")
	}
	// 0 表示不限制。
	if err := g.CheckConcurrency(10_000, domain.TenantQuota{}); err != nil {
		t.Fatalf("配额为 0 表示不限制: %v", err)
	}

	// 负载降级（不失败）。
	degraded, notes := g.CheckLoad(q.MaxFilesPerTask+1, q.MaxCodeChars+1, q)
	if !degraded || len(notes) != 2 {
		t.Fatalf("超限应降级并给出两条说明: %v %v", degraded, notes)
	}
	if !strings.Contains(strings.Join(notes, "|"), "文件数") || !strings.Contains(strings.Join(notes, "|"), "字符数") {
		t.Fatalf("降级说明内容不完整: %v", notes)
	}
	degraded, notes = g.CheckLoad(1, 1, q)
	if degraded || len(notes) != 0 {
		t.Fatalf("未超限不应降级: %v %v", degraded, notes)
	}
	if got := ClampFiles(q.MaxFilesPerTask+10, q); got != q.MaxFilesPerTask {
		t.Fatalf("文件数裁剪错误: %d", got)
	}
	if got := ClampChars(q.MaxCodeChars+10, q); got != q.MaxCodeChars {
		t.Fatalf("字符数裁剪错误: %d", got)
	}

	// Token 配额。
	if err := g.CheckTokens(q.MaxTokensPerTask, q); err != nil {
		t.Fatalf("未超 Token 配额应放行: %v", err)
	}
	if err := g.CheckTokens(q.MaxTokensPerTask+1, q); !IsQuotaError(err) {
		t.Fatalf("超 Token 配额应返回 *QuotaError，实际: %v", err)
	}
	if qe, ok := AsQuotaError(g.CheckTokens(q.MaxTokensPerTask+1, q)); !ok || qe.Kind != QuotaKindTokens {
		t.Fatalf("Token QuotaError 内容错误: %+v", qe)
	}
	if d := TaskTimeout(q, time.Minute); d != time.Duration(q.MaxTaskSeconds)*time.Second {
		t.Fatalf("任务超时取值错误: %v", d)
	}
}

// ---------------------------------------------------------------------------
// 引导数据初始化（默认租户）
// ---------------------------------------------------------------------------

func TestEnsureSeedIdempotent(t *testing.T) {
	st := store.New()
	ctx := context.Background()

	res1, err := EnsureSeed(ctx, st)
	if err != nil {
		t.Fatalf("首次初始化失败: %v", err)
	}
	if !res1.Created {
		t.Fatal("首次初始化应报告 Created=true")
	}
	if res1.TenantID != BootstrapTenantID {
		t.Fatalf("引导租户 ID 错误: %s", res1.TenantID)
	}
	tenant, ok := st.GetTenant(BootstrapTenantID)
	if !ok || tenant.Status != domain.TenantActive {
		t.Fatalf("引导租户状态异常: %+v", tenant)
	}

	// 只补租户，不再注入任何演示业务数据。
	if got := len(st.ListRepos(BootstrapTenantID)); got != 0 {
		t.Fatalf("不应注入演示仓库，实际 %d", got)
	}
	if got := len(st.ListGroups(BootstrapTenantID)); got != 0 {
		t.Fatalf("不应注入演示分组，实际 %d", got)
	}
	if got := len(st.ListCredentials(BootstrapTenantID)); got != 0 {
		t.Fatalf("不应注入演示凭证，实际 %d", got)
	}
	if got := len(st.ListAPIKeys(BootstrapTenantID)); got != 0 {
		t.Fatalf("不应注入演示接入密钥，实际 %d", got)
	}

	// 第二次调用：幂等。
	res2, err := EnsureSeed(ctx, st)
	if err != nil {
		t.Fatalf("第二次初始化失败: %v", err)
	}
	if res2.Created {
		t.Fatal("第二次调用不应再次创建租户")
	}
	if res2.TenantID != BootstrapTenantID {
		t.Fatalf("第二次调用租户 ID 错误: %s", res2.TenantID)
	}
	if got := len(st.ListTenants()); got != 1 {
		t.Fatalf("幂等调用后租户数量应仍为 1，实际 %d", got)
	}
}

func TestEnsureSeedConcurrent(t *testing.T) {
	st := store.New()
	ctx := context.Background()
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := EnsureSeed(ctx, st)
			done <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatalf("并发初始化失败: %v", err)
		}
	}
	if got := len(st.ListTenants()); got != 1 {
		t.Fatalf("并发初始化后租户应唯一，实际 %d", got)
	}
}

func TestEnsureSeedRejectsNilStore(t *testing.T) {
	if _, err := EnsureSeed(context.Background(), nil); err == nil {
		t.Fatal("store 为空应返回错误")
	}
	if _, err := EnsureSeed(context.Background(), store.New()); err != nil {
		t.Fatalf("正常 store 初始化不应失败: %v", err)
	}
}
