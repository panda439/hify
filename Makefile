-include .env
export

.PHONY: dev build test test-race migrate-up migrate-down sqlc check-deps db-up db-down web-dev web-build eval eval-retrieval-gate

dev:
	air

# 集成测试依赖 docker-compose 容器（make db-up）；容器没起时自动 skip，
# 只跑纯逻辑单测。
test:
	go vet ./internal/...
	go test ./internal/...

test-race:
	go vet ./internal/...
	go test -race -count=1 ./internal/...

build: web-build
	go build -o bin/hify ./cmd/hify

migrate-up:
	go run ./cmd/hify migrate up

migrate-down:
	go run ./cmd/hify migrate down

sqlc:
	cd internal/db && sqlc generate

check-deps:
	./scripts/check-deps.sh

db-up:
	docker compose up -d

db-down:
	docker compose down

web-dev:
	cd web && npm run dev

web-build:
	cd web && npm run build

# 跑一遍 eval/testset.yaml，和 eval/baseline.json 做回归对比。需要
# JUDGE_MODEL_ID/EVAL_USER_ID 两个环境变量（裁判模型和跑测试用的现有用户）。
eval:
	go run ./cmd/evalrunner --testset eval/testset.yaml --judge-model-id $(JUDGE_MODEL_ID) --user-id $(EVAL_USER_ID) --baseline eval/baseline.json

# Phase 6：确定性检索回归门禁。真实 MySQL+PostgreSQL/pgvector/pg_trgm +
# fake embedding，走公开 knowledge.Service.Retrieve，不依赖 LLM/Judge，也
# 不需要 JUDGE_MODEL_ID/EVAL_USER_ID。容器没起时和其它集成测试一样打印原因后
# SKIP（testutil 既定约定），不是静默跳过。人类可读报告写到仓库根目录的
# eval/runs/（用绝对路径传给测试，因为 go test 的工作目录是包目录
# internal/knowledge，不是仓库根——普通 `go test ./...` 不设这个变量，默认
# 写进 t.TempDir()，绝不弄脏源码树）。见 docs/eval-phase6-retrieval-gate-report.md。
eval-retrieval-gate:
	HIFY_RETRIEVAL_GATE_REPORT_PATH=$(CURDIR)/eval/runs/phase6-retrieval-gate-latest.json \
		go test -v -race -count=1 -run TestRetrievalGatePhase6 ./internal/knowledge/
