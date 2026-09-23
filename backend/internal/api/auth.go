package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
)

// loginRequest 控制台登录请求体（小驼峰 JSON 字段，开启 DisallowUnknownFields 严格解析）。
type loginRequest struct {
	TenantKey string `json:"tenantKey"`
	Username  string `json:"username"`
	Password  string `json:"password"`
}

// loginResponse 登录响应：token + 主体。
type loginResponse struct {
	Token   string          `json:"token"`
	Subject *domain.Subject `json:"subject"`
}

// apiKeyManager 可选的 API Key 自助管理能力。
//
// 采用"可选接口 + 类型断言"而非修改 domain 包：认证实现（internal/env）若支持
// 密钥自助管理则实现该方法；未实现时接入层返回 503 降级说明，保证契约冻结。
type apiKeyManager interface {
	GenerateAPIKey(tenantID, name string, scopes []string, ttl time.Duration) (string, *domain.APIKey, error)
}

// credentialSealer 可选的凭证加密能力（AES-GCM 加密箱）。
//
// 明文 secret 只作为入参，必须经该接口加密后才允许写入 store.CreateCredential；
// 未提供实现时凭证创建返回 503，绝不允许明文落库（契约 §6.2）。
type credentialSealer interface {
	Seal(plain string) (string, error)
}

// handleLogin POST /api/v1/auth/login —— 控制台登录。
//
// 认证失败统一返回 401，且不区分"租户不存在/用户不存在/密码错误"，避免账号枚举。
func (d *Deps) handleLogin(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "auth.login")

	var req loginRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "auth.login", err)
		writeError(w, r, err)
		return
	}
	req.TenantKey = strings.TrimSpace(req.TenantKey)
	req.Username = strings.TrimSpace(req.Username)
	if req.TenantKey == "" || req.Username == "" || req.Password == "" {
		end(d, r, reqID, "auth.login", errors.New("参数缺失"))
		writeError(w, r, errBadRequest("tenantKey/username/password 均为必填"))
		return
	}

	if d.Auth != nil && !isNilInterface(d.Auth) {
		token, sub, err := d.Auth.Login(r.Context(), req.TenantKey, req.Username, req.Password)
		if err != nil || sub == nil {
			d.log().Warn("登录失败", "req", reqID, "tenantKey", req.TenantKey, "username", req.Username,
				"err", errText(err))
			end(d, r, reqID, "auth.login", errors.New("认证失败"))
			writeError(w, r, errUnauthorized("租户、用户名或密码不正确"))
			return
		}
		sub.RequestID = reqID
		httpx.WriteJSON(w, r, loginResponse{Token: token, Subject: sub})
		end(d, r, reqID, "auth.login", nil)
		return
	}

	// 认证组件缺失（单测 / 轻量化部署）：仅校验租户存在，返回空 token 的匿名主体。
	// ⚠️ 该分支同样只适用于本地轻量化部署，生产必须装配 internal/env 的 Authorizer。
	if d.Store == nil {
		end(d, r, reqID, "auth.login", errors.New("store 未装配"))
		writeError(w, r, errUnavailable("认证组件未装配", nil))
		return
	}
	tenant, ok := d.Store.GetTenantByKey(req.TenantKey)
	if !ok {
		end(d, r, reqID, "auth.login", errors.New("租户不存在"))
		writeError(w, r, errUnauthorized("租户、用户名或密码不正确"))
		return
	}
	sub := &domain.Subject{
		TenantID:   tenant.ID,
		TenantName: tenant.Name,
		Scopes:     []string{scopeAdminAll},
		Admin:      true,
		Username:   req.Username,
		RequestID:  reqID,
	}
	d.log().Warn("认证组件未装配，登录退化为本地放行", "req", reqID, "tenant", tenant.ID)
	httpx.WriteJSON(w, r, loginResponse{Token: "", Subject: sub})
	end(d, r, reqID, "auth.login", nil)
}

// handleProfile GET /api/v1/auth/profile —— 返回当前认证主体。
func (d *Deps) handleProfile(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "auth.profile")
	sub, ok := httpx.SubjectFrom(r.Context())
	if !ok {
		end(d, r, reqID, "auth.profile", errors.New("缺少主体"))
		writeError(w, r, errUnauthorized("缺少认证主体"))
		return
	}
	httpx.WriteJSON(w, r, sub)
	end(d, r, reqID, "auth.profile", nil)
}

// errText 安全提取错误文本（nil 安全）。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
