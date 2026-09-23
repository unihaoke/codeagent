package handler

import (
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
	"github.com/codeagent/backend/internal/store"
)

// createTenantRequest 创建租户请求体。
type createTenantRequest struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Quota       *domain.TenantQuota `json:"quota,omitempty"`
}

// updateTenantRequest 更新当前租户请求体（名称 / 配额）。
type updateTenantRequest struct {
	Name        *string             `json:"name,omitempty"`
	Description *string             `json:"description,omitempty"`
	Quota       *domain.TenantQuota `json:"quota,omitempty"`
}

// createAPIKeyRequest 创建 API Key 请求体。
type createAPIKeyRequest struct {
	Name string `json:"name"`
	// Scopes 权限范围，缺省 task:write + repo:read。
	Scopes []string `json:"scopes,omitempty"`
	// TTLHours 有效期（小时），<=0 表示永不过期。
	TTLHours int `json:"ttlHours,omitempty"`

	// 回调鉴权（可选）。启用后，该密钥触发的任务终态回调将携带 HMAC-SHA256 签名。
	// CallbackSecret 留空则服务端自动生成；也可由调用方自带（需自行保管，明文仅返回一次）。
	CallbackEnabled bool     `json:"callbackEnabled,omitempty"`
	CallbackSecret  string   `json:"callbackSecret,omitempty"`
	CallbackHosts   []string `json:"callbackHosts,omitempty"`
}

// createCredentialRequest 创建 Git 凭证请求体（secret 明文仅入参，不落库、不回显）。
type createCredentialRequest struct {
	Name     string                `json:"name"`
	Type     domain.CredentialType `json:"type"`
	Username string                `json:"username,omitempty"`
	Secret   string                `json:"secret"`
}

// handleListTenants GET /api/v1/tenants —— 租户列表（管理主体可见全量）。
func (d *Deps) handleListTenants(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "tenant.list")
	tenants := d.Store.ListTenants()
	// 非管理主体只能看到自己（防止横向探测）。
	sub := subject(r)
	if !sub.Has(scopeAdminAll) {
		filtered := make([]domain.Tenant, 0, 1)
		for _, t := range tenants {
			if t.ID == sub.TenantID {
				filtered = append(filtered, t)
			}
		}
		tenants = filtered
	}
	if tenants == nil {
		tenants = []domain.Tenant{}
	}
	httpx.WriteJSON(w, r, map[string]any{"items": tenants, "total": len(tenants)})
	end(d, r, reqID, "tenant.list", nil)
}

// handleCreateTenant POST /api/v1/tenants —— 创建租户（要求 admin:all）。
func (d *Deps) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "tenant.create")

	var req createTenantRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "tenant.create", err)
		writeError(w, r, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		end(d, r, reqID, "tenant.create", errors.New("name 为空"))
		writeError(w, r, errBadRequest("name 为必填"))
		return
	}
	now := time.Now()
	t := &domain.Tenant{
		ID:          httpx.NewResourceID("t"),
		Name:        req.Name,
		Status:      domain.TenantActive,
		Description: req.Description,
		Quota:       domain.DefaultQuota(),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if req.Quota != nil {
		t.Quota = normalizeQuota(*req.Quota)
	}
	if err := d.Store.CreateTenant(t); err != nil {
		end(d, r, reqID, "tenant.create", err)
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	httpx.WriteJSON(w, r, t)
	end(d, r, reqID, "tenant.create", nil)
}

// handleGetCurrentTenant GET /api/v1/tenants/current —— 当前租户详情。
func (d *Deps) handleGetCurrentTenant(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "tenant.getCurrent")
	sub := subject(r)
	t, ok := d.Store.GetTenant(sub.TenantID)
	if !ok {
		// 租户尚未落库（如匿名模式首次访问）：返回主体携带的轻量信息，便于前端渲染。
		httpx.WriteJSON(w, r, &domain.Tenant{
			ID: sub.TenantID, Name: tenantDisplayName(sub), Status: domain.TenantActive,
			Quota: domain.DefaultQuota(),
		})
		end(d, r, reqID, "tenant.getCurrent", nil)
		return
	}
	httpx.WriteJSON(w, r, t)
	end(d, r, reqID, "tenant.getCurrent", nil)
}

