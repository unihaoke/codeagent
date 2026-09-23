package handler

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
)

// ---------------------------------------------------------------------------
// AI 设置（模型提供方配置）
//
// 配置文件里的 mcp.providers 是"初始默认"，本组接口维护的是"运行时配置"：
// 保存在 Store（随快照持久化），优先级高于配置文件，且保存后立即热重载到
// 模型管控层。API Key 始终以密文落库，对外只暴露 hasKey 与掩码提示。
// ---------------------------------------------------------------------------

// modelConfigAdmin 模型管控层的可选管理能力（避免修改冻结的 domain.MCPGateway 契约）。
//
// 与 apiKeyManager 同套路：未实现即返回 503 降级说明，绝不退化为写内存不生效。
type modelConfigAdmin interface {
	// ReloadProviders 全量替换模型提供方，返回生效数、被跳过项与错误。
	ReloadProviders(cfgs []config.ProviderConfig) (int, []string, error)
	// TestProvider 用临时配置做一次连通性探测。
	TestProvider(ctx context.Context, pc config.ProviderConfig) (int64, string, error)
}

// providerKinds 受支持的提供方类型。
var providerKinds = map[string]bool{
	"openai": true, "deepseek": true, "azure": true,
	"ollama": true, "openai_compatible": true, "mock": true,
}

// providerTiers 受支持的分层。
var providerTiers = map[string]bool{
	string(domain.TierLight): true, string(domain.TierStrong): true, string(domain.TierFallback): true,
}

// providerSetting AI 设置项的对外视图（不含任何密钥明文）。
type providerSetting struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	BaseURL     string   `json:"baseUrl"`
	Models      []string `json:"models"`
	Tier        string   `json:"tier"`
	MaxTokens   int      `json:"maxTokens"`
	Temperature float64  `json:"temperature"`
	Weight      int      `json:"weight"`
	Enabled     bool     `json:"enabled"`
	// HasKey 是否配置了 API Key。
	HasKey bool `json:"hasKey"`
	// KeyHint 密钥掩码提示（仅末 4 位），用于识别"改了哪一个"。
	KeyHint string `json:"keyHint,omitempty"`
	// Source 配置来源：store（控制台维护）或 config（配置文件/环境变量）。
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// maskSecret 生成密钥提示：保留末 4 位，其余以 • 遮蔽。
func maskSecret(plain string) string {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return ""
	}
	if len(plain) <= 4 {
		return strings.Repeat("•", len(plain))
	}
	return strings.Repeat("•", 8) + plain[len(plain)-4:]
}

// toProviderSetting 把持久化实体转为对外视图。
func toProviderSetting(c domain.ModelProviderConfig) providerSetting {
	models := c.Models
	if models == nil {
		models = []string{}
	}
	return providerSetting{
		Name: c.Name, Kind: c.Kind, BaseURL: c.BaseURL, Models: models,
		Tier: c.Tier, MaxTokens: c.MaxTokens, Temperature: c.Temperature,
		Weight: c.Weight, Enabled: c.Enabled, HasKey: c.HasKey, KeyHint: c.KeyHint,
		Source: "store", UpdatedAt: c.UpdatedAt,
	}
}

// runtimeProviderSettings 返回当前生效的模型配置视图：
// Store 中若有记录则以其为准，否则回显配置文件中的静态配置（只读参考）。
func runtimeProviderSettings(d *Deps) []providerSetting {
	out := []providerSetting{}
	for _, c := range d.Store.ListModelProviders() {
		out = append(out, toProviderSetting(c))
	}
	if len(out) > 0 {
		return out
	}
	// 尚未在控制台维护过任何配置：回显配置文件/环境变量来源的配置便于对照。
	if d.Cfg != nil {
		for _, pc := range d.Cfg.MCP.Providers {
			out = append(out, providerSetting{
				Name: pc.Name, Kind: pc.Kind, BaseURL: pc.BaseURL, Models: pc.Models,
				Tier: pc.Tier, MaxTokens: pc.MaxTokens, Temperature: pc.Temperature,
				Weight: pc.Weight, Enabled: true, HasKey: pc.APIKey != "",
				KeyHint: maskSecret(pc.APIKey), Source: "config",
			})
		}
	}
	return out
}

