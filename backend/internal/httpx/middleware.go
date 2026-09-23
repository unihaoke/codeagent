package httpx

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
)

// Middleware 标准中间件签名。
type Middleware func(http.Handler) http.Handler

// 中间件链顺序（从外到内）：
//
//	Recover → RequestID → AccessLog → CORS → MaxBytes → RateLimit → Auth → mux
//
// 顺序不可调整：Recover 必须在最外层才能兜住所有 panic；RequestID 必须在
// AccessLog 之外，保证日志与响应头都带同一个请求 ID；Auth 必须在最内层，
// 保证未认证请求不会污染限流桶与访问日志的租户字段。

// ---------------------------------------------------------------------------
// 1) Recover
// ---------------------------------------------------------------------------

// statusWriter 记录状态码与响应字节数，并透传 Flush/Hijack 等可选能力。
type statusWriter struct {
	http.ResponseWriter
	status  int
	written int
}

// WriteHeader 记录首个状态码并透传（重复调用只记录，不改变已写出的状态码）。
func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += n
	return n, err
}

// Flush 透传 Flush（SSE 必须）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 透传连接劫持（WebSocket 升级必须）。
//
// 说明：net/http 只会对最外层 ResponseWriter 做 Hijacker 类型断言，本中间件包了一层
// 状态码记录器，因此必须显式转发 Hijack，否则 WebSocket 握手会退化为 500。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("底层 ResponseWriter 不支持 Hijack")
	}
	return hj.Hijack()
}

// Unwrap 让 http.NewResponseController 能找到底层 writer（Hijack/Flush/Deadline）。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// RecoverMiddleware 捕获 panic，记录结构化日志（含堆栈），向客户端返回统一 500。
//
// 安全约束：堆栈只进日志，绝不返回给客户端。
func RecoverMiddleware(log *logx.Logger) Middleware {
	if log == nil {
		log = logx.Nop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					reqID := RequestIDFrom(r.Context())
					log.Error("请求处理 panic",
						"req", reqID,
						"method", r.Method,
						"path", r.URL.Path,
						"panic", toText(rec),
						"stack", string(debug.Stack()),
					)
					// 已写入响应头时无法再改状态码，只能记录。
					if sw, ok := w.(*statusWriter); ok && sw.status != 0 {
						return
					}
					WriteError(w, r, ErrInternalFn("服务内部错误", nil))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// toText 把 panic 值安全地转成一行文本（避免堆栈/换行污染日志字段）。
func toText(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		s = x
	case error:
		s = x.Error()
	case interface{ String() string }:
		s = x.String()
	default:
		s = fmt.Sprintf("%v", x)
	}
	return strings.Join(strings.Fields(s), " ")
}

// ---------------------------------------------------------------------------
// 2) RequestID
// ---------------------------------------------------------------------------

// RequestIDHeader 请求 ID 的 HTTP 头名。
const RequestIDHeader = "X-Request-Id"

// RequestIDMiddleware 注入请求 ID：优先取请求头 X-Request-Id，缺省生成 r-<uuid 前 12 位>；
// 注入 context 并回写响应头，保证日志、审计、响应三处一致。
func RequestIDMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimSpace(r.Header.Get(RequestIDHeader))
			if id == "" {
				id = newRequestID()
			}
			if len(id) > 128 {
				id = id[:128]
			}
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
		})
	}
}

// ---------------------------------------------------------------------------
// 3) AccessLog
// ---------------------------------------------------------------------------

// accessLogSkipPaths 高频探针路径不记录访问日志，避免日志淹没。
var accessLogSkipPaths = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
	"/metrics": true,
}

// AccessLogMiddleware 记录方法、路径、状态码、耗时、租户、请求 ID。
func AccessLogMiddleware(log *logx.Logger) Middleware {
	if log == nil {
		log = logx.Nop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)

			if accessLogSkipPaths[r.URL.Path] {
				return
			}
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			tenant := ""
			if sub, ok := SubjectFrom(r.Context()); ok {
				tenant = sub.TenantID
			}
			log.Info("http.access",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"bytes", sw.written,
				"ms", time.Since(start).Milliseconds(),
				"tenant", tenant,
				"req", RequestIDFrom(r.Context()),
				"ip", clientIP(r),
			)
		})
	}
}

