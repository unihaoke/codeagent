// Package env 实现租户与安全权限层。
//
// 本包提供四类能力：
//   - 凭证加密箱：AES-256-GCM 加解密 + 日志/源码脱敏（实现 domain.CredentialBox）；
//   - 认证授权：接入层 API Key 与控制台 HS256 JWT（实现 domain.Authorizer）与越权拦截；
//   - 限流配额：租户级整型令牌桶与租户配额守卫；
//   - 示例数据：幂等注入演示租户、凭证、仓库、分组与接入密钥。
//
// 本包只依赖标准库、github.com/google/uuid 与 golang.org/x/crypto/bcrypt。
package env

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
)

// DefaultEncryptionKey 未配置 EncryptionKey 时使用的开发默认密钥。
// 生产环境必须通过配置文件或 CA_ENCRYPTION_KEY 覆盖。
const DefaultEncryptionKey = "codeagent-local-dev-encryption-key"

// maskPlaceholder 脱敏后的占位符。
const maskPlaceholder = "***"

// 内置脱敏规则集中定义：全部在包初始化时编译一次，运行期零编译开销。
var (
	// maskKeyAlt 敏感键名（password=xxx、secretKey: xxx、"api_key":"xxx" 等）。
	maskKeyAlt = `(?:access[_-]?key[_-]?secret|access[_-]?key[_-]?id|access[_-]?key|api[_-]?key|secret[_-]?key|client[_-]?secret|private[_-]?key|auth[_-]?token|refresh[_-]?token|db[_-]?password|password|passwd|pwd|passphrase|secret|token)`

	// reMaskQuotedKV 引号包裹的键值对（JSON/YAML 风格）：保留键名与引号结构。
	// 分隔符兼容 =、: 与 Go 的 :=。
	reMaskQuotedKV = regexp.MustCompile(`(?i)(` + maskKeyAlt + `)(["']?\s*(?::=|[:=])\s*["'])([^"'\r\n]+)(["'])`)
	// reMaskBareKV 裸键值对：password=abc123 / token: abc123 / pwd := "x"。
	reMaskBareKV = regexp.MustCompile(`(?i)(` + maskKeyAlt + `)(\s*(?::=|[:=])\s*)([^\s,;)}\]]+)`)
	// reMaskAuthHeader Authorization: Bearer xxx（保留可读前缀）。
	reMaskAuthHeader = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)((?:bearer|basic|digest|token|api[_-]?key)\s+)(\S+)`)
	// reMaskBearer 不含 Authorization 前缀的裸 Bearer Token。
	reMaskBearer = regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9\-._~+/=]{8,})`)
	// reMaskAWSKeyID AWS AccessKeyId（AKIA/ASIA + 16 位大写字母数字）。
	reMaskAWSKeyID = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	// reMaskKnownToken 常见平台的长期 Token 前缀（-/_ 分隔均覆盖），保留前缀便于人工定位。
	reMaskKnownToken = regexp.MustCompile(`\b((?:sk|ghp|gho|ghu|ghs|ghr|glpat|xoxb|xoxp|xoxa|shpat|dop_v1)[_-])[A-Za-z0-9_\-]{16,}`)
	// reMaskConnString 连接串密码段：scheme://user:pass@host（含 jdbc:mysql:// 形态）。
	reMaskConnString = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://[^/\s:@]*:)([^@/\s]+)(@)`)
)

// AESCredentialBox 凭证加密箱：AES-256-GCM 加解密 + 文本脱敏。
//
// 密钥由 SecurityConfig.EncryptionKey 经 SHA-256 派生为固定 32 字节，
// 密文格式为 base64(RawURLEncoding, nonce || ciphertext)。
type AESCredentialBox struct {
	aead        cipher.AEAD
	extraRules  []*regexp.Regexp
	maskEnabled bool
}

// 编译期校验：AESCredentialBox 必须满足 domain.CredentialBox 契约。
var _ domain.CredentialBox = (*AESCredentialBox)(nil)

// NewCredentialBox 创建凭证加密箱。
//
// EncryptionKey 为空时使用 DefaultEncryptionKey（仅限本地开发）。
// SecurityConfig.MaskRules 中的自定义正则在内置规则之后追加执行，
// 单条规则编译失败会直接返回错误（配置错误应当启动即暴露）。
func NewCredentialBox(cfg config.SecurityConfig) (*AESCredentialBox, error) {
	key := strings.TrimSpace(cfg.EncryptionKey)
	if key == "" {
		key = DefaultEncryptionKey
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("初始化凭证加密箱失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES-GCM 失败: %w", err)
	}
	box := &AESCredentialBox{aead: aead, maskEnabled: cfg.MaskEnabled}
	for i, rule := range cfg.MaskRules {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		re, err := regexp.Compile(rule)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条自定义脱敏规则编译失败: %w", i+1, err)
		}
		box.extraRules = append(box.extraRules, re)
	}
	return box, nil
}