// handleUpdateCurrentTenant PUT /api/v1/tenants/current —— 更新当前租户（名称/配额）。
func (d *Deps) handleUpdateCurrentTenant(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "tenant.updateCurrent")

	var req updateTenantRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "tenant.updateCurrent", err)
		writeError(w, r, err)
		return
	}
	sub := subject(r)
	t, ok := d.Store.GetTenant(sub.TenantID)
	if !ok {
		end(d, r, reqID, "tenant.updateCurrent", store.ErrNotFound)
		writeError(w, r, errNotFoundFn("当前租户不存在"))
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			end(d, r, reqID, "tenant.updateCurrent", errors.New("name 为空"))
			writeError(w, r, errBadRequest("name 不能为空"))
			return
		}
		t.Name = name
	}
	if req.Description != nil {
		t.Description = *req.Description
	}
	if req.Quota != nil {
		t.Quota = normalizeQuota(*req.Quota)
	}
	t.UpdatedAt = time.Now()
	if err := d.Store.UpdateTenant(t); err != nil {
		end(d, r, reqID, "tenant.updateCurrent", err)
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, t)
	end(d, r, reqID, "tenant.updateCurrent", nil)
}

// ---------------------------------------------------------------------------
// API Key
// ---------------------------------------------------------------------------

// apiKeyView API Key 列表视图：绝不返回明文与哈希（契约 §6.2）。
type apiKeyView struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenantId"`
	Name       string    `json:"name"`
	KeyPrefix  string    `json:"keyPrefix"`
	Scopes     []string  `json:"scopes"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
	Revoked    bool      `json:"revoked"`
	CreatedAt  time.Time `json:"createdAt"`
	// 回调鉴权状态（不返回密钥密文/明文）。
	CallbackEnabled   bool     `json:"callbackEnabled"`
	CallbackMode      string   `json:"callbackMode,omitempty"`
	HasCallbackSecret bool     `json:"hasCallbackSecret"`
	CallbackHosts     []string `json:"callbackHosts,omitempty"`
}

// maskAPIKey 把 APIKey 转换为脱敏视图（丢弃 KeyHash）。
func maskAPIKey(k domain.APIKey) apiKeyView {
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return apiKeyView{
		ID: k.ID, TenantID: k.TenantID, Name: k.Name, KeyPrefix: k.KeyPrefix,
		Scopes: scopes, ExpiresAt: k.ExpiresAt, LastUsedAt: k.LastUsedAt,
		Revoked: k.Revoked, CreatedAt: k.CreatedAt,
		CallbackEnabled:    k.CallbackEnabled,
		CallbackMode:       k.CallbackMode,
		HasCallbackSecret:  k.CallbackSecretEnc != "",
		CallbackHosts:      k.CallbackHosts,
	}
}

// handleListAPIKeys GET /api/v1/api-keys —— 密钥列表（脱敏）。
func (d *Deps) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "apikey.list")
	keys := d.Store.ListAPIKeys(subject(r).TenantID)
	out := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, maskAPIKey(k))
	}
	httpx.WriteJSON(w, r, map[string]any{"items": out, "total": len(out)})
	end(d, r, reqID, "apikey.list", nil)
}

// handleCreateAPIKey POST /api/v1/api-keys —— 创建密钥，明文 key 仅返回一次。
func (d *Deps) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "apikey.create")

	var req createAPIKeyRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "apikey.create", err)
		writeError(w, r, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		end(d, r, reqID, "apikey.create", errors.New("name 为空"))
		writeError(w, r, errBadRequest("name 为必填"))
		return
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{scopeTaskWrite, scopeRepoRead}
	}
	if req.TTLHours < 0 {
		end(d, r, reqID, "apikey.create", errors.New("ttlHours 非法"))
		writeError(w, r, errBadRequest("ttlHours 不能为负数"))
		return
	}
	ttl := time.Duration(req.TTLHours) * time.Hour

	// 必须走 domain.Authorizer 的可选能力；未实现则 503 降级说明（不修改 domain 包）。
	mgr, ok := d.Auth.(apiKeyManager)
	if !ok || d.Auth == nil || isNilInterface(d.Auth) {
		end(d, r, reqID, "apikey.create", errors.New("auth 未实现 apiKeyManager"))
		writeError(w, r, errUnavailable("当前认证实现不支持密钥自助管理", nil))
		return
	}
	plain, key, err := mgr.GenerateAPIKey(subject(r).TenantID, req.Name, scopes, ttl)
	if err != nil || key == nil {
		end(d, r, reqID, "apikey.create", err)
		writeError(w, r, err)
		return
	}
	// 回调鉴权（可选）：密钥与接入密钥/服务绑定，终态回调携带 HMAC 签名。
	var callbackSecret string
	if req.CallbackEnabled {
		sealer, ok := d.Auth.(credentialSealer)
		if !ok || d.Auth == nil || isNilInterface(d.Auth) {
			end(d, r, reqID, "apikey.create", errors.New("auth 未实现 credentialSealer"))
			writeError(w, r, errUnavailable("当前认证实现不支持回调密钥加密", nil))
			return
		}
		callbackSecret = strings.TrimSpace(req.CallbackSecret)
		if callbackSecret == "" {
			gen := func() (string, error) {
				const alpha = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
				buf := make([]byte, 40)
				for i := range buf {
					v, gerr := rand.Int(rand.Reader, big.NewInt(int64(len(alpha))))
					if gerr != nil {
						return "", gerr
					}
					buf[i] = alpha[v.Int64()]
				}
				return string(buf), nil
			}
			cs, gerr := gen()
			if gerr != nil {
				end(d, r, reqID, "apikey.create", gerr)
				writeError(w, r, errUnavailable("生成回调密钥失败", gerr))
				return
			}
			callbackSecret = cs
		}
		enc, serr := sealer.Seal(callbackSecret)
		if serr != nil {
			end(d, r, reqID, "apikey.create", serr)
			writeError(w, r, errUnavailable("加密回调密钥失败", serr))
			return
		}
		key.CallbackEnabled = true
		key.CallbackSecretEnc = enc
		key.CallbackMode = "hmac"
		key.CallbackHosts = req.CallbackHosts
		// GenerateAPIKey 已先行落库（无回调字段），此处覆盖写入完整密钥。
		if err := d.Store.CreateAPIKey(key); err != nil {
			end(d, r, reqID, "apikey.create", err)
			writeError(w, r, errUnavailable("保存回调配置失败", err))
			return
		}
	}

	// 明文 key 只在此处返回一次；列表接口永不返回明文与哈希。
	w.WriteHeader(http.StatusCreated)
	resp := map[string]any{
		"plainKey": plain,
		"apiKey":   maskAPIKey(*key),
		"notice":   "明文密钥仅此一次返回，请立即妥善保存；服务端只保存哈希。",
	}
	if req.CallbackEnabled {
		resp["callbackSecret"] = callbackSecret
		resp["callbackNotice"] = "回调签名密钥仅此一次返回，请配置到对端服务；服务端仅保存密文。"
	}
	httpx.WriteJSON(w, r, resp)
	end(d, r, reqID, "apikey.create", nil)
}

// handleRevokeAPIKey DELETE /api/v1/api-keys/{id} —— 吊销密钥。
func (d *Deps) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "apikey.revoke")
	id := pathValue(r, "id")
	if id == "" {
		end(d, r, reqID, "apikey.revoke", errors.New("id 为空"))
		writeError(w, r, errBadRequest("id 为必填"))
		return
	}
	// 先区分"不存在"与"越权"：两者都必须拒绝，但语义不同。
	// 旧实现把这两种情况都折叠成 404（store 里的 k.TenantID != tenantID 也返回
	// ErrNotFound），导致控制台实操时"密钥明明在列表里却提示不存在"，无从排障。
	tenantID := strings.TrimSpace(subject(r).TenantID)
	existing, found := d.Store.GetAPIKey(id)
	if !found {
		end(d, r, reqID, "apikey.revoke", store.ErrNotFound)
		writeError(w, r, errNotFoundFn("接入密钥不存在："+id))
		return
	}
	if strings.TrimSpace(existing.TenantID) != tenantID {
		end(d, r, reqID, "apikey.revoke", errors.New("跨租户吊销被拒绝"))
		writeError(w, r, errForbidden("无权吊销其他租户的接入密钥"))
		return
	}
	if err := d.Store.RevokeAPIKey(tenantID, id); err != nil {
		end(d, r, reqID, "apikey.revoke", err)
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, map[string]any{"id": id, "revoked": true})
	end(d, r, reqID, "apikey.revoke", nil)
}

// ---------------------------------------------------------------------------
// Git 凭证
// ---------------------------------------------------------------------------

// credentialView 凭证视图：不含任何密文（契约 §6.2）。
type credentialView struct {
	ID        string                `json:"id"`
	TenantID  string                `json:"tenantId"`
	Name      string                `json:"name"`
	Type      domain.CredentialType `json:"type"`
	Username  string                `json:"username,omitempty"`
	HasSecret bool                  `json:"hasSecret"`
	CreatedAt time.Time             `json:"createdAt"`
	UpdatedAt time.Time             `json:"updatedAt"`
}

func maskCredential(c domain.Credential) credentialView {
	return credentialView{
		ID: c.ID, TenantID: c.TenantID, Name: c.Name, Type: c.Type, Username: c.Username,
		// 即使存储层意外带回密文，也在此强制置为"仅有密钥"的布尔事实。
		HasSecret: c.HasSecret || c.SecretEnc != "",
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// handleListCredentials GET /api/v1/credentials —— 凭证列表（不含密文）。
func (d *Deps) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "credential.list")
	creds := d.Store.ListCredentials(subject(r).TenantID)
	out := make([]credentialView, 0, len(creds))
	for _, c := range creds {
		out = append(out, maskCredential(c))
	}
	httpx.WriteJSON(w, r, map[string]any{"items": out, "total": len(out)})
	end(d, r, reqID, "credential.list", nil)
}

// handleCreateCredential POST /api/v1/credentials —— 创建凭证（明文仅入参，加密后落库）。
func (d *Deps) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "credential.create")

	var req createCredentialRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "credential.create", err)
		writeError(w, r, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		end(d, r, reqID, "credential.create", errors.New("name 为空"))
		writeError(w, r, errBadRequest("name 为必填"))
		return
	}
	if req.Type == "" {
		req.Type = domain.CredToken
	}
	if !validCredentialType(req.Type) {
		end(d, r, reqID, "credential.create", errors.New("凭证类型非法"))
		writeError(w, r, errBadRequest("type 非法，可选 ssh_key/token/basic/none"))
		return
	}

	// 必须经加密箱 Seal，禁止明文落库；未提供 sealer 时 503。
	//
	// 加密箱由认证层实现（internal/env 的 apiKeyManager/credentialSealer 同体），
	// 因此这里只对 Auth 做一次可选接口断言：未实现即降级，绝不退化为明文存储。
	sealer, ok := d.Auth.(credentialSealer)
	if !ok || d.Auth == nil || isNilInterface(d.Auth) {
		end(d, r, reqID, "credential.create", errors.New("未提供 credentialSealer"))
		writeError(w, r, errUnavailable("当前部署未启用凭证加密，无法创建 Git 凭证", nil))
		return
	}

	now := time.Now()
	cred := &domain.Credential{
		ID:        httpx.NewResourceID("cred"),
		TenantID:  subject(r).TenantID,
		Name:      req.Name,
		Type:      req.Type,
		Username:  req.Username,
		HasSecret: strings.TrimSpace(req.Secret) != "",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if cred.HasSecret {
		sealed, err := sealer.Seal(req.Secret)
		if err != nil {
			end(d, r, reqID, "credential.create", err)
			writeError(w, r, errInternalFn("凭证加密失败", err))
			return
		}
		cred.SecretEnc = sealed
	}
	if err := d.Store.CreateCredential(cred); err != nil {
		end(d, r, reqID, "credential.create", err)
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	httpx.WriteJSON(w, r, maskCredential(*cred)) // 响应中 HasSecret=true 且不含密文
	end(d, r, reqID, "credential.create", nil)
}

// handleDeleteCredential DELETE /api/v1/credentials/{id} —— 删除凭证。
func (d *Deps) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "credential.delete")
	id := pathValue(r, "id")
	if id == "" {
		end(d, r, reqID, "credential.delete", errors.New("id 为空"))
		writeError(w, r, errBadRequest("id 为必填"))
		return
	}
	if err := d.Store.DeleteCredential(subject(r).TenantID, id); err != nil {
		end(d, r, reqID, "credential.delete", err)
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, map[string]any{"id": id, "deleted": true})
	end(d, r, reqID, "credential.delete", nil)
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func validCredentialType(t domain.CredentialType) bool {
	switch t {
	case domain.CredSSHKey, domain.CredToken, domain.CredBasic, domain.CredNone:
		return true
	default:
		return false
	}
}

// normalizeQuota 修正非法配额（<=0 的项回落为默认值，避免租户被"零配额"锁死）。
func normalizeQuota(q domain.TenantQuota) domain.TenantQuota {
	def := domain.DefaultQuota()
	if q.MaxConcurrentTasks <= 0 {
		q.MaxConcurrentTasks = def.MaxConcurrentTasks
	}
	if q.MaxFilesPerTask <= 0 {
		q.MaxFilesPerTask = def.MaxFilesPerTask
	}
	if q.MaxCodeChars <= 0 {
		q.MaxCodeChars = def.MaxCodeChars
	}
	if q.MaxTokensPerTask <= 0 {
		q.MaxTokensPerTask = def.MaxTokensPerTask
	}
	if q.MaxTaskSeconds <= 0 {
		q.MaxTaskSeconds = def.MaxTaskSeconds
	}
	return q
}

// tenantDisplayName 返回主体的租户展示名。
func tenantDisplayName(sub *domain.Subject) string {
	if sub == nil {
		return ""
	}
	if sub.TenantName != "" {
		return sub.TenantName
	}
	return sub.TenantID
}
