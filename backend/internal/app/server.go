// Package app 是接入层的服务装配包（HTTP 服务、路由挂载、中间件链、优雅关闭）。
//
// 拆分原因：各资源处理器位于 internal/api，而处理器需要复用 internal/httpx 的
// 响应信封与上下文工具；若服务装配也放在 httpx 内，就会形成
// httpx → api → httpx 的 import cycle。因此把"装配"上移到本包：
//
//	internal/httpx  提供中间件、WebSocket Hub、响应信封（被 api 复用）
//	internal/api    提供各资源 HTTP 处理器（依赖 httpx 与 domain）
//	internal/app    组装二者 + 依赖注入（被 cmd/server 调用）
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	handler "github.com/codeagent/backend/internal/api"
	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/store"
)

// serverVersion 当前接入层版本号，用于 /healthz 与 /metrics。
const serverVersion = "1.0.0"

// shutdownTimeout 优雅关闭的最长等待时间：收到 ctx.Done / SIGINT / SIGTERM 后在
// 该时间内等待在途请求完成，超时强制关闭。
const shutdownTimeout = 15 * time.Second

// Deps 接入层依赖（全部为 domain 接口 + store，便于并行开发与单测）。
//
// 所有字段（除 Cfg / Store 外）都允许为 nil：为 nil 时对应能力自动降级
// （例如 Source 为 nil 时 /repos/{id}/probe 返回 503 降级说明），
// 从而支持"契约先行"的并行开发与纯 fake 单元测试。
type Deps struct {
	// Cfg 全系统配置；为 nil 时使用 config.Default()。
	Cfg *config.Config
	// Store 数据访问层；为 nil 时使用空的内存 store。
	Store *store.Store
	// Auth 认证授权（第二层）；为 nil 时放行并注入匿名主体（仅单测）。
	Auth domain.Authorizer
	// Engine Agent 核心调度层；为 nil 时任务写操作返回 503。
	Engine domain.TaskEngine
	// Repos 仓库分组索引（第三层）；为 nil 时回退直接使用 Store。
	Repos domain.RepoIndex
	// Source 源码解析（第七层）；为 nil 时探测接口降级。
	Source domain.SourceResolver
	// Skills 技能注册中心（第五层）；可为 nil。
	Skills domain.SkillRegistry
	// Runner 技能执行器（第五层）；接入层仅透传依赖，可为 nil。
	Runner domain.SkillRunner
	// MCP 模型管控层（第六层）；为 nil 时 /models 返回空态。
	MCP domain.MCPGateway
	// Recorder 审计与调用轨迹记录器；为 nil 时回退 Store。
	Recorder domain.Recorder
	// Secrets 凭证加密箱（AES-GCM）；为 nil 时控制台无法在线维护含密钥的 AI 模型配置。
	Secrets domain.CredentialBox
	// Bus 实时事件总线；为 nil 时 SSE/WS 降级。
	Bus domain.EventBus
	// Log 结构化日志器；为 nil 时使用 no-op 日志器。
	Log *logx.Logger
	// Hub WebSocket 广播中心；由 NewServer 自动创建，外部无需传入。
	Hub *httpx.Hub
	// WebDir 前端构建产物目录；非空且存在时由后端一并托管 SPA 静态资源。
	WebDir string
}

// Server 接入层服务：持有全部依赖、路由与 WebSocket hub。
type Server struct {
	cfg    *config.Config
	st     *store.Store
	auth   domain.Authorizer
	engine domain.TaskEngine
	repos  domain.RepoIndex
	source domain.SourceResolver
	skills domain.SkillRegistry
	runner domain.SkillRunner
	mcp    domain.MCPGateway
	rec     domain.Recorder
	secrets domain.CredentialBox
	bus     domain.EventBus
	log     *logx.Logger
	hub    *httpx.Hub
	webDir string

	// started 进程/服务启动时间，用于 uptime 与 metrics。
	started time.Time

	once sync.Once
	h    http.Handler
}

// NewServer 构建接入层。
//
// 行为：
//   - Cfg 为 nil → config.Default()；Store 为 nil → store.New() 且 Repos 走空适配器；
//   - Repos 为 nil → 自动使用 handler.NewStoreRepoIndex（*store.Store 与 domain.RepoIndex
//     存在签名差异，详见 internal/api/deps.go 的适配器说明）；
//   - Bus 非 nil 时创建 WebSocket Hub（连接上限 500）。
func NewServer(d Deps) (*Server, error) {
	if d.Cfg == nil {
		d.Cfg = config.Default()
	}
	if err := d.Cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置校验失败: %w", err)
	}
	if d.Store == nil {
		d.Store = store.New()
	}
	log := d.Log
	if log == nil {
		log = logx.Nop()
	}
	if d.Repos == nil {
		d.Repos = handler.NewStoreRepoIndex(d.Store)
	}
	s := &Server{
		cfg:     d.Cfg,
		st:      d.Store,
		auth:    d.Auth,
		engine:  d.Engine,
		repos:   d.Repos,
		source:  d.Source,
		skills:  d.Skills,
		runner:  d.Runner,
		mcp:     d.MCP,
		rec:     d.Recorder,
		secrets: d.Secrets,
		bus:     d.Bus,
		log:     log,
		hub:     d.Hub,
		webDir:  d.WebDir,
		started: time.Now(),
	}
	if s.hub == nil {
		s.hub = httpx.NewHub(d.Bus, log)
	}
	return s, nil
}

