package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// 提供方抽象
// ---------------------------------------------------------------------------

// Provider 单一模型提供方（OpenAI 兼容协议）。
//
// 实现方不得自行重试：重试、降级、熔断统一由 Gateway 负责，
// 这样"最多尝试几次""退避多久"等策略才有唯一事实来源。
type Provider interface {
	// Name 返回提供方名称（配置中的 name，全局唯一）。
	Name() string
	// Target 返回该提供方的默认模型目标（多模型时取第一个）。
	Target() domain.ModelTarget
	// Chat 执行一次对话补全。messages 已标准化；responseFormat 为 "json" 时要求模型输出 JSON。
	Chat(ctx context.Context, req ProviderRequest) (*ProviderResponse, error)
	// Healthy 返回提供方自身是否可用（配置有效、未被判定为凭证失效）。
	Healthy() bool
}

// ProviderRequest 一次提供方调用请求。
type ProviderRequest struct {
	// Model 目标模型名（由 Gateway 按路由策略选定）。
	Model string
	// Messages 已标准化的对话消息（system 在前）。
	Messages []domain.ModelMessage
	// Temperature 采样温度；<=0 视为未指定，使用提供方默认值。
	Temperature float64
	// MaxTokens 输出 Token 上限；<=0 视为未指定。
	MaxTokens int
	// JSONMode 要求模型输出 JSON。
	JSONMode bool
	// Schema 非空时通过 response_format json_schema 传递（若提供方不支持则仅靠提示词约束）。
	Schema map[string]any
	// Stage 当前流水线阶段（可选）。Mock 提供方据此产出对应阶段的结构化结果。
	Stage string
	// Metadata 附加元信息（可选）。Mock 在缺少 [STAGE:] 标记时从这里识别阶段。
	Metadata map[string]any
}

// ProviderResponse 一次提供方调用结果。
type ProviderResponse struct {
	// Text 模型返回的正文（已去除首尾空白）。
	Text string
	// Usage Token 消耗；提供方未返回时按字符数估算。
	Usage domain.ModelUsage
	// Model 实际使用的模型名（以响应体为准，便于识别服务端改写）。
	Model string
	// Raw 原始响应体（已解码），便于排查与审计。
	Raw map[string]any
}

// providerHTTPError 提供方 HTTP 层错误。
//
// 错误信息只包含状态码与**截断后的响应体**（≤512 字符），
// 且构造前已对 API Key 做脱敏，绝不会把密钥写进错误或日志。
type providerHTTPError struct {
	Provider string
	Status   int
	Body     string
}

// Error 实现 error 接口。
func (e *providerHTTPError) Error() string {
	return fmt.Sprintf("模型提供方 %s 返回 HTTP %d：%s", e.Provider, e.Status, e.Body)
}

// ---------------------------------------------------------------------------
// 构造函数
// ---------------------------------------------------------------------------

// NewProvider 依据配置构造提供方。
//
// 约定：
//   - kind 为 openai / deepseek / azure / ollama / openai_compatible 时统一走 OpenAI 兼容协议；
//   - kind 为 mock 时构造内置 Mock 推理器（离线可跑通全链路）；
//   - API Key 为空且 kind 不是 mock 时返回错误，由 Gateway 决定是否剔除该提供方；
//   - 构造失败不 panic、不重试，配置问题在启动阶段就暴露。
func NewProvider(cfg config.ProviderConfig, timeout time.Duration, log *logx.Logger) (Provider, error) {
	return NewProviderWithHeaders(cfg, timeout, log, nil)
}