// ---------------------------------------------------------------------------
// 4) CORS
// ---------------------------------------------------------------------------

// corsAllowHeaders 允许的请求头（契约固定集合）。
const corsAllowHeaders = "Authorization, X-API-Key, Content-Type, X-Request-Id"

// CORSMiddleware 处理跨域：支持 `*` 与精确白名单，预检请求直接 204。
func CORSMiddleware(cfg *config.Config) Middleware {
	origins := []string{"*"}
	if cfg != nil && len(cfg.Server.CORSOrigins) > 0 {
		origins = cfg.Server.CORSOrigins
	}
	wildcard := false
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		o = strings.TrimSpace(o)
		if o == "*" {
			wildcard = true
			continue
		}
		allowed[strings.ToLower(strings.TrimRight(o, "/"))] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin != "" {
				if wildcard {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else if allowed[strings.ToLower(strings.TrimRight(origin, "/"))] {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Add("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h := w.Header()
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
				h.Set("Access-Control-Max-Age", "600")
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// 5) MaxBytes
// ---------------------------------------------------------------------------

// MaxBytesMiddleware 限制请求体大小（Cfg.Server.MaxBodyBytes，缺省 4MiB）。
func MaxBytesMiddleware(cfg *config.Config) Middleware {
	limit := int64(4 << 20)
	if cfg != nil && cfg.Server.MaxBodyBytes > 0 {
		limit = cfg.Server.MaxBodyBytes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// 6) RateLimit（自实现轻量令牌桶，不依赖 internal/env）
// ---------------------------------------------------------------------------

// tokenBucket 令牌桶：按需补充（lazy refill），无需后台 goroutine。
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	rate     float64 // 每秒补充令牌数
	last     time.Time
	seen     time.Time
}

// allow 尝试取走一个令牌。
func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// limiter 限流器：key（租户或 IP）→ 令牌桶，带惰性清理避免内存膨胀。
type limiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	capacity float64
	rate     float64
	lastGC   time.Time
	ttl      time.Duration
}

func newLimiter(perMin int) *limiter {
	if perMin <= 0 {
		perMin = 600
	}
	return &limiter{
		buckets:  make(map[string]*tokenBucket, 64),
		capacity: float64(perMin),
		rate:     float64(perMin) / 60.0,
		lastGC:   time.Now(),
		ttl:      10 * time.Minute,
	}
}

func (l *limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: l.capacity, capacity: l.capacity, rate: l.rate, last: now, seen: now}
		l.buckets[key] = b
	}
	if now.Sub(l.lastGC) > 5*time.Minute {
		for k, v := range l.buckets {
			if now.Sub(v.lastSeen()) > l.ttl {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	l.mu.Unlock()
	return b.allow(now)
}

func (b *tokenBucket) lastSeen() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seen
}

// rateLimitKey 限流维度：优先租户（X-Tenant-Id，认证前的粗粒度隔离），其次来源 IP。
//
// 说明：中间件顺序中 RateLimit 位于 Auth 之前（契约 §2 固定顺序），此处的
// X-Tenant-Id 仅作分桶键、不代表已认证身份；真正的租户隔离由 Auth + handler 保证。
func rateLimitKey(r *http.Request) string {
	if t := strings.TrimSpace(r.Header.Get("X-Tenant-Id")); t != "" {
		return "t:" + t
	}
	return "ip:" + clientIP(r)
}

// RateLimitMiddleware 按租户/来源 IP 做令牌桶限流，超限返回 429。
func RateLimitMiddleware(cfg *config.Config, log *logx.Logger) Middleware {
	perMin := 600
	if cfg != nil && cfg.Server.RateLimitPerMin > 0 {
		perMin = cfg.Server.RateLimitPerMin
	}
	lim := newLimiter(perMin)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 健康检查与指标接口不参与限流，避免探针被误伤。
			if accessLogSkipPaths[r.URL.Path] || r.URL.Path == "/api/v1/metrics" {
				next.ServeHTTP(w, r)
				return
			}
			if !lim.allow(rateLimitKey(r)) {
				if log != nil {
					log.Warn("请求被限流", "key", rateLimitKey(r), "path", r.URL.Path, "req", RequestIDFrom(r.Context()))
				}
				w.Header().Set("Retry-After", "1")
				WriteError(w, r, ErrRateLimited("请求过于频繁，请稍后重试"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// 7) Auth
// ---------------------------------------------------------------------------

// publicPaths 免认证路径集合。
//
// /metrics 允许通过 ?token=<jwt> 或内网直接访问（保持简单）：
// 本接入层不做指标网关，生产部署应由网络策略限制 /metrics 的暴露面。
var publicPaths = map[string]bool{
	"/healthz":           true,
	"/readyz":            true,
	"/metrics":           true,
	"/api/v1/metrics":    true,
	"/api/v1/auth/login": true,
	"/auth/login":        true,
	"/auth/profile":      false, // 显式声明：profile 需要认证
}

// anonymousTenantDefault 匿名模式缺省租户。
const anonymousTenantDefault = "t-demo"

// AuthMiddleware 统一认证中间件。
//
// 认证策略（严格按契约 §2.2）：
//  1. `Authorization: Bearer <jwt>` → Auth.VerifyToken（控制台管理主体）；
//  2. `X-API-Key: ca_xxx` 或 `Authorization: ApiKey ca_xxx` → Auth.Authenticate（接入层）；
//  3. WebSocket / SSE 等无法自定义头的场景补充 `?token=` / `?apiKey=` 查询参数；
//  4. 两者都缺失且 Cfg.Auth.AllowAnonymous=true → 构造匿名主体；
//  5. 其余情况 → 401。
//
// ⚠️ 匿名模式（AllowAnonymous）风险提示：该模式会把任意匿名请求直接提升为
// 目标租户的 `admin:all` 主体（租户取自可伪造的 X-Tenant-Id 头，缺省 t-demo），
// 等价于**关闭全部认证与隔离**。仅允许用于本地轻量化部署 / 演示 / 单测，
// 生产环境必须在配置中保持 allowAnonymous=false 并由网关提供认证。
func AuthMiddleware(cfg *config.Config, auth domain.Authorizer, log *logx.Logger) Middleware {
	allowAnon := cfg != nil && cfg.Auth.AllowAnonymous
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if publicPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}

			if auth == nil || isNil(auth) {
				// 单测 / 未装配认证实现：放行并注入匿名主体。
				if log != nil {
					log.Debug("认证组件缺失，放行匿名主体", "path", r.URL.Path, "req", RequestIDFrom(r.Context()))
				}
				next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), anonSubject(r))))
				return
			}

			token, apiKey := credentialFrom(r)
			switch {
			case token != "":
				sub, err := auth.VerifyToken(r.Context(), token)
				if err != nil || sub == nil {
					unauthorized(w, r, "登录凭证无效或已过期", err)
					return
				}
				sub.RequestID = RequestIDFrom(r.Context())
				next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), sub)))
			case apiKey != "":
				sub, err := auth.Authenticate(r.Context(), apiKey)
				if err != nil || sub == nil {
					unauthorized(w, r, "API Key 无效或已吊销", err)
					return
				}
				sub.RequestID = RequestIDFrom(r.Context())
				next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), sub)))
			case allowAnon:
				next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), anonSubject(r))))
			default:
				unauthorized(w, r, "缺少认证凭证（Authorization 或 X-API-Key）", nil)
			}
		})
	}
}

