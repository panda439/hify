// Command evalrunner replays eval/testset.yaml through conversation.Service,
// has a judge model score each reply, and compares against a baseline —
// the eval harness half of Hify's observability work (see
// internal/eval's package doc). It shares config.Load with cmd/hify so it
// reads the same HIFY_* env vars, but wires up only the layers
// conversation.Service actually needs, with no HTTP server and no asynq
// worker started.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"hify/internal/agent"
	"hify/internal/config"
	"hify/internal/conversation"
	"hify/internal/eval"
	"hify/internal/knowledge"
	"hify/internal/mcp"
	"hify/internal/platform"
	"hify/internal/platform/trace"
	"hify/internal/provider"
)

func main() {
	testsetPath := flag.String("testset", "eval/testset.yaml", "test set YAML file")
	judgeModelID := flag.String("judge-model-id", "", "model ID (already registered in Hify) to use as the judge")
	userID := flag.String("user-id", "", "existing user ID that owns the eval conversations")
	baselinePath := flag.String("baseline", "eval/baseline.json", "baseline report to compare against (skipped if missing)")
	outPath := flag.String("out", "", "where to write this run's report (default: eval/runs/<timestamp>.json)")
	regressionThreshold := flag.Int("regression-threshold", 1, "exit non-zero if any case's score drops by at least this many points vs baseline")
	flag.Parse()

	if *judgeModelID == "" || *userID == "" {
		fmt.Fprintln(os.Stderr, "evalrunner: --judge-model-id and --user-id are required")
		os.Exit(2)
	}
	if *outPath == "" {
		*outPath = fmt.Sprintf("eval/runs/%s.json", time.Now().UTC().Format("20060102-150405"))
	}

	if err := run(*testsetPath, *judgeModelID, *userID, *baselinePath, *outPath, *regressionThreshold); err != nil {
		fmt.Fprintln(os.Stderr, "evalrunner:", err)
		os.Exit(1)
	}
}

func run(testsetPath, judgeModelID, userID, baselinePath, outPath string, regressionThreshold int) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	testsetBytes, err := os.ReadFile(testsetPath)
	if err != nil {
		return fmt.Errorf("read test set: %w", err)
	}
	var testSet eval.TestSet
	if err := yaml.Unmarshal(testsetBytes, &testSet); err != nil {
		return fmt.Errorf("parse test set: %w", err)
	}
	if len(testSet.Cases) == 0 {
		return fmt.Errorf("test set has no cases")
	}

	db, err := platform.NewMySQLPool(cfg.MySQLDSN)
	if err != nil {
		return fmt.Errorf("connect mysql: %w", err)
	}
	defer db.Close()

	pgdb, err := platform.NewPostgresPool(cfg.PostgresDSN)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pgdb.Close()

	redisCfg := platform.RedisConfig{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB}
	rdb := platform.NewRedisClient(redisCfg)
	defer rdb.Close()
	asynqClient := platform.NewAsynqClient(redisCfg)
	defer asynqClient.Close()

	providerSvc, err := provider.NewService(provider.NewRepository(db), cfg.EncryptionKey, rdb)
	if err != nil {
		return fmt.Errorf("build provider service: %w", err)
	}
	mcpSvc := mcp.NewService(mcp.NewRepository(db))
	knowledgeSvc := knowledge.NewService(knowledge.NewRepository(db, pgdb), providerSvc, asynqClient, cfg.KnowledgeStorageDir, cfg.RAGRerankEnabled, cfg.RAGRerankModelID, cfg.RAGRerankTimeout, cfg.RAGMetadataFilterEnabled)
	agentSvc := agent.NewService(agent.NewRepository(db), providerSvc, knowledgeSvc, mcpSvc)
	traceStore := trace.NewStore(db)
	conversationSvc := conversation.NewService(conversation.NewRepository(db), agentSvc, providerSvc, knowledgeSvc, mcpSvc, traceStore, cfg.RAGQueryRewriteEnabled, cfg.RAGQueryRewriteModelID, cfg.RAGQueryRewriteTimeout)

	judgeModel, err := providerSvc.GetModel(context.Background(), judgeModelID)
	if err != nil {
		return fmt.Errorf("resolve judge model: %w", err)
	}
	judgeClient, err := providerSvc.ResolveClient(context.Background(), judgeModel.ProviderID)
	if err != nil {
		return fmt.Errorf("resolve judge client: %w", err)
	}

	report := eval.Run(context.Background(), conversationSvc, traceStore, judgeClient, judgeModel.ModelName, userID, testSet.Cases)
	report.RanAt = time.Now().UTC().Format(time.RFC3339)

	failed := 0
	for _, r := range report.Results {
		if r.Err != "" {
			failed++
			fmt.Fprintf(os.Stderr, "case %q failed: %s\n", r.Name, r.Err)
		} else {
			fmt.Printf("case %q: score=%d  %s\n", r.Name, r.Score, r.Reasoning)
		}
	}

	if err := eval.Save(report, outPath); err != nil {
		return fmt.Errorf("save report: %w", err)
	}
	fmt.Printf("\n结果已写入 %s\n", outPath)

	if _, statErr := os.Stat(baselinePath); statErr == nil {
		baseline, err := eval.Load(baselinePath)
		if err != nil {
			return fmt.Errorf("load baseline: %w", err)
		}
		fmt.Println()
		fmt.Println(eval.Compare(report, baseline))
		if eval.Regressed(report, baseline, regressionThreshold) {
			return fmt.Errorf("regression detected (>= %d point drop on at least one case)", regressionThreshold)
		}
	} else {
		fmt.Printf("未找到基线 %s，跳过回归对比（可以把本次结果复制过去作为下次的基线）\n", baselinePath)
	}

	if failed > 0 {
		return fmt.Errorf("%d/%d cases failed to run", failed, len(report.Results))
	}
	return nil
}