// NewProviderWithHeaders 在 NewProvider 基础上支持自定义请求头。
//
// config.ProviderConfig 中没有 Headers 字段，因此自定义头由此入口注入
// （例如企业网关需要的 x-trace-id / 租户路由头）；headers 为空时不添加任何头。
func NewProviderWithHeaders(cfg config.ProviderConfig, timeout time.Duration, log *logx.Logger, headers map[string]string) (Provider, error) {
	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	if kind == "" {
		kind = "openai"
	}
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = kind
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	models := normalizeModels(cfg.Models)

	if kind == "mock" {
		return NewMockProvider(config.ProviderConfig{
			Name:        name,
			Kind:        kind,
			Models:      models,
			Tier:        cfg.Tier,
			MaxTokens:   cfg.MaxTokens,
			Temperature: cfg.Temperature,
			Weight:      cfg.Weight,
		}, 0, log), nil
	}

	if !supportedKind(kind) {
		return nil, fmt.Errorf("模型提供方 %s 的 kind=%q 不受支持（支持 openai/deepseek/azure/ollama/openai_compatible/mock）", name, cfg.Kind)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("模型提供方 %s 未配置 models", name)
	}
	// 契约要求：非 mock 提供方必须显式配置 API Key（ollama 也会带任意占位符，
	// 只是不会把 Key 写入请求头），避免"忘了配 Key"被误判成网络故障。
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("模型提供方 %s 未配置 apiKey", name)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	chatURL, err := resolveChatURL(kind, baseURL, models[0])
	if err != nil {
		return nil, fmt.Errorf("模型提供方 %s 地址不可用: %w", name, err)
	}

	p := &openaiProvider{
		name:        name,
		kind:        kind,
		baseURL:     baseURL,
		apiKey:      apiKey,
		models:      models,
		tier:        normalizeTier(cfg.Tier),
		maxTokens:   cfg.MaxTokens,
		temperature: cfg.Temperature,
		headers:     headers,
		chatURL:     chatURL,
		timeout:     timeout,
		client:      &http.Client{Timeout: timeout},
		log:         loggerOr(log),
	}
	p.healthy.Store(true)
	return p, nil
}

// supportedKind 判断提供方类型是否受支持。
func supportedKind(kind string) bool {
	switch kind {
	case "openai", "deepseek", "azure", "ollama", "openai_compatible":
		return true
	}
	return false
}

// normalizeModels 归一化模型列表：去空白、去空项、去重且保持顺序。
func normalizeModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// normalizeTier 归一化模型层级，非法值回落到 strong。
func normalizeTier(tier string) domain.ModelTier {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case string(domain.TierLight):
		return domain.TierLight
	case string(domain.TierFallback):
		return domain.TierFallback
	case string(domain.TierStrong):
		return domain.TierStrong
	}
	return domain.TierStrong
}

// defaultAzureAPIVersion Azure OpenAI 默认 API 版本。
const defaultAzureAPIVersion = "2024-06-01"

// resolveChatURL 依据 kind 拼接对话补全地址。
//
// 支持"直接给出完整地址"的写法：baseUrl 已含 /chat/completions 时原样使用，
// 便于对接企业内网网关或反向代理。
func resolveChatURL(kind, baseURL, model string) (string, error) {
	switch kind {
	case "ollama":
		if baseURL == "" {
			baseURL = "http://127.0.0.1:11434"
		}
		if strings.HasSuffix(baseURL, "/api/chat") {
			return baseURL, nil
		}
		return baseURL + "/api/chat", nil
	case "azure":
		if baseURL == "" {
			return "", errors.New("azure 必须配置 baseUrl")
		}
		if strings.Contains(baseURL, "/chat/completions") {
			if strings.Contains(baseURL, "api-version=") {
				return baseURL, nil
			}
			sep := "?"
			if strings.Contains(baseURL, "?") {
				sep = "&"
			}
			return baseURL + sep + "api-version=" + defaultAzureAPIVersion, nil
		}
		return baseURL + "/openai/deployments/" + url.PathEscape(model) +
			"/chat/completions?api-version=" + defaultAzureAPIVersion, nil
	default:
		if baseURL == "" {
			switch kind {
			case "openai":
				baseURL = "https://api.openai.com"
			case "deepseek":
				baseURL = "https://api.deepseek.com"
			default:
				return "", errors.New("openai_compatible 必须配置 baseUrl")
			}
		}
		if strings.HasSuffix(baseURL, "/chat/completions") {
			return baseURL, nil
		}
		if strings.HasSuffix(baseURL, "/v1") {
			return baseURL + "/chat/completions", nil
		}
		return baseURL + "/v1/chat/completions", nil
	}
}

// ---------------------------------------------------------------------------
// OpenAI 兼容实现
// ---------------------------------------------------------------------------

// openaiProvider OpenAI 兼容协议的提供方实现。
//
// 覆盖 openai / deepseek / azure / ollama / openai_compatible 五类 kind，
// 差异仅体现在鉴权头、地址与请求体字段上。
type openaiProvider struct {
	name        string
	kind        string
	baseURL     string
	apiKey      string
	models      []string
	tier        domain.ModelTier
	maxTokens   int
	temperature float64
	headers     map[string]string
	chatURL     string
	timeout     time.Duration
	client      *http.Client
	log         *logx.Logger
	// healthy 提供方自身可用性：凭证类错误（401/403）判定为短期内不可自愈。
	healthy atomic.Bool
}

// Name 返回提供方名称。
func (p *openaiProvider) Name() string { return p.name }