// RuntimeProviders 导出给装配层使用：把控制台维护的模型配置还原为可装配的运行时配置。
//
// Store 中没有任何记录时返回 nil，调用方应回退到配置文件中的静态 providers。
// secrets 为 nil 或解密失败时该提供方被跳过（宁可不可用，也不明文/错密文上线）。
func RuntimeProviders(st interface {
	ListModelProviders() []domain.ModelProviderConfig
}, secrets domain.CredentialBox) []config.ProviderConfig {
	if st == nil {
		return nil
	}
	list := st.ListModelProviders()
	out := make([]config.ProviderConfig, 0, len(list))
	for _, c := range list {
		if !c.Enabled {
			continue
		}
		key := ""
		if c.HasKey && c.APIKeyEnc != "" && secrets != nil {
			plain, err := secrets.Open(c.APIKeyEnc)
			if err != nil {
				continue
			}
			key = plain
		}
		out = append(out, config.ProviderConfig{
			Name: c.Name, Kind: c.Kind, BaseURL: c.BaseURL, APIKey: key,
			Models: c.Models, Tier: c.Tier, MaxTokens: c.MaxTokens,
			Temperature: c.Temperature, Weight: c.Weight,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// handleGetModelConfig GET /api/v1/models/config —— AI 设置：模型提供方配置清单（脱敏）。
func (d *Deps) handleGetModelConfig(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "model.config.get")
	items := runtimeProviderSettings(d)
	httpx.WriteJSON(w, r, map[string]any{
		"enabled":      d.Cfg == nil || d.Cfg.MCP.Enabled,
		"secretReady":  d.Secrets != nil && !isNilInterface(d.Secrets),
		"hotReload":    modelAdmin(d.MCP) != nil,
		"items":        items,
		"total":        len(items),
		"kinds":        sortedKeys(providerKinds),
		"tiers":        []string{string(domain.TierLight), string(domain.TierStrong), string(domain.TierFallback)},
		"persisted":    len(d.Store.ListModelProviders()) > 0,
	})
	end(d, r, reqID, "model.config.get", nil)
}

// updateConfigRequest 保存 AI 设置的请求体。
type updateConfigRequest struct {
	// Items 全量提交：列表中不存在的已有提供方会被删除。
	Items []struct {
		Name        string   `json:"name"`
		Kind        string   `json:"kind"`
		BaseURL     string   `json:"baseUrl"`
		APIKey      string   `json:"apiKey"`
		Models      []string `json:"models"`
		Tier        string   `json:"tier"`
		MaxTokens   int      `json:"maxTokens"`
		Temperature float64  `json:"temperature"`
		Weight      int      `json:"weight"`
		Enabled     bool     `json:"enabled"`
	} `json:"items"`
}

// handleUpdateModelConfig PUT /api/v1/models/config —— 保存 AI 设置并热重载模型层。
//
// 语义要点：
//   - items 为唯一真源，全量替换（缺失即删除），避免"删不掉的配置僵尸"；
//   - apiKey 留空表示保留已保存的密钥，非空才覆盖并重新加密；
//   - 只有一个可用提供方都没有时才报错回滚，单条不可用仅跳过并提示。
func (d *Deps) handleUpdateModelConfig(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "model.config.put")

	var req updateConfigRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "model.config.put", err)
		writeError(w, r, err)
		return
	}
	secrets := d.Secrets
	if secrets == nil || isNilInterface(secrets) {
		end(d, r, reqID, "model.config.put", errors.New("未装配凭证加密箱"))
		writeError(w, r, errUnavailable("当前部署未启用凭证加密，无法保存模型密钥", nil))
		return
	}
	admin := modelAdmin(d.MCP)
	if admin == nil {
		end(d, r, reqID, "model.config.put", errors.New("模型层不支持热重载"))
		writeError(w, r, errUnavailable("当前模型管控层不支持在线更新配置，请重启服务后重试", nil))
		return
	}

	existing := map[string]domain.ModelProviderConfig{}
	for _, c := range d.Store.ListModelProviders() {
		existing[c.Name] = c
	}

	next := make([]domain.ModelProviderConfig, 0, len(req.Items))
	runtime := make([]config.ProviderConfig, 0, len(req.Items))
	seen := map[string]bool{}
	for _, raw := range req.Items {
		name := strings.TrimSpace(raw.Name)
		if name == "" {
			writeError(w, r, errBadRequest("模型提供方名称不能为空"))
			return
		}
		if seen[name] {
			writeError(w, r, errConflictFn("模型提供方名称重复：" + name))
			return
		}
		seen[name] = true

		kind := strings.ToLower(strings.TrimSpace(raw.Kind))
		if kind == "" {
			kind = "openai_compatible"
		}
		if !providerKinds[kind] {
			writeError(w, r, errBadRequest("不支持的提供方类型：" + kind))
			return
		}
		models := []string{}
		for _, m := range raw.Models {
			if m = strings.TrimSpace(m); m != "" {
				models = append(models, m)
			}
		}
		tier := strings.TrimSpace(raw.Tier)
		if tier == "" {
			tier = string(domain.TierStrong)
		}
		if !providerTiers[tier] {
			writeError(w, r, errBadRequest("不支持的模型层级：" + tier))
			return
		}
		baseURL := strings.TrimSpace(raw.BaseURL)
		if baseURL == "" && kind != "mock" {
			writeError(w, r, errBadRequest("接口地址不能为空：" + name))
			return
		}

		c := domain.ModelProviderConfig{
			Name: name, Kind: kind, BaseURL: baseURL, Models: models, Tier: tier,
			MaxTokens: raw.MaxTokens, Temperature: raw.Temperature,
			Weight: raw.Weight, Enabled: raw.Enabled, UpdatedAt: time.Now(),
		}
		// 密钥：留空沿用旧密文，非空才重新加密。
		if key := strings.TrimSpace(raw.APIKey); key != "" {
			enc, err := secrets.Seal(key)
			if err != nil {
				end(d, r, reqID, "model.config.put", err)
				writeError(w, r, errInternalFn("模型密钥加密失败", err))
				return
			}
			c.APIKeyEnc, c.HasKey, c.KeyHint = enc, true, maskSecret(key)
		} else if old, ok := existing[name]; ok {
			c.APIKeyEnc, c.HasKey, c.KeyHint = old.APIKeyEnc, old.HasKey, old.KeyHint
		}

		if raw.MaxTokens <= 0 {
			c.MaxTokens = 8192
		}
		if c.Temperature <= 0 {
			c.Temperature = 0.1
		}
		if c.Weight <= 0 {
			c.Weight = 1
		}
		next = append(next, c)

		key := ""
		if c.HasKey && c.APIKeyEnc != "" {
			plain, err := secrets.Open(c.APIKeyEnc)
			if err != nil {
				end(d, r, reqID, "model.config.put", err)
				writeError(w, r, errInternalFn("模型密钥解密失败", err))
				return
			}
			key = plain
		}
		runtime = append(runtime, config.ProviderConfig{
			Name: c.Name, Kind: c.Kind, BaseURL: c.BaseURL, APIKey: key,
			Models: c.Models, Tier: c.Tier, MaxTokens: c.MaxTokens,
			Temperature: c.Temperature, Weight: c.Weight,
		})
	}

	loaded, skipped, err := admin.ReloadProviders(runtime)
	if err != nil {
		end(d, r, reqID, "model.config.put", err)
		writeError(w, r, errBadRequest(err.Error()))
		return
	}

	// 热重载成功才全量替换持久化配置，保证"页面看到的 == 实际生效的"。
	if err := d.Store.ReplaceModelProviders(next); err != nil {
		end(d, r, reqID, "model.config.put", err)
		writeError(w, r, err)
		return
	}
	if err := d.Store.Flush(); err != nil {
		d.log().Warn("AI 设置落盘失败", "err", err.Error(), "req", reqID)
	}

	items := make([]providerSetting, 0, len(next))
	for _, c := range next {
		items = append(items, toProviderSetting(c))
	}
	httpx.WriteJSON(w, r, map[string]any{
		"items": items, "total": len(items),
		"loaded": loaded, "skipped": skipped,
		"notice": "配置已保存并立即生效；密钥仅以密文保存，页面不再显示明文。",
	})
	end(d, r, reqID, "model.config.put", nil)
}

