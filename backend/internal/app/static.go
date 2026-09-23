// Package app 提供前端静态资源托管（生产部署形态）。
//
// 设计：前端为 Vue SPA（history 路由），因此：
//   - 命中真实文件（/assets/*.js）直接返回；
//   - 未命中且不是 /api 前缀 → 回退到 index.html，交给前端路由处理；
//   - /api、/healthz、/readyz、/metrics、/ws 一律不拦截。
package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// spaHandler 返回一个 SPA 静态资源处理器。
//
//	dir      前端构建产物目录（如 ./web）
//	fallback 由内层 handler 处理（API 等），未命中静态文件且非 API 路径时回退 index.html
func spaHandler(dir string, fallback http.Handler) (http.Handler, bool) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, false
	}
	index := filepath.Join(dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		return nil, false
	}
	fs := http.FileServer(http.Dir(dir))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API 与运维探针不参与前端路由回退。
		if isBackendPath(r.URL.Path) {
			fallback.ServeHTTP(w, r)
			return
		}
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean != "." && clean != "" {
			candidate := filepath.Join(dir, clean)
			if rel, err := filepath.Rel(dir, candidate); err == nil && !strings.HasPrefix(rel, "..") {
				if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
					// 带内容哈希的静态资源可长缓存；index.html 不缓存。
					if strings.HasPrefix(clean, "assets/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					}
					fs.ServeHTTP(w, r)
					return
				}
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fallback.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFile(w, r, index)
	}), true
}

// isBackendPath 判断是否为后端专属路径。
func isBackendPath(p string) bool {
	switch {
	case strings.HasPrefix(p, "/api/"), strings.EqualFold(p, "/api"):
		return true
	case p == "/healthz", p == "/readyz", p == "/metrics", p == "/ws":
		return true
	case p == "/favicon.ico", p == "/logo.svg", p == "/robots.txt":
		// 浏览器在未认证时会直接请求这些静态资源，交回静态分支处理（未命中即回退 index.html）。
		return false
	case strings.HasPrefix(p, "/ws/"):
		return true
	default:
		return false
	}
}
