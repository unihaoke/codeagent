# =============================================================================
# CodeAgent 多阶段构建：前端静态资源 + 后端单二进制
# 构建： docker build -t codeagent:1.0.0 .
# 运行： docker run -d -p 8080:8080 -v codeagent-data:/app/data \
#          -e CA_JWT_SECRET=xxx -e CA_ENCRYPTION_KEY=yyy codeagent:1.0.0
# =============================================================================

# ---------- 阶段 1：前端构建 ----------
FROM node:20-alpine AS web
WORKDIR /web
RUN corepack enable
# packageManager 字段已锁定 pnpm 10.33.2：pnpm 11+ 不再读取 package.json 的 pnpm 字段，
# 锁版本可避免 CI/镜像 silently 升级到 pnpm 12 后构建脚本策略再次漂移。
COPY frontend/package.json frontend/pnpm-lock.yaml* ./
# package.json 的 pnpm.onlyBuiltDependencies 已放行 esbuild / vue-demi 的构建脚本，
# 用于满足 pnpm 10 起的构建脚本审批机制，替代无法在镜像构建中使用的交互式 pnpm approve-builds。
RUN pnpm install --frozen-lockfile=false
COPY frontend/ ./
RUN pnpm run build

# ---------- 阶段 2：控制台镜像（Nginx 托管前端产物）----------
# 复用阶段 1 的 web，前端只构建一次；dist 直接打进镜像，
# 不再依赖宿主机 ./frontend/dist —— 该目录不存在时 Docker 会静默挂载空目录，
# nginx 会以 "index.html not found" 返回 403，排查成本很高。
FROM nginx:1.27-alpine AS webconsole
COPY deploy/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=web /web/dist /usr/share/nginx/html
EXPOSE 80
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1/ >/dev/null || exit 1

# ---------- 阶段 3：后端构建 ----------
FROM golang:1.23-alpine AS server
# Go 模块代理：golang.org/x/crypto 等依赖走 proxy.golang.org，国内构建机常因超时导致
# `go mod download` 以 exit 1 失败；默认给出可用代理并保留官方源兜底。
# 海外环境可用 --build-arg GOPROXY=https://proxy.golang.org,direct 覆盖。
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/codeagent-server ./cmd/server

# ---------- 阶段 4：运行镜像（默认 target）----------
# 注意：原 alpine:3.20 已于 2026-05 EOL，其仓库索引随时可能从 CDN 下线导致 `apk add`
# 报 exit code 4。与阶段 3（golang:1.23-alpine）走同一个 CDN，只要阶段 3 正常、本阶段
# 失败，即可判定是基础镜像版本 EOL 而非网络问题。
# 固定仍在维护期的版本，且不写 latest，避免将来再次漂移。
FROM alpine:3.22
# 拆成多行：一旦某条失败可直接从构建日志定位是 apk 还是用户创建出错
RUN apk add --no-cache git ca-certificates tzdata curl \
    && addgroup -S codeagent \
    && adduser -S -G codeagent codeagent \
    && mkdir -p /app/data \
    && chown -R codeagent:codeagent /app

WORKDIR /app
COPY --from=server /out/codeagent-server /app/codeagent-server
COPY --from=web /web/dist /app/web
COPY deploy/config.example.json /app/config.json

USER codeagent
ENV CA_ADDR=":8080" \
    CA_STORE_DRIVER="file" \
    CA_STORE_FILE="/app/data/state.json" \
    CA_CACHE_DIR="/app/data/cache" \
    CA_WORKSPACE_DIR="/app/data/workspace" \
    CA_LOG_FORMAT="json"

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/codeagent-server"]
CMD ["--config", "/app/config.json", "--web", "/app/web"]
