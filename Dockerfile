# syntax=docker/dockerfile:1

# ==================== 构建阶段 ====================
FROM golang:1.24-alpine AS builder

WORKDIR /build

# 先只拷贝依赖清单，充分利用 Docker 层缓存：go.mod/go.sum 不变时跳过依赖下载
COPY go.mod go.sum ./
RUN go mod download

# 再拷贝源码并构建静态二进制
# CGO_ENABLED=0：纯静态编译，运行阶段无需 glibc
# -trimpath -ldflags="-s -w"：去掉路径信息和调试符号，减小体积
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/glm2api ./cmd/server

# ==================== 运行阶段 ====================
FROM alpine:3.21

# ca-certificates：访问 https://chatglm.cn 上游必须
# tzdata：时区支持（容器内日志时间与本地一致）
# 创建非 root 用户运行服务
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 app \
    && adduser -D -u 10001 -G app app

WORKDIR /app
COPY --from=builder /out/glm2api /app/glm2api

ENV TZ=Asia/Shanghai

# 非 root 运行
# 注意：token.txt / .env 会在运行时从宿主机挂载（refresh token 会自动写回），
# Linux 宿主机上需保证该文件对 uid 10001 可写（chmod 666 或 chown 10001）；
# Docker Desktop（Windows/macOS）的挂载默认可写，无需处理
USER app

EXPOSE 8000

# 健康检查：复用程序自带的 /health 接口
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8000/health >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/glm2api"]