// Target 返回该提供方的默认模型目标。
func (p *openaiProvider) Target() domain.ModelTarget {
	return domain.ModelTarget{
		Provider:    p.name,
		Model:       p.firstModel(),
		Tier:        p.tier,
		Endpoint:    p.baseURL,
		MaxTokens:   p.maxTokens,
		Temperature: p.temperature,
	}
}

// Healthy 返回提供方是否可用。
func (p *openaiProvider) Healthy() bool { return p.healthy.Load() }

// firstModel 返回主模型名。
func (p *openaiProvider) firstModel() string {
	if len(p.models) > 0 {
		return p.models[0]
	}
	return ""
}

// Chat 执行一次对话补全。
//
// 超时统一由 context 约束（同时给 http.Client 设置兜底超时）；
// 非 2xx 返回包含状态码与截断响应体的错误；API Key 不出现在任何错误信息中。
func (p *openaiProvider) Chat(ctx context.Context, req ProviderRequest) (*ProviderResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = p.firstModel()
	}
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	payload, err := p.buildPayload(model, req)
	if err != nil {
		return nil, fmt.Errorf("构造模型请求失败: %w", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化模型请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, p.chatURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造模型请求失败: %w", p.redactErr(err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	p.applyAuth(httpReq)
	for k, v := range p.headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		httpReq.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("调用模型提供方 %s 失败（耗时 %dms）: %w", p.name, time.Since(start).Milliseconds(), p.redactErr(err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return nil, &providerHTTPError{Provider: p.name, Status: resp.StatusCode, Body: p.redact(truncateRunes(string(raw), 512))}
		}
		httpErr := &providerHTTPError{Provider: p.name, Status: resp.StatusCode, Body: p.redact(truncateRunes(string(raw), 512))}
		// 401/403 属于凭证或权限问题，短期内不会自愈，标记为不健康，避免持续打无效请求。
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			p.healthy.Store(false)
		}
		return nil, httpErr
	}
	if readErr != nil {
		return nil, fmt.Errorf("读取模型提供方 %s 响应失败: %w", p.name, readErr)
	}

	out, err := p.parseResponse(raw, model, req)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// applyAuth 依据 kind 写入鉴权头。
//
// azure 使用 api-key 头；ollama 无鉴权；其余使用 Bearer Token。
func (p *openaiProvider) applyAuth(req *http.Request) {
	switch p.kind {
	case "azure":
		req.Header.Set("api-key", p.apiKey)
	case "ollama":
		// 本地/内网部署的 ollama 不做鉴权，不发送任何凭证头。
	default:
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
}

// buildPayload 构造请求体。
//
// openai 结构化输出：JSONMode 且给出 Schema 时使用 response_format.type=json_schema；
// 其它提供方退化为 json_object（仅靠提示词约束结构）。
func (p *openaiProvider) buildPayload(model string, req ProviderRequest) (map[string]any, error) {
	if len(req.Messages) == 0 {
		return nil, errors.New("messages 不能为空")
	}
	msgs := make([]map[string]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			role = "user"
		}
		msgs = append(msgs, map[string]string{"role": role, "content": m.Content})
	}
	temp := req.Temperature
	if temp <= 0 {
		temp = p.temperature
	}
	if temp <= 0 {
		temp = 0.1
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.maxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	if p.kind == "ollama" {
		body := map[string]any{
			"model":    model,
			"messages": msgs,
			"stream":   false,
			"options": map[string]any{
				"temperature": temp,
				"num_predict": maxTokens,
			},
		}
		switch {
		case req.Schema != nil:
			// ollama 支持直接传入 JSON Schema 作为 format。
			body["format"] = req.Schema
		case req.JSONMode:
			body["format"] = "json"
		}
		return body, nil
	}

	body := map[string]any{
		"model":       model,
		"messages":    msgs,
		"stream":      false,
		"temperature": temp,
		"max_tokens":  maxTokens,
	}
	if req.JSONMode {
		switch {
		case req.Schema != nil && p.kind == "openai":
			body["response_format"] = map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "codeagent_structured_output",
					"strict": true,
					"schema": req.Schema,
				},
			}
		case req.Schema != nil:
			// 非 openai 提供方对 json_schema 支持不一，统一退化为 json_object。
			body["response_format"] = map[string]any{"type": "json_object"}
		default:
			body["response_format"] = map[string]any{"type": "json_object"}
		}
	}
	return body, nil
}