// testProviderRequest 连通性测试请求体（不落库）。
type testProviderRequest struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	BaseURL     string   `json:"baseUrl"`
	APIKey      string   `json:"apiKey"`
	Models      []string `json:"models"`
	TestModel   string   `json:"testModel"`
	Tier        string   `json:"tier"`
	MaxTokens   int      `json:"maxTokens"`
	Temperature float64  `json:"temperature"`
}

// handleTestModelProvider POST /api/v1/models/test —— 连通性探测（不保存、不影响线上调度）。
func (d *Deps) handleTestModelProvider(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "model.test")

	admin := modelAdmin(d.MCP)
	if admin == nil {
		end(d, r, reqID, "model.test", errors.New("模型层不支持连通性测试"))
		writeError(w, r, errUnavailable("当前模型管控层不支持连通性测试", nil))
		return
	}
	var req testProviderRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "model.test", err)
		writeError(w, r, err)
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = "openai_compatible"
	}
	if !providerKinds[kind] {
		writeError(w, r, errBadRequest("不支持的提供方类型：" + kind))
		return
	}
	models := []string{}
	seenM := map[string]bool{}
	// testModel 优先探测（若提供且不在 models 中，置于首位）。
	if tm := strings.TrimSpace(req.TestModel); tm != "" {
		models = append(models, tm)
		seenM[tm] = true
	}
	for _, m := range req.Models {
		if m = strings.TrimSpace(m); m != "" && !seenM[m] {
			models = append(models, m)
			seenM[m] = true
		}
	}
	pc := config.ProviderConfig{
		Name: strings.TrimSpace(req.Name), Kind: kind, BaseURL: strings.TrimSpace(req.BaseURL),
		APIKey: strings.TrimSpace(req.APIKey), Models: models,
		Tier: req.Tier, MaxTokens: req.MaxTokens, Temperature: req.Temperature,
	}
	// 未提供密钥时沿用该提供方已保存的密钥（避免"已配好的模型测不通"的误判）。
	if pc.APIKey == "" && pc.Name != "" && d.Secrets != nil && !isNilInterface(d.Secrets) {
		if old, ok := d.Store.GetModelProvider(pc.Name); ok && old.HasKey && old.APIKeyEnc != "" {
			if plain, err := d.Secrets.Open(old.APIKeyEnc); err == nil {
				pc.APIKey = plain
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	ms, model, err := admin.TestProvider(ctx, pc)
	if err != nil {
		end(d, r, reqID, "model.test", err)
		httpx.WriteJSON(w, r, map[string]any{
			"ok": false, "durationMs": ms, "model": model,
			"message": sanitizeText(err.Error()),
		})
		return
	}
	httpx.WriteJSON(w, r, map[string]any{
		"ok": true, "durationMs": ms, "model": model, "message": "连通性正常",
	})
	end(d, r, reqID, "model.test", nil)
}

// modelAdmin 把 domain.MCPGateway 断言为管理接口；未实现返回 nil。
func modelAdmin(mcp domain.MCPGateway) modelConfigAdmin {
	if mcp == nil || isNilInterface(mcp) {
		return nil
	}
	admin, ok := mcp.(modelConfigAdmin)
	if !ok {
		return nil
	}
	return admin
}

// sortedKeys 返回 map 的字典序键列表（用于给前端渲染下拉选项）。
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// modelView 模型视图：提供方配置 + 健康度合并（便于前端按行渲染）。
type modelView struct {
	Target domain.ModelTarget  `json:"target"`
	Health *domain.ModelHealth `json:"health,omitempty"`
}

// handleListModels GET /api/v1/models —— 模型提供方与健康度。
//
// 响应 data 同时提供三种等价视图，兼容不同前端/测试的取用方式：
//   - `targets`：已配置的模型目标（domain.ModelTarget 数组）；
//   - `health`：各模型健康度（domain.ModelHealth 数组）；
//   - `items`：targets 与 health 的合并视图（按 provider+model 对齐）。
//
// MCP 为 nil（未装配模型层）时返回 200 + 空数组 + `{enabled:false}`，
// 便于前端直接渲染空态而不必处理错误分支。
func (d *Deps) handleListModels(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "model.list")

	if d.MCP == nil || isNilInterface(d.MCP) {
		httpx.WriteJSON(w, r, map[string]any{
			"enabled":   false,
			"targets":   []domain.ModelTarget{},
			"health":    []domain.ModelHealth{},
			"items":     []modelView{},
			"total":     0,
			"providers": []domain.ModelTarget{},
			"reason":    "模型管控层未装配，模型能力不可用",
		})
		end(d, r, reqID, "model.list", nil)
		return
	}

	targets := d.MCP.Providers()
	if targets == nil {
		targets = []domain.ModelTarget{}
	}
	health := d.MCP.Health(r.Context())
	if health == nil {
		health = []domain.ModelHealth{}
	}
	healthByKey := map[string]domain.ModelHealth{}
	for _, h := range health {
		healthByKey[h.Provider+"|"+h.Model] = h
	}
	items := make([]modelView, 0, len(targets))
	for _, t := range targets {
		view := modelView{Target: t}
		if h, ok := healthByKey[t.Provider+"|"+t.Model]; ok {
			hc := h
			view.Health = &hc
		}
		items = append(items, view)
	}
	httpx.WriteJSON(w, r, map[string]any{
		"enabled":   true,
		"targets":   targets,
		"health":    health,
		"items":     items,
		"total":     len(items),
		"providers": targets,
	})
	end(d, r, reqID, "model.list", nil)
}

// handleModelStats GET /api/v1/models/stats —— 模型层统计。
func (d *Deps) handleModelStats(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "model.stats")
	if d.MCP == nil || isNilInterface(d.MCP) {
		httpx.WriteJSON(w, r, map[string]any{
			"enabled": false,
			"stats":   domain.ModelStats{},
			"reason":  "模型管控层未装配，模型统计不可用",
		})
		end(d, r, reqID, "model.stats", nil)
		return
	}
	stats := d.MCP.Stats(r.Context())
	httpx.WriteJSON(w, r, map[string]any{"enabled": true, "stats": stats})
	end(d, r, reqID, "model.stats", nil)
}