// Enabled 返回配置中是否开启"送模型前脱敏"。
// 注意：Mask/MaskCode 本身始终执行脱敏，是否调用由上层依据该开关决定。
func (b *AESCredentialBox) Enabled() bool {
	return b != nil && b.maskEnabled
}

// Seal 加密明文密钥，返回 base64 密文（随机 nonce 前置）。
func (b *AESCredentialBox) Seal(plain string) (string, error) {
	if b == nil || b.aead == nil {
		return "", errors.New("凭证加密箱未初始化")
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成随机 nonce 失败: %w", err)
	}
	// Seal 将密文追加在 nonce 之后，得到 nonce || ciphertext。
	sealed := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open 解密 Seal 产出的密文。
func (b *AESCredentialBox) Open(sealed string) (string, error) {
	if b == nil || b.aead == nil {
		return "", errors.New("凭证加密箱未初始化")
	}
	raw, err := decodeSealed(sealed)
	if err != nil {
		return "", err
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("密文长度非法：缺少 nonce")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥不匹配或密文被篡改）: %w", err)
	}
	return string(plain), nil
}

// decodeSealed 兼容多种 base64 编码解析密文。
func decodeSealed(sealed string) ([]byte, error) {
	s := strings.TrimSpace(sealed)
	if s == "" {
		return nil, errors.New("密文为空")
	}
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding} {
		if raw, err := enc.DecodeString(s); err == nil {
			return raw, nil
		}
	}
	return nil, errors.New("密文不是合法的 base64 文本")
}

// Mask 对任意文本逐行脱敏。
//
// 设计约束（对 diff 定位很重要）：
//   - 行数不变，行的结构与可读前缀尽量保留；
//   - 幂等：对已脱敏文本再次调用结果不变；
//   - 覆盖 key=value 敏感键、Authorization/Bearer、私钥块、云厂商 AK、
//     常见平台 Token、连接串密码段，以及 SecurityConfig.MaskRules 追加的规则。
func (b *AESCredentialBox) Mask(text string) string {
	if text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	inPEM := false
	for i, line := range lines {
		if inPEM {
			// 私钥块内部一律整体替换，只保留行数。
			lines[i] = maskPlaceholder
			if isPEMEnd(line) {
				inPEM = false
			}
			continue
		}
		if isPEMBegin(line) {
			// 私钥块整体（含首行）替换为 ***。
			lines[i] = maskPlaceholder
			inPEM = true
			continue
		}
		lines[i] = b.maskLine(line)
	}
	return strings.Join(lines, "\n")
}

// MaskCode 对代码文本脱敏，返回脱敏结果与是否发生变化。
func (b *AESCredentialBox) MaskCode(code string) (string, bool) {
	masked := b.Mask(code)
	return masked, masked != code
}

// maskLine 对单行文本应用全部内置规则与自定义规则。
func (b *AESCredentialBox) maskLine(line string) string {
	if line == "" {
		return line
	}
	out := line
	// 顺序：先处理带可读前缀的头部/连接串，再处理键值对，最后追加自定义规则。
	out = reMaskAuthHeader.ReplaceAllString(out, "${1}${2}"+maskPlaceholder)
	out = reMaskBearer.ReplaceAllString(out, "${1}"+maskPlaceholder)
	out = reMaskConnString.ReplaceAllString(out, "${1}"+maskPlaceholder+"${3}")
	out = reMaskAWSKeyID.ReplaceAllString(out, maskPlaceholder)
	out = reMaskKnownToken.ReplaceAllString(out, "${1}"+maskPlaceholder)
	out = reMaskQuotedKV.ReplaceAllString(out, "${1}${2}"+maskPlaceholder+"${4}")
	out = reMaskBareKV.ReplaceAllString(out, "${1}${2}"+maskPlaceholder)
	if b != nil {
		for _, re := range b.extraRules {
			out = replaceUnlessMasked(out, re)
		}
	}
	return out
}

// replaceUnlessMasked 用 *** 替换整段匹配，已脱敏的 *** 片段保持原样以保证幂等。
func replaceUnlessMasked(line string, re *regexp.Regexp) string {
	return re.ReplaceAllStringFunc(line, func(m string) string {
		if isMaskPlaceholder(m) {
			return m
		}
		return maskPlaceholder
	})
}

// isMaskPlaceholder 判断字符串是否已是脱敏占位符。
func isMaskPlaceholder(s string) bool {
	if len(s) < 3 {
		return false
	}
	return strings.Trim(s, "*") == ""
}

// isPEMBegin 判断是否为私钥块起始行。
func isPEMBegin(line string) bool {
	return strings.Contains(line, "-----BEGIN") && strings.Contains(line, "PRIVATE KEY-----")
}

// isPEMEnd 判断是否为私钥块结束行。
func isPEMEnd(line string) bool {
	return strings.Contains(line, "-----END") && strings.Contains(line, "PRIVATE KEY-----")
}
