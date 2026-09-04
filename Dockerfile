# Hify 生产镜像：前端构建 -> Go 构建（把 dist 用 go:embed 收进二进制）->
# 运行时只留一个静态链接的可执行文件。最终镜像里没有 node、没有 Go 工具链、
# 没有源码，就是 CLAUDE.md 说的「单个 Go 二进制」。

# ---------- 1. 前端 ----------
FROM node:22-alpine AS web
WORKDIR /src/web
# 先只拷 lockfile 装依赖，让改业务代码不会让 npm ci 这一层缓存失效。
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---------- 2. 后端 ----------
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 前端产物必须落在 web/dist，这是 web/embed.go 里 go:embed 的路径。
COPY --from=web /src/web/dist ./web/dist
# CGO_ENABLED=0：go-sql-driver/mysql 和 pgx 都是纯 Go 实现，关掉 cgo 才能得到
# 不依赖 glibc 的静态二进制，运行阶段才敢用 alpine。
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hify ./cmd/hify

# ---------- 3. 运行时 ----------
FROM alpine:3.20
# ca-certificates：调用外部模型供应商（OpenAI 等）走 HTTPS 要用；
# tzdata：项目统一按 UTC 存储，但日志和 time.Local 解析需要时区库兜底。
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 hify
WORKDIR /app
COPY --from=build /out/hify /usr/local/bin/hify
# 知识库上传的原始文件落本地磁盘（HIFY_KNOWLEDGE_STORAGE_DIR），必须挂 volume，
# 否则容器一重建文档源文件就没了——这是当前单实例部署的已知限制。
RUN mkdir -p /app/data/knowledge && chown -R hify:hify /app/data
USER hify
ENV HIFY_HTTP_ADDR=":8080" \
    HIFY_KNOWLEDGE_STORAGE_DIR="/app/data/knowledge"
EXPOSE 8080
ENTRYPOINT ["hify"]
CMD ["serve"]
