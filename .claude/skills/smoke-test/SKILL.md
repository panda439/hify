---
name: smoke-test
description: 把 Hify 后端+MySQL+Redis 拉起来，跑一遍标准冒烟检查（健康检查、迁移、admin 登录、当前已实现阶段的核心接口），跑完汇报通过/失败并清理掉自己建的测试数据和进程。用户说"跑一遍验证"、"测一下现在能不能用"、每个 Phase 做完想确认没崩的时候用。
disable-model-invocation: false
user-invocable: true
allowed-tools: Bash, Read
---

# Hify 冒烟测试

这是每个 Phase 收尾、或怀疑哪里坏了的时候，用来快速确认"整个栈还能正常跑"的标准流程。不是单元测试，是走一遍真实的 HTTP 请求。

## 步骤

### 1. 基础设施

```bash
docker compose -f /Users/lishurong/go/src/hify/docker-compose.yml ps
```
如果 `hify-mysql-1`/`hify-postgres-1`/`hify-redis-1` 不是 `healthy`，执行 `docker compose up -d` 并等到健康检查通过再继续。（postgres 是 pgvector 容器，存 chunks 向量数据，`/ready` 现在会一并 ping 它。）

### 2. 构建 + 迁移

```bash
cd /Users/lishurong/go/src/hify
unset GOROOT   # 这台机器 ~/.bash_profile 曾经有个坏的 GOROOT，新终端应该已经不需要这行了，保留是为了保险
go build -o bin/hify ./cmd/hify
set -a; source .env; set +a
go run ./cmd/hify migrate up   # 幂等，已经是最新版本时会打印 no change 之类，不是错误
```

### 3. 启动服务（后台）

```bash
lsof -ti :8080 2>/dev/null | xargs -r kill -9 2>/dev/null   # 先确保端口没有残留进程
set -a; source .env; set +a
nohup ./bin/hify serve > /tmp/hify_smoketest.log 2>&1 &
echo $! > /tmp/hify_smoketest.pid
sleep 2
cat /tmp/hify_smoketest.log   # 确认没有 ERROR，看到 "hify starting" 字样
```

### 4. 核心检查（按顺序执行，任何一步非预期结果就停下汇报，不要往下走）

```bash
# 4.1 健康检查
curl -sS -o /dev/null -w "health: %{http_code}\n" http://127.0.0.1:8080/api/v1/health
curl -sS -o /dev/null -w "ready: %{http_code}\n" http://127.0.0.1:8080/api/v1/ready
# 期望：都是 200。ready 是 200 说明 MySQL/Redis 真的连得上，不只是进程活着。

# 4.2 无 token 应该被拒绝
curl -sS -o /dev/null -w "no-token users/me: %{http_code}\n" http://127.0.0.1:8080/api/v1/users/me
# 期望：401

# 4.3 admin 登录（账号密码来自 .env 的 ADMIN_EMAIL/ADMIN_PASSWORD）
TOKEN=$(curl -sS -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | jq -r '.access_token')
echo "token acquired: ${TOKEN:0:20}..."
# 期望：拿到一个非 null 的 JWT。如果是 null，很可能是密码不对或者触发了登录限流（见下面"常见问题"）。

# 4.4 带 token 应该成功
curl -sS -o /dev/null -w "users/me: %{http_code}\n" http://127.0.0.1:8080/api/v1/users/me -H "Authorization: Bearer $TOKEN"
# 期望：200

# 4.5 Phase 1：供应商管理（如果这个阶段已经实现）
curl -sS -o /dev/null -w "providers list: %{http_code}\n" "http://127.0.0.1:8080/api/v1/providers?limit=10" -H "Authorization: Bearer $TOKEN"
# 期望：200
```

**后续 Phase 上线后，在这里追加对应的核心接口检查**（比如 Phase 2 加 `POST /conversations`、Phase 3 加知识库上传状态查询等），保持这个清单和实际已实现的功能同步。

### 5. 清理

```bash
kill -TERM "$(cat /tmp/hify_smoketest.pid)" 2>/dev/null
sleep 1
lsof -ti :8080 2>/dev/null | xargs -r kill -9 2>/dev/null
rm -f /tmp/hify_smoketest.log /tmp/hify_smoketest.pid
```

如果第 4 步过程中创建了测试数据（比如临时供应商），也要在这里用 `docker exec hify-mysql-1 mysql -uroot -phify_root_dev hify -e "..."` 删掉，不要把测试数据留在开发库里。

## 常见问题

- **登录 429**：这个会话反复测登录会触发限流（15分钟10次），是 auth 模块限流生效了，不是 bug。可以 `docker exec hify-redis-1 redis-cli DEL "login:127.0.0.1:$ADMIN_EMAIL"` 清掉这个 key 再重试。
- **8080 端口占用**：多半是上一次测试的进程没清理干净，`lsof -ti :8080 | xargs kill -9` 强制清一下。
- **`go build` 报 GOROOT 相关错误**：说明当前 shell 还是旧的，执行 `unset GOROOT` 再重试。

## 汇报格式

跑完之后用一句话总结：全部通过 / 哪一步失败了以及具体的错误信息，不需要把每条 curl 的完整输出都贴给用户，除非有失败项。
