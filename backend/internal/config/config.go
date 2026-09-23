// Package config 负责加载与校验系统配置。
//
// 配置优先级：默认值 < 配置文件(JSON) < 环境变量(CA_*)。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 全系统配置。
type Config struct {
	Server    ServerConfig    `json:"server"`
	Auth      AuthConfig      `json:"auth"`
	Security  SecurityConfig  `json:"security"`
	Engine    EngineConfig    `json:"engine"`
	Source    SourceConfig    `json:"source"`
	Sandbox   SandboxConfig   `json:"sandbox"`
	MCP       MCPConfig       `json:"mcp"`
	Skill     SkillConfig     `json:"skill"`
	Store     StoreConfig     `json:"store"`
	Log       LogConfig       `json:"log"`
	Bootstrap BootstrapConfig `json:"bootstrap"`

	// 以下字段仅由环境变量注入，不参与 JSON 序列化。
	mcpAPIKey  string
	mcpBaseURL string
	mcpModel   string
}

// ServerConfig 接入层配置。
type ServerConfig struct {
	Addr            string   `json:"addr"`
	ReadTimeoutSec  int      `json:"readTimeoutSec"`
	WriteTimeoutSec int      `json:"writeTimeoutSec"`
	CORSOrigins     []string `json:"corsOrigins"`
	// RateLimitPerMin 单租户每分钟请求上限（令牌桶）。
	RateLimitPerMin int `json:"rateLimitPerMin"`
	// MaxBodyBytes 单请求体上限。
	MaxBodyBytes int64 `json:"maxBodyBytes"`
	// EnablePlayground 是否允许控制台一键调试任务。
	EnablePlayground bool `json:"enablePlayground"`
	// PublicURL 对外可访问的基础地址（含协议与端口，不含路径），用于生成报告跳转链接。
	PublicURL string `json:"publicUrl"`
}

// AuthConfig 认证配置。
type AuthConfig struct {
	JWTSecret   string `json:"jwtSecret"`
	JWTTTLHours int    `json:"jwtTtlHours"`
	// AdminUsername / AdminPasswordHash 控制台初始管理员。
	AdminUsername     string `json:"adminUsername"`
	AdminPasswordHash string `json:"adminPasswordHash"`
	// AllowAnonymous 允许匿名调用（仅本地轻量化部署建议开启）。
	AllowAnonymous bool `json:"allowAnonymous"`
}

// SecurityConfig 安全隔离配置。
type SecurityConfig struct {
	// EncryptionKey AES-256 密钥（32 字节，base64 或原文）。
	EncryptionKey string `json:"encryptionKey"`
	// MaskEnabled 送模型前对源码脱敏。
	MaskEnabled bool `json:"maskEnabled"`
	// MaskRules 追加的自定义脱敏正则。
	MaskRules []string `json:"maskRules"`
}

// EngineConfig Agent 调度核心层配置。
type EngineConfig struct {
	Workers           int `json:"workers"`
	QueueSize         int `json:"queueSize"`
	TaskTimeoutSec    int `json:"taskTimeoutSec"`
	StageTimeoutSec   int `json:"stageTimeoutSec"`
	MaxRetries        int `json:"maxRetries"`
	IdempotencyTTLSec int `json:"idempotencyTtlSec"`
	// MaxRepairRounds 单任务最大"修复-验证"迭代轮数，保证收敛。
	MaxRepairRounds int `json:"maxRepairRounds"`
	// AutoFix 是否自动产出补丁（关闭则只做根因分析）。
	AutoFix bool `json:"autoFix"`
}

// SourceConfig 源码管理配置。
type SourceConfig struct {
	// CacheDir 本地仓库镜像根目录（bare mirror + 按 commit 检出）。
	CacheDir string `json:"cacheDir"`
	// WorkspaceDir 沙箱工作区根目录。
	WorkspaceDir string `json:"workspaceDir"`
	// CacheCapacity 文件级缓存条目上限（LRU）。
	CacheCapacity int `json:"cacheCapacity"`
	// CacheTTLSec 文件缓存有效期。
	CacheTTLSec int `json:"cacheTtlSec"`
	// FetchDepth 拉取深度，0 表示全量。
	FetchDepth int `json:"fetchDepth"`
	// GitBinary git 可执行文件路径。
	GitBinary string `json:"gitBinary"`
	// CloneTimeoutSec 单仓库拉取超时。
	CloneTimeoutSec int `json:"cloneTimeoutSec"`
	// RespectGitignore 加载文件时遵循 .gitignore。
	RespectGitignore bool `json:"respectGitignore"`
	// IgnorePatterns 额外忽略目录/文件。
	IgnorePatterns []string `json:"ignorePatterns"`
}

