// Package httpx 是接入层的实现包：统一响应、中间件、路由与 WebSocket 推送。
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/codeagent/backend/internal/domain"
)

// 业务错误码（与 HTTP 状态码平行）。
const (
	CodeOK            = 0
	CodeBadRequest    = 400
	CodeUnauthorized  = 401
	CodeForbidden     = 403
	CodeNotFound      = 404
	CodeConflict      = 409
	CodeUnprocessable = 422
	CodeRateLimited   = 429
	CodeInternal      = 500
	CodeUnavailable   = 503
)

// Envelope 统一响应信封。
type Envelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
	Data      any    `json:"data,omitempty"`
}

// APIError 可携带业务码的错误。
type APIError struct {
	Code    int
	Message string
	Err     error
}

// Error 实现 error。
func (e *APIError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

// Unwrap 支持 errors.Is/As。
func (e *APIError) Unwrap() error { return e.Err }

// NewError 构造业务错误。
func NewError(code int, msg string, err error) *APIError {
	return &APIError{Code: code, Message: msg, Err: err}
}

// 常用错误快捷构造。
var (
	ErrBadRequest    = func(msg string) *APIError { return NewError(CodeBadRequest, msg, nil) }
	ErrUnauthorized  = func(msg string) *APIError { return NewError(CodeUnauthorized, msg, nil) }
	ErrForbidden     = func(msg string) *APIError { return NewError(CodeForbidden, msg, nil) }
	ErrNotFoundFn    = func(msg string) *APIError { return NewError(CodeNotFound, msg, nil) }
	ErrConflictFn    = func(msg string) *APIError { return NewError(CodeConflict, msg, nil) }
	ErrInternalFn    = func(msg string, err error) *APIError { return NewError(CodeInternal, msg, err) }
	ErrUnprocessable = func(msg string) *APIError { return NewError(CodeUnprocessable, msg, nil) }
	ErrRateLimited   = func(msg string) *APIError { return NewError(CodeRateLimited, msg, nil) }
	ErrUnavailable   = func(msg string, err error) *APIError { return NewError(CodeUnavailable, msg, err) }
)

// WriteJSON 输出成功响应。
func WriteJSON(w http.ResponseWriter, r *http.Request, data any) {
	writeEnvelope(w, r, http.StatusOK, Envelope{
		Code:      CodeOK,
		Message:   "ok",
		RequestID: RequestIDFrom(r.Context()),
		Data:      data,
	})
}

// WritePage 输出分页响应。
func WritePage[T any](w http.ResponseWriter, r *http.Request, p domain.Page[T]) {
	WriteJSON(w, r, p)
}

// WriteError 输出错误响应。
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := &APIError{Code: CodeInternal, Message: "内部错误", Err: err}
	if !errors.As(err, &apiErr) {
		apiErr = NewError(CodeInternal, err.Error(), err)
	}
	status := httpStatusOf(apiErr.Code)
	writeEnvelope(w, r, status, Envelope{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		RequestID: RequestIDFrom(r.Context()),
	})
}

func writeEnvelope(w http.ResponseWriter, r *http.Request, status int, env Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-Id", env.RequestID)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(env)
}

func httpStatusOf(code int) int {
	switch code {
	case CodeOK:
		return http.StatusOK
	case CodeBadRequest, CodeUnprocessable:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// DecodeJSON 解析请求体（带大小限制与严格模式）。
func DecodeJSON(r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return ErrBadRequest("请求体解析失败: " + err.Error())
	}
	return nil
}

// ---------------------------------------------------------------------------
// 上下文辅助
// ---------------------------------------------------------------------------

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeySubject
	ctxKeyCallCtx
	ctxKeyLogger
)

// WithRequestID 注入请求 ID。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestIDFrom 读取请求 ID。
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// WithSubject 注入认证主体。
func WithSubject(ctx context.Context, s *domain.Subject) context.Context {
	return context.WithValue(ctx, ctxKeySubject, s)
}

// SubjectFrom 读取认证主体。
func SubjectFrom(ctx context.Context) (*domain.Subject, bool) {
	s, ok := ctx.Value(ctxKeySubject).(*domain.Subject)
	return s, ok && s != nil
}

// MustSubject 读取主体，缺失时 panic（仅用于已挂认证中间件的路由）。
func MustSubject(ctx context.Context) *domain.Subject {
	s, _ := SubjectFrom(ctx)
	return s
}

// WithCallContext 注入调用上下文。
func WithCallContext(ctx context.Context, cc *domain.CallContext) context.Context {
	return context.WithValue(ctx, ctxKeyCallCtx, cc)
}

// CallContextFrom 读取调用上下文。
func CallContextFrom(ctx context.Context) *domain.CallContext {
	if v, ok := ctx.Value(ctxKeyCallCtx).(*domain.CallContext); ok {
		return v
	}
	return nil
}

// WithLogger 注入日志器。
func WithLogger(ctx context.Context, l any) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// QueryInt 读取整型查询参数。
func QueryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// PageQueryFrom 解析分页查询参数。
func PageQueryFrom(r *http.Request) domain.PageQuery {
	q := domain.PageQuery{
		Page:     QueryInt(r, "page", 1),
		PageSize: QueryInt(r, "pageSize", 20),
		Keyword:  r.URL.Query().Get("keyword"),
		State:    r.URL.Query().Get("state"),
		Sort:     r.URL.Query().Get("sort"),
	}
	q.Normalize()
	return q
}

// PathValue 读取路径参数（包装标准库）。
func PathValue(r *http.Request, name string) string { return r.PathValue(name) }