// Handler 返回带全部路由与中间件的 http.Handler（只构建一次，可重复调用）。
//
// 中间件顺序（从外到内，与契约 §2 严格一致）：
// Recover → RequestID → AccessLog → CORS → MaxBytes → RateLimit → Auth → mux。
func (s *Server) Handler() http.Handler {
	s.once.Do(func() {
		d := s.handlerDeps()
		mux := http.NewServeMux()
		if regErr := handler.Register(mux, d); regErr != nil {
			// 路由注册失败属于装配期致命错误：暴露给调用方（NewServer 已尽量前置校验）。
			s.log.Error("路由注册失败", "err", regErr.Error())
			mux = http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				httpx.WriteError(w, r, httpx.ErrInternalFn("路由注册失败", regErr))
			})
		}
		// mux 内层先做路径规范化：带方法的 ServeMux pattern 对尾斜杠/重复斜杠零容忍，
		// 不规范化就会把 POST/DELETE 静默变成 404。
		routed := httpx.PathNormalizeMiddleware()(mux)
		h := httpx.AuthMiddleware(s.cfg, s.auth, s.log)(routed)
		h = httpx.RateLimitMiddleware(s.cfg, s.log)(h)
		h = httpx.MaxBytesMiddleware(s.cfg)(h)
		h = httpx.CORSMiddleware(s.cfg)(h)
		h = httpx.AccessLogMiddleware(s.log)(h)
		h = httpx.RequestIDMiddleware()(h)
		h = httpx.RecoverMiddleware(s.log)(h)

		// 可选：由后端一并托管前端 SPA（生产单进程部署形态）。
		if s.webDir != "" {
			if spa, ok := spaHandler(s.webDir, h); ok {
				s.log.Info("已启用前端静态资源托管", "dir", s.webDir)
				h = spa
			} else {
				s.log.Warn("前端静态资源目录不可用，跳过托管", "dir", s.webDir)
			}
		}
		s.h = h
	})
	return s.h
}

// handlerDeps 组装 handler 层依赖（已把 *store.Store 适配为 domain.RepoIndex）。
func (s *Server) handlerDeps() *handler.Deps {
	return &handler.Deps{
		Cfg:      s.cfg,
		Store:    s.st,
		Auth:     s.auth,
		Engine:   s.engine,
		Repos:    s.repos,
		Source:   s.source,
		Skills:   s.skills,
		Runner:   s.runner,
		MCP:      s.mcp,
		Recorder: s.rec,
		Secrets:  s.secrets,
		Bus:      s.bus,
		Log:      s.log,
		Hub:      s.hub,
		Version:  serverVersion,
		Started:  s.started,
	}
}

// Hub 返回 WebSocket 广播中心（供主控在停机时调用 Close）。
func (s *Server) Hub() *httpx.Hub { return s.hub }

// Run 启动 HTTP 服务（含优雅关闭：收到 ctx.Done / SIGINT / SIGTERM 后 15s 内关闭）。
//
// 关闭顺序：停止接收新连接 → 关闭 WebSocket 连接 → 等待在途请求 → 返回。
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Server.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: secondsOr(s.cfg.Server.ReadTimeoutSec, 30),
		WriteTimeout:      time.Duration(s.cfg.Server.WriteTimeoutSec) * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		s.log.Info("HTTP 服务启动", "addr", s.cfg.Server.Addr, "version", serverVersion)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigc)

	select {
	case err := <-errc:
		s.hub.Close()
		if err != nil {
			return fmt.Errorf("HTTP 服务异常退出: %w", err)
		}
		return nil
	case <-ctx.Done():
		s.log.Info("收到上下文取消，开始优雅关闭")
	case sig := <-sigc:
		s.log.Info("收到退出信号，开始优雅关闭", "signal", sig.String())
	}

	s.hub.Close()
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("优雅关闭超时: %w", err)
	}
	<-errc
	s.log.Info("HTTP 服务已关闭")
	return nil
}

func secondsOr(sec, def int) time.Duration {
	if sec <= 0 {
		sec = def
	}
	return time.Duration(sec) * time.Second
}