// SandboxConfig 沙箱执行层配置。
type SandboxConfig struct {
	Enabled bool `json:"enabled"`
	// AllowCommandExec 是否允许在工作区执行编译/测试命令。
	AllowCommandExec bool `json:"allowCommandExec"`
	// CommandTimeoutSec 单条校验命令超时。
	CommandTimeoutSec int `json:"commandTimeoutSec"`
	// MaxOutputBytes 命令输出截断阈值。
	MaxOutputBytes int `json:"maxOutputBytes"`
	// Checks 语言 → 校验命令模板。
	Checks map[string][]string `json:"checks"`
	// NetworkIsolated 标记沙箱无网络（影响依赖解析降级）。
	NetworkIsolated bool `json:"networkIsolated"`
	// KeepWorkspace 保留工作区以便人工复核。
	KeepWorkspace bool `json:"keepWorkspace"`
}

// MCPConfig 模型管控层配置。
type MCPConfig struct {
	Enabled    bool             `json:"enabled"`
	Default    string           `json:"default"` // 默认 provider 名
	TimeoutSec int              `json:"timeoutSec"`
	MaxRetries int              `json:"maxRetries"`
	Providers  []ProviderConfig `json:"providers"`
	// Routing 分层调度策略。
	Routing RoutingConfig `json:"routing"`
	// Budget 上下文预算。
	Budget ContextBudgetConfig `json:"budget"`
	// Circuit 熔断策略。
	Circuit CircuitConfig `json:"circuit"`
	// Mock 内置 Mock 推理：默认关闭，仅在显式开启时装配。
	//
	// 历史行为是"没有真实模型就悄悄启用 Mock"，会让生产环境产出假结论；
	// 现在未配置任何模型时模型层为空，由控制台「AI 设置」引导配置真实模型。
	Mock MockConfig `json:"mock"`
}

// ProviderConfig 模型提供方配置（OpenAI 兼容协议）。
type ProviderConfig struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // openai | deepseek | azure | ollama | mock | openai_compatible
	BaseURL     string   `json:"baseUrl"`
	APIKey      string   `json:"apiKey"`
	Models      []string `json:"models"`
	Tier        string   `json:"tier"` // light | strong | fallback
	MaxTokens   int      `json:"maxTokens"`
	Temperature float64  `json:"temperature"`
	// Weight 同层多模型时的权重。
	Weight int `json:"weight"`
}

// RoutingConfig 多模型智能调度策略。
type RoutingConfig struct {
	// StrongStages 必须使用强力模型的关键阶段。
	StrongStages []string `json:"strongStages"`
	// LightStages 可用轻量模型的阶段。
	LightStages []string `json:"lightStages"`
	// CostAware 成本优先：简单任务全部走轻量模型。
	CostAware bool `json:"costAware"`
}

// ContextBudgetConfig 上下文预算（字符数，1 token ≈ 1.6 中文字符 / 4 英文字符）。
type ContextBudgetConfig struct {
	MaxChars         int `json:"maxChars"`
	MaxSlices        int `json:"maxSlices"`
	MaxSliceChars    int `json:"maxSliceChars"`
	MaxLogChars      int `json:"maxLogChars"`
	ReserveForOutput int `json:"reserveForOutput"`
}

// CircuitConfig 熔断配置。
type CircuitConfig struct {
	FailureThreshold int `json:"failureThreshold"`
	CooldownSec      int `json:"cooldownSec"`
	HalfOpenProbes   int `json:"halfOpenProbes"`
}

// MockConfig 内置 Mock 推理配置。
type MockConfig struct {
	Enabled   bool   `json:"enabled"`
	Model     string `json:"model"`
	LatencyMS int    `json:"latencyMs"`
}

// SkillConfig 技能层配置。
type SkillConfig struct {
	Enabled           bool            `json:"enabled"`
	Dir               string          `json:"dir"` // 外部技能清单目录（热加载）
	FailureThreshold  int             `json:"failureThreshold"`
	CooldownSec       int             `json:"cooldownSec"`
	DefaultTimeoutSec int             `json:"defaultTimeoutSec"`
	DefaultRetries    int             `json:"defaultRetries"`
	Overrides         []SkillOverride `json:"overrides"`
}

// SkillOverride 技能级覆盖配置（灰度、熔断阈值、兜底链）。
type SkillOverride struct {
	Name             string   `json:"name"`
	Status           string   `json:"status"`
	Version          string   `json:"version"`
	TimeoutSec       int      `json:"timeoutSec"`
	Retries          int      `json:"retries"`
	FailureThreshold int      `json:"failureThreshold"`
	CooldownSec      int      `json:"cooldownSec"`
	FallbackSkills   []string `json:"fallbackSkills"`
	CanaryPercent    int      `json:"canaryPercent"`
}

