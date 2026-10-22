# syntax=docker/dockerfile:1

# ── 构建阶段 ──────────────────────────────────────────────
FROM golang:1.23-alpine AS build
WORKDIR /src

# 先复制依赖清单，利用层缓存：仅代码变更时不重复下载模块
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/agentrix ./cmd/agentrix

# ── 运行阶段 ──────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 app
WORKDIR /app

COPY --from=build /out/agentrix /app/agentrix

# 场景与提示词按部署环境挂载，例如：
#   -v $PWD/scenes:/app/scenes
ENV AGENTRIX_SCENES_DIR=/app/scenes

USER app
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/agentrix"]
