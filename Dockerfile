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
COPY frontend/package.json frontend/pnpm-lock.yaml* ./
RUN pnpm install --frozen-lockfile=false
COPY frontend/ ./
RUN pnpm run build

# ---------- 阶段 2：后端构建 ----------
FROM golang:1.23-alpine AS server
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/codeagent-server ./cmd/server

# ---------- 阶段 3：运行镜像 ----------
FROM alpine:3.20
RUN apk add --no-cache git ca-certificates tzdata curl && \
    addgroup -S codeagent && adduser -S -G codeagent codeagent && \
    mkdir -p /app/data && chown -R codeagent:codeagent /app

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
