package httpx

import (
	"crypto/rand"
	"reflect"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// 包内与跨层共用的 ID / 反射工具。
//
// 这些工具原与接入层 Server 同文件，为打破 httpx ↔ internal/api 的 import cycle，
// Server 装配已上移到 internal/app；此处只保留被中间件、WebSocket 与处理器
// 共同使用的纯函数。
// ---------------------------------------------------------------------------

// newRequestID 生成请求 ID：r-<uuid 前 12 位>（去掉连字符，小写）。
func newRequestID() string {
	return "r-" + shortUUID(12)
}

// newID 生成资源 ID（带业务前缀，便于日志排查）。
func newID(prefix string) string {
	id := shortUUID(20)
	if prefix == "" {
		return id
	}
	return prefix + "-" + id
}

// NewRequestID 暴露请求 ID 生成能力给 handler 层（保持格式统一）。
func NewRequestID() string { return newRequestID() }

// NewResourceID 暴露资源 ID 生成能力给 handler 层（保持格式统一）。
func NewResourceID(prefix string) string { return newID(prefix) }

// isNil 判断接口值是否为 nil（含类型化 nil 指针），避免装配期空指针 panic。
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}

// randomHex 生成 n 字节随机数的十六进制串（用于密钥/ID，密码学安全）。
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, n*2)
	for _, c := range b {
		out = append(out, hexdigits[c>>4], hexdigits[c&0x0f])
	}
	return string(out), nil
}

// shortUUID 返回去掉连字符并截断到 n 位的 UUID 字符串。
func shortUUID(n int) string {
	raw := uuid.New().String()
	compact := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '-' {
			compact = append(compact, raw[i])
		}
	}
	if n > 0 && len(compact) > n {
		compact = compact[:n]
	}
	return string(compact)
}

// uuidNew 统一 UUID 生成入口（便于测试替换）。
func uuidNew() string { return uuid.New().String() }