// parseResponse 解析响应体。
func (p *openaiProvider) parseResponse(raw []byte, model string, req ProviderRequest) (*ProviderResponse, error) {
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("解析模型提供方 %s 响应失败: %v（响应体 %s）", p.name, err, p.redact(truncateRunes(string(raw), 512)))
	}

	text, usage, usedModel := "", domain.ModelUsage{}, model
	if p.kind == "ollama" {
		var r struct {
			Model   string `json:"model"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			PromptEvalCount int `json:"prompt_eval_count"`
			EvalCount       int `json:"eval_count"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("解析 ollama 响应失败: %v", err)
		}
		text = r.Message.Content
		usage = domain.ModelUsage{PromptTokens: r.PromptEvalCount, CompletionTokens: r.EvalCount}
		if strings.TrimSpace(r.Model) != "" {
			usedModel = r.Model
		}
	} else {
		var r struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("解析模型提供方 %s 响应失败: %v", p.name, err)
		}
		if len(r.Choices) == 0 {
			return nil, fmt.Errorf("模型提供方 %s 响应缺少 choices 字段：%s", p.name, p.redact(truncateRunes(string(raw), 512)))
		}
		text = r.Choices[0].Message.Content
		if strings.TrimSpace(text) == "" && strings.TrimSpace(r.Choices[0].Message.ReasoningContent) != "" {
			// 推理模型（如 deepseek-reasoner）可能只返回思维链，做一次兜底取值。
			text = r.Choices[0].Message.ReasoningContent
		}
		usage = domain.ModelUsage{
			PromptTokens:     r.Usage.PromptTokens,
			CompletionTokens: r.Usage.CompletionTokens,
			TotalTokens:      r.Usage.TotalTokens,
		}
		if strings.TrimSpace(r.Model) != "" {
			usedModel = r.Model
		}
	}

	text = strings.TrimSpace(text)
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if usage.TotalTokens == 0 {
		// 提供方未返回 usage（部分代理/私有化部署），按字符数估算，保证配额统计不为 0。
		usage.PromptTokens = estimateTokensByChars(messageChars(req.Messages))
		usage.CompletionTokens = estimateTokensByChars(len(text))
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if text == "" {
		return nil, fmt.Errorf("模型提供方 %s 返回了空内容", p.name)
	}

	return &ProviderResponse{Text: text, Usage: usage, Model: usedModel, Raw: decoded}, nil
}

// redact 对文本做 API Key 脱敏。
func (p *openaiProvider) redact(s string) string {
	if p.apiKey == "" || len(p.apiKey) < 4 {
		return s
	}
	return strings.ReplaceAll(s, p.apiKey, "***")
}

// redactErr 对错误信息做 API Key 脱敏（返回原错误类型，保留超时等语义）。
func (p *openaiProvider) redactErr(err error) error {
	if err == nil {
		return nil
	}
	if p.apiKey == "" || len(p.apiKey) < 4 || !strings.Contains(err.Error(), p.apiKey) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), p.apiKey, "***"))
}

// ---------------------------------------------------------------------------
// 错误分类（供 Gateway 决定是否重试）
// ---------------------------------------------------------------------------

// isRetryableError 判断错误是否值得重试。
//
// 可重试：超时、网络类错误、5xx、408、429；
// 不可重试：4xx 参数/鉴权错误（重试只会浪费预算）、调用方主动取消。
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var httpErr *providerHTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.Status >= 500:
			return true
		case httpErr.Status == http.StatusRequestTimeout, httpErr.Status == http.StatusTooManyRequests:
			return true
		default:
			return false
		}
	}
	if isTimeoutError(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// isTimeoutError 判断错误是否为超时（用于 ModelCall.Status 记录 timeout）。
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// httpStatusOf 返回错误携带的 HTTP 状态码（无则返回 0）。
func httpStatusOf(err error) int {
	var httpErr *providerHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status
	}
	return 0
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// messageChars 统计消息内容的总字符数（UTF-8 字节数）。
func messageChars(msgs []domain.ModelMessage) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)
	}
	return total
}

// estimateTokensByChars 按字符数估算 Token（2 字符 ≈ 1 token）。
func estimateTokensByChars(chars int) int {
	if chars <= 0 {
		return 0
	}
	return chars / 2
}

// loggerOr 返回可用日志器（nil 时返回 no-op）。
func loggerOr(log *logx.Logger) *logx.Logger {
	if log == nil {
		return logx.Nop()
	}
	return log
}

// previewForLog 返回日志可打印的文本前缀（禁止打印完整 prompt）。
func previewForLog(s string, n int) string {
	return truncateRunes(strings.ReplaceAll(s, "\n", "\\n"), n)
}
