// Command server 是 CodeAgent 的进程入口，也是**唯一的依赖注入装配点**。
//
// 装配顺序（自下而上，与七层架构一致）：
//
//	配置 / 日志 / 事件总线 / 数据访问层
//	  → ② 租户&安全权限层（凭证加密箱、认证授权、配额、演示数据）
//	  → ⑦ 源码层（git 镜像 + 版本锁定 + 文件级缓存）与沙箱层（工作区隔离 + 补丁验证）
//	  → ⑤ Skill 插件层（注册中心 + 执行器 + 7 个内置技能）
//	  → ⑥ MCP 模型管控层（网关 + 上下文打包器）
//	  → ④ Agent 调度核心层（九阶段流水线 + 任务引擎）
//	  → ① 接入层（HTTP 路由 / 中间件 / SSE / WebSocket）
//
// 只有本文件同时 import 各层实现包，其余各层之间一律通过 internal/domain 的接口交互。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/app"
	handler "github.com/codeagent/backend/internal/api"
	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/engine"
	"github.com/codeagent/backend/internal/env"
	"github.com/codeagent/backend/internal/mcp"
	"github.com/codeagent/backend/internal/platform/audit"
	"github.com/codeagent/backend/internal/platform/eventbus"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/sandbox"
	"github.com/codeagent/backend/internal/skill"
	"github.com/codeagent/backend/internal/skill/builtin"
	"github.com/codeagent/backend/internal/source"
	"github.com/codeagent/backend/internal/store"
)