// StoreConfig 持久化配置。
type StoreConfig struct {
	Driver   string `json:"driver"` // memory | file
	DataFile string `json:"dataFile"`
	// SnapshotIntervalSec 落盘节流间隔。
	SnapshotIntervalSec int `json:"snapshotIntervalSec"`
}

// LogConfig 日志配置。
type LogConfig struct {
	Level  string `json:"level"`  // debug | info | warn | error
	Format string `json:"format"` // text | json
	// File 为空则输出到 stdout。
	File string `json:"file"`
}

// BootstrapConfig 启动引导配置。
//
// 历史上这里有个 DemoData 开关用于注入演示仓库/分组/凭证，
// 现已随演示数据一并移除：系统只在首次启动时确保默认租户存在。
type BootstrapConfig struct{}

// Default 返回默认配置。
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:             ":8080",
			ReadTimeoutSec:   30,
			WriteTimeoutSec:  0,
			CORSOrigins:      []string{"*"},
			RateLimitPerMin:  600,
			MaxBodyBytes:     4 << 20,
			EnablePlayground: true,
		},
		Auth: AuthConfig{
			JWTSecret:     "codeagent-dev-secret-change-me",
			JWTTTLHours:   12,
			AdminUsername: "admin",
			// 默认密码 admin123（bcrypt hash 在 main 中懒生成）
			AdminPasswordHash: "",
		},
		Security: SecurityConfig{
			EncryptionKey: "codeagent-local-dev-encryption-key",
			MaskEnabled:   true,
		},
		Engine: EngineConfig{
			Workers:           4,
			QueueSize:         256,
			TaskTimeoutSec:    900,
			StageTimeoutSec:   180,
			MaxRetries:        2,
			IdempotencyTTLSec: 3600,
			MaxRepairRounds:   2,
			AutoFix:           true,
		},
		Source: SourceConfig{
			CacheDir:         "data/cache",
			WorkspaceDir:     "data/workspace",
			CacheCapacity:    2000,
			CacheTTLSec:      1800,
			FetchDepth:       50,
			GitBinary:        "git",
			CloneTimeoutSec:  180,
			RespectGitignore: true,
			IgnorePatterns:   []string{".git", "node_modules", "dist", "build", "target", "vendor", ".idea", "__pycache__", ".venv"},
		},
		Sandbox: SandboxConfig{
			Enabled:           true,
			AllowCommandExec:  true,
			CommandTimeoutSec: 120,
			MaxOutputBytes:    64 << 10,
			NetworkIsolated:   true,
			KeepWorkspace:     false,
			Checks: map[string][]string{
				"go":         {"go vet ./...", "go build ./..."},
				"java":       {"./mvnw -q -o compile", "mvn -q -o compile"},
				"typescript": {"npx --no-install tsc --noEmit"},
				"javascript": {"node --check"},
				"python":     {"python -m compileall -q ."},
			},
		},
		MCP: MCPConfig{
			Enabled: true,
			// Default 仅在通过环境变量注入单个 provider 时作为其名称；不再预设 mock。
			Default:    "",
			TimeoutSec: 120,
			MaxRetries: 2,
			Providers:  []ProviderConfig{},
			Routing: RoutingConfig{
				StrongStages: []string{"root_cause", "patch_synthesize"},
				LightStages:  []string{"log_clean", "classify", "summary"},
				CostAware:    true,
			},
			Budget: ContextBudgetConfig{
				MaxChars:         120_000,
				MaxSlices:        24,
				MaxSliceChars:    12_000,
				MaxLogChars:      20_000,
				ReserveForOutput: 8_000,
			},
			Circuit: CircuitConfig{FailureThreshold: 3, CooldownSec: 30, HalfOpenProbes: 1},
			Mock:    MockConfig{Enabled: false, Model: "mock-reasoner-v1", LatencyMS: 5},
		},
		Skill: SkillConfig{
			Enabled:           true,
			Dir:               "data/skills",
			FailureThreshold:  3,
			CooldownSec:       30,
			DefaultTimeoutSec: 60,
			DefaultRetries:    1,
		},
		Store: StoreConfig{
			Driver:              "file",
			DataFile:            "data/state.json",
			SnapshotIntervalSec: 2,
		},
		Log:       LogConfig{Level: "info", Format: "text"},
		Bootstrap: BootstrapConfig{},
	}
}