// credentialFrom 从请求中提取凭证：Bearer Token 优先，其次 API Key（头或查询参数）。
func credentialFrom(r *http.Request) (token, apiKey string) {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz != "" {
		lower := strings.ToLower(authz)
		switch {
		case strings.HasPrefix(lower, "bearer "):
			token = strings.TrimSpace(authz[len("bearer "):])
		case strings.HasPrefix(lower, "apikey "):
			apiKey = strings.TrimSpace(authz[len("apikey "):])
		}
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if token == "" && apiKey == "" {
		q := r.URL.Query()
		if v := strings.TrimSpace(q.Get("token")); v != "" {
			// EventSource / WebSocket 无法自定义请求头，只能走查询参数。
			// 此处兼容两种凭证：JWT 直接作为 token，接入层 API Key（ca_ 前缀）按 apiKey 处理。
			if strings.HasPrefix(v, apiKeyPrefix) {
				apiKey = v
			} else {
				token = v
			}
		} else if v := strings.TrimSpace(q.Get("apiKey")); v != "" {
			apiKey = v
		}
	}
	return token, apiKey
}

// apiKeyPrefix 接入层 API Key 的固定前缀（与 env 层签发规则一致）。
const apiKeyPrefix = "ca_"

// anonSubject 构造匿名主体（仅本地轻量化部署可用，见 AuthMiddleware 风险说明）。
func anonSubject(r *http.Request) *domain.Subject {
	tenant := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
	if tenant == "" {
		tenant = anonymousTenantDefault
	}
	return &domain.Subject{
		TenantID:   tenant,
		TenantName: tenant,
		Scopes:     []string{"admin:all"},
		Admin:      true,
		Username:   "anonymous",
		RequestID:  RequestIDFrom(r.Context()),
	}
}

func unauthorized(w http.ResponseWriter, r *http.Request, msg string, err error) {
	_ = err // 具体原因只进日志（由调用方 AccessLog 记录状态码），不回显给客户端
	w.Header().Set("WWW-Authenticate", `Bearer realm="codeagent", ApiKey`)
	WriteError(w, r, ErrUnauthorized(msg))
}

// RequireScope 授权辅助：要求主体具备指定 scope（admin:all 通配），否则 403。
//
// 使用约定：租户管理、API Key 管理、技能启停等管理类路由要求 `admin:all`；
// 业务路由要求 `repo:read` / `group:read` / `task:write` 等。
func RequireScope(scope string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sub, ok := SubjectFrom(r.Context())
			if !ok {
				WriteError(w, r, ErrUnauthorized("缺少认证主体"))
				return
			}
			if scope != "" && !sub.Has(scope) {
				WriteError(w, r, ErrForbidden("当前主体缺少权限: "+scope))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// 8) 路径规范化
// ---------------------------------------------------------------------------

// PathNormalizeMiddleware 规范化请求路径，消除两类"看起来一样其实不一样"的 URL：
//
//   - 重复斜杠：`/api/v1//api-keys` → `/api/v1/api-keys`；
//   - 非根路径尾斜杠：`POST /api/v1/api-keys/` → `/api/v1/api-keys`。
//
// 必要性：Go 1.22 ServeMux 的带方法 pattern（`POST /api/v1/api-keys`）是精确匹配，
// 既不像无方法 pattern 那样自动补尾斜杠，也不会对写请求做重定向——
// 一旦 URL 里多一个斜杠，POST/DELETE 就静默变成路由级 404，极难排查。
//
// 该中间件必须紧贴 mux 内层：放在最内层既不污染访问日志里的原始路径，
// 也不影响 CORS/限流等中间件对路径的判断。
func PathNormalizeMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.URL.Path
			normalized := normalizePath(raw)
			if normalized == raw {
				next.ServeHTTP(w, r)
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = normalized
			next.ServeHTTP(w, r2)
		})
	}
}

// normalizePath 合并连续斜杠并去掉非根路径的末尾斜杠。
func normalizePath(p string) string {
	if p == "" {
		return p
	}
	var b strings.Builder
	b.Grow(len(p))
	prevSlash := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '/' {
			if prevSlash {
				continue
			}
			prevSlash = true
		} else {
			prevSlash = false
		}
		b.WriteByte(c)
	}
	out := b.String()
	if len(out) > 1 && strings.HasSuffix(out, "/") {
		out = strings.TrimRight(out, "/")
	}
	if out == "" {
		out = "/"
	}
	return out
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// clientIP 解析客户端 IP：优先 X-Forwarded-For 首段，其次 X-Real-IP，最后 RemoteAddr。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