// buildVersion 由构建脚本通过 -ldflags 注入。
var buildVersion = "1.0.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfgPath  = flag.String("config", "", "配置文件路径（JSON）。留空则仅使用默认值 + 环境变量 CA_*")
		webDir   = flag.String("web", "", "前端构建产物目录（非空时由后端一并托管 SPA）")
		addr     = flag.String("addr", "", "覆盖监听地址，例如 :9090")
		printVer = flag.Bool("version", false, "打印版本后退出")
		showHelp = flag.Bool("help", false, "打印帮助")
	)
	flag.Parse()

	if *printVer {
		fmt.Printf("codeagent-server %s\n", buildVersion)
		return nil
	}
	if *showHelp {
		flag.Usage()
		return nil
	}

	// ---------------------------------------------------------------------
	// 配置：默认值 < 配置文件 < 环境变量
	// ---------------------------------------------------------------------
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}
	// 相对路径统一锚定到配置文件所在目录（或进程工作目录），避免"从哪启动数据就写哪"。
	baseDir := workingBase(*cfgPath)
	cfg.Source.CacheDir = anchor(baseDir, cfg.Source.CacheDir)
	cfg.Source.WorkspaceDir = anchor(baseDir, cfg.Source.WorkspaceDir)
	cfg.Store.DataFile = anchor(baseDir, cfg.Store.DataFile)
	cfg.Log.File = anchor(baseDir, cfg.Log.File)

	// ---------------------------------------------------------------------
	// 日志与事件总线
	// ---------------------------------------------------------------------
	log, closer, err := logx.New(cfg.Log.Level, cfg.Log.Format, cfg.Log.File)
	if err != nil {
		return err
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
	}
	log.Info("CodeAgent 启动中",
		"version", buildVersion, "addr", cfg.Server.Addr,
		"workers", cfg.Engine.Workers, "providers", len(cfg.MCP.Providers))

	bus := eventbus.New(1024)

	// ---------------------------------------------------------------------
	// 数据访问层
	// ---------------------------------------------------------------------
	var st *store.Store
	switch strings.ToLower(cfg.Store.Driver) {
	case "memory":
		st = store.New()
		log.Info("数据存储：内存模式（进程退出即丢失）")
	default:
		interval := time.Duration(cfg.Store.SnapshotIntervalSec) * time.Second
		st, err = store.OpenFile(cfg.Store.DataFile, interval)
		if err != nil {
			return fmt.Errorf("打开数据文件失败: %w", err)
		}
		log.Info("数据存储：文件快照模式", "file", cfg.Store.DataFile)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Warn("数据落盘失败", "err", err.Error())
		}
	}()

	// ---------------------------------------------------------------------
	// ② 租户&安全权限层
	// ---------------------------------------------------------------------
	box, err := env.NewCredentialBox(cfg.Security)
	if err != nil {
		return fmt.Errorf("初始化凭证加密箱失败: %w", err)
	}
	auth := env.NewAuth(cfg, st)
	rateLimiter := env.NewRateLimiter(cfg.Server.RateLimitPerMin, cfg.Server.RateLimitPerMin/6+10)
	defer rateLimiter.Close()
	_ = env.NewQuotaGuard() // 配额守卫由引擎层按租户配额实现，此处保留实例用于后续扩展

	if _, err := env.EnsureSeed(context.Background(), st); err != nil {
		log.Error("初始化默认租户失败", "err", err.Error())
	}
	// 启动自检：打印已加载的租户与运行数量，便于确认审计指标的数据来源是否就绪。
	if tenants := st.ListTenants(); len(tenants) > 0 {
		totalRuns := 0
		for _, t := range tenants {
			totalRuns += len(st.AllRuns(t.ID))
		}
		log.Info("存储加载完成（审计指标基于运行记录）", "tenants", len(tenants), "runs", totalRuns)
	} else {
		log.Warn("存储为空：未加载到任何租户，审计指标将无数据")
	}

	// 审计与调用轨迹记录器（贯穿全层的可观测实现）
	recorder := audit.New(st, bus)

	// ---------------------------------------------------------------------
	// ⑦ 源码层与沙箱执行层
	// ---------------------------------------------------------------------
	secrets := &secretProvider{st: st, box: box}
	gitMgr := source.NewGitRepoManager(cfg.Source, log.With("layer", "source"))
	defer func() { _ = gitMgr.Close() }()
	resolver := source.NewResolver(cfg.Source, st, secrets, log.With("layer", "source"))
	// 沙箱通过窄接口物化仓库快照：内部复用 Resolver.Materialize（精准 commit 检出）。
	workspace := sandbox.NewManager(cfg.Sandbox, cfg.Source, &materializer{resolver: resolver, log: log}, log.With("layer", "sandbox"))

	// ---------------------------------------------------------------------
	// ⑤ Skill 插件层
	// ---------------------------------------------------------------------
	registry := skill.NewRegistry(cfg.Skill, log.With("layer", "skill"))
	runner := skill.NewRunner(registry, cfg.Skill, recorder, log.With("layer", "skill"))
	if cfg.Skill.Enabled {
		if err := builtin.Register(registry, builtin.Deps{}); err != nil {
			return fmt.Errorf("注册内置技能失败: %w", err)
		}
		if cfg.Skill.Dir != "" {
			if n, err := registry.LoadManifests(cfg.Skill.Dir); err != nil {
				log.Warn("外部技能清单加载失败（不影响内置技能）", "dir", cfg.Skill.Dir, "err", err.Error())
			} else if n > 0 {
				log.Info("外部技能清单已加载", "count", n, "dir", cfg.Skill.Dir)
			}
		}
		log.Info("技能注册中心就绪", "skills", registry.Names())
	}

	// ---------------------------------------------------------------------
	// ⑥ MCP 模型管控层
	// ---------------------------------------------------------------------
	// 控制台「AI 设置」维护的模型配置优先于配置文件/环境变量：
	// 只有在 store 中存在记录时才覆盖，否则仍使用静态配置（首次部署行为不变）。
	if runtime := handler.RuntimeProviders(st, box); len(runtime) > 0 {
		cfg.MCP.Providers = runtime
		log.Info("已加载控制台维护的 AI 模型配置", "providers", len(runtime))
	}
	gateway, err := mcp.NewGateway(cfg.MCP, cfg.Engine, recorder, log.With("layer", "mcp"))
	if err != nil {
		return fmt.Errorf("初始化模型管控层失败: %w", err)
	}
	packer := mcp.NewContextPacker(cfg.MCP.Budget, log.With("layer", "mcp"))
	{
		targets := gateway.Providers()
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			names = append(names, fmt.Sprintf("%s/%s(%s)", t.Provider, t.Model, t.Tier))
		}
		log.Info("模型管控层就绪", "providers", names)
	}

	// ---------------------------------------------------------------------
	// ④ Agent 调度核心层
	// ---------------------------------------------------------------------
	pipeline := engine.NewPipeline(engine.Deps{
		Store:    st,
		Cfg:      cfg,
		Source:   resolver,
		Skills:   runner,
		MCP:      gateway,
		Sandbox:  workspace,
		Packer:   packer,
		Bus:      bus,
		Recorder: recorder,
		Log:      log.With("layer", "engine"),
	})
	taskEngine, err := engine.NewEngine(cfg, st, pipeline, recorder, bus, auth, log.With("layer", "engine"))
	if err != nil {
		return fmt.Errorf("初始化任务引擎失败: %w", err)
	}
	taskEngine.SetPublicURL(cfg.Server.PublicURL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := taskEngine.Start(ctx); err != nil {
		return fmt.Errorf("启动任务引擎失败: %w", err)
	}
	defer func() {
		if err := taskEngine.Stop(); err != nil {
			log.Warn("任务引擎停止异常", "err", err.Error())
		}
	}()

	// ---------------------------------------------------------------------
	// ① 接入层
	// ---------------------------------------------------------------------
	resolvedWeb := ""
	if *webDir != "" {
		resolvedWeb = anchor(baseDir, *webDir)
	}
	server, err := app.NewServer(app.Deps{
		Cfg:      cfg,
		Store:    st,
		Auth:     auth,
		Secrets:  box,
		Engine:   taskEngine,
		Source:   resolver,
		Skills:   registry,
		Runner:   runner,
		MCP:      gateway,
		Recorder: recorder,
		Bus:      bus,
		Log:      log.With("layer", "api"),
		WebDir:   resolvedWeb,
	})
	if err != nil {
		return fmt.Errorf("初始化接入层失败: %w", err)
	}

	log.Info("全部组件装配完成，开始对外服务",
		"console", fmt.Sprintf("http://127.0.0.1%s", normalizeAddr(cfg.Server.Addr)),
		"api", fmt.Sprintf("http://127.0.0.1%s/api/v1", normalizeAddr(cfg.Server.Addr)))

	if err := server.Run(ctx); err != nil {
		return err
	}
	log.Info("CodeAgent 已退出")
	return nil
}