// Load 加载配置：默认值 → 文件 → 环境变量。
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("读取配置文件失败: %w", err)
			}
		} else if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件失败: %w", err)
		}
	}
	applyEnv(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv 使用 CA_ 前缀环境变量覆盖配置。
func applyEnv(c *Config) {
	env := func(key string, dst *string) {
		if v := os.Getenv("CA_" + key); v != "" {
			*dst = v
		}
	}
	envInt := func(key string, dst *int) {
		if v := os.Getenv("CA_" + key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	envBool := func(key string, dst *bool) {
		if v := os.Getenv("CA_" + key); v != "" {
			*dst = strings.EqualFold(v, "true") || v == "1"
		}
	}

	env("ADDR", &c.Server.Addr)
	env("PUBLIC_URL", &c.Server.PublicURL)
	envInt("RATE_LIMIT", &c.Server.RateLimitPerMin)
	env("JWT_SECRET", &c.Auth.JWTSecret)
	envBool("ALLOW_ANONYMOUS", &c.Auth.AllowAnonymous)
	env("ADMIN_USERNAME", &c.Auth.AdminUsername)
	env("ADMIN_PASSWORD_HASH", &c.Auth.AdminPasswordHash)
	env("ENCRYPTION_KEY", &c.Security.EncryptionKey)
	envBool("MASK_ENABLED", &c.Security.MaskEnabled)
	envInt("WORKERS", &c.Engine.Workers)
	envInt("TASK_TIMEOUT_SEC", &c.Engine.TaskTimeoutSec)
	env("CACHE_DIR", &c.Source.CacheDir)
	env("WORKSPACE_DIR", &c.Source.WorkspaceDir)
	env("GIT_BINARY", &c.Source.GitBinary)
	envBool("SANDBOX_ENABLED", &c.Sandbox.Enabled)
	envBool("SANDBOX_ALLOW_EXEC", &c.Sandbox.AllowCommandExec)
	env("MODEL_PROVIDER", &c.MCP.Default)
	env("MODEL_API_KEY", &c.mcpAPIKey)
	env("MODEL_BASE_URL", &c.mcpBaseURL)
	env("MODEL_NAME", &c.mcpModel)
	env("STORE_DRIVER", &c.Store.Driver)
	env("STORE_FILE", &c.Store.DataFile)
	env("LOG_LEVEL", &c.Log.Level)
	env("LOG_FORMAT", &c.Log.Format)
}

// Validate 校验并归一化配置。
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr 不能为空")
	}
	if c.Engine.Workers <= 0 {
		c.Engine.Workers = 4
	}
	if c.Engine.QueueSize <= 0 {
		c.Engine.QueueSize = 128
	}
	if c.Engine.MaxRepairRounds < 0 {
		c.Engine.MaxRepairRounds = 0
	}
	if c.MCP.TimeoutSec <= 0 {
		c.MCP.TimeoutSec = 120
	}
	if c.MCP.Budget.MaxChars <= 0 {
		c.MCP.Budget.MaxChars = 120_000
	}
	if c.Skill.DefaultTimeoutSec <= 0 {
		c.Skill.DefaultTimeoutSec = 60
	}
	if c.Source.CacheCapacity <= 0 {
		c.Source.CacheCapacity = 1000
	}
	if c.Store.Driver == "" {
		c.Store.Driver = "memory"
	}
	if c.MCP.Default == "" {
		c.MCP.Default = "default"
	}
	// 环境变量注入的临时 provider 字段
	if c.mcpAPIKey != "" || c.mcpBaseURL != "" {
		name := c.MCP.Default
		found := false
		for i := range c.MCP.Providers {
			if c.MCP.Providers[i].Name == name {
				found = true
				if c.mcpAPIKey != "" {
					c.MCP.Providers[i].APIKey = c.mcpAPIKey
				}
				if c.mcpBaseURL != "" {
					c.MCP.Providers[i].BaseURL = c.mcpBaseURL
				}
				if c.mcpModel != "" {
					c.MCP.Providers[i].Models = []string{c.mcpModel}
				}
			}
		}
		if !found {
			model := c.mcpModel
			if model == "" {
				model = "deepseek-chat"
			}
			c.MCP.Providers = append(c.MCP.Providers, ProviderConfig{
				Name: name, Kind: "openai", BaseURL: c.mcpBaseURL, APIKey: c.mcpAPIKey,
				Models: []string{model}, Tier: "strong", MaxTokens: 8192, Temperature: 0.1, Weight: 1,
			})
		}
	}
	return nil
}

// TaskTimeout 返回任务超时时间。
func (c *Config) TaskTimeout() time.Duration {
	return time.Duration(c.Engine.TaskTimeoutSec) * time.Second
}

// StageTimeout 返回单阶段超时时间。
func (c *Config) StageTimeout() time.Duration {
	return time.Duration(c.Engine.StageTimeoutSec) * time.Second
}

// --- 内部临时字段（仅用于环境变量注入） ---