// ---------------------------------------------------------------------------
// 适配器
// ---------------------------------------------------------------------------

// secretProvider 把 env 的凭证加密箱适配为 source.SecretProvider。
//
// 安全要点：密钥只在此处解密并直接交给 git 调用，不进入日志、不进入任务上下文。
type secretProvider struct {
	st  *store.Store
	box domain.CredentialBox
}

// RepoSecret 返回仓库解密后的访问凭证。
func (p *secretProvider) RepoSecret(_ context.Context, tenantID, repoID string) (string, string, error) {
	repo, ok := p.st.GetRepo(tenantID, repoID)
	if !ok {
		return "", "", nil
	}
	if repo.CredentialID == "" {
		return "", "", nil
	}
	cred, ok := p.st.GetCredential(tenantID, repo.CredentialID)
	if !ok || cred.SecretEnc == "" {
		return "", "", nil
	}
	secret, err := p.box.Open(cred.SecretEnc)
	if err != nil {
		return "", "", fmt.Errorf("解密仓库凭证失败（repo=%s）: %w", repo.Key, err)
	}
	return cred.Username, secret, nil
}

// materializer 把 source.Resolver 的物化能力适配为 sandbox.Materializer。
//
// 两者目录语义略有差异（Resolver 输出 <destRoot>/<repoKey>，沙箱要求精确落在 dest 目录），
// 因此这里先物化到临时目录再搬运，保证沙箱工作区结构稳定且不暴露中间目录。
type materializer struct {
	resolver *source.Resolver
	log      *logx.Logger
}

// Materialize 把指定仓库的 commit 快照检出到 dest 目录。
func (m *materializer) Materialize(ctx context.Context, repo *domain.Repository, commit, dest string) error {
	if repo == nil {
		return errors.New("仓库为空")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(dest), ".staging-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	dir, err := m.resolver.Materialize(ctx, domain.RepoRef{Repository: repo, Commit: commit}, staging)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := copyDir(dir, dest); err != nil {
		return err
	}
	if m.log != nil {
		m.log.Debug("仓库快照已物化到沙箱工作区",
			"repo", repo.Key, "commit", shortCommit(commit), "dir", dest)
	}
	return nil
}

// copyDir 递归复制目录（仅用于沙箱工作区物化，不做符号链接跟随以避免越界）。
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 路径工具
// ---------------------------------------------------------------------------

// workingBase 返回相对路径的锚点目录：优先配置文件所在目录，其次进程工作目录。
func workingBase(cfgPath string) string {
	if cfgPath != "" {
		if abs, err := filepath.Abs(cfgPath); err == nil {
			return filepath.Dir(abs)
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// anchor 把相对路径锚定到 base 目录；绝对路径与空串原样返回。
func anchor(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// normalizeAddr 把 ":8080" 归一化为 ":8080"，把 "0.0.0.0:8080" 归一化为 ":8080"（用于日志展示）。
func normalizeAddr(addr string) string {
	if addr == "" {
		return ":8080"
	}
	if i := strings.LastIndex(addr, ":"); i > 0 {
		host := addr[:i]
		if host == "0.0.0.0" || host == "[::]" || host == "::" {
			return addr[i:]
		}
	}
	return addr
}

// shortCommit 截断 commit 便于日志展示。
func shortCommit(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	return c
}
