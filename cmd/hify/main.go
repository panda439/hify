// Command hify is the single entrypoint for the Hify binary: `serve` runs
// the API server, `migrate up`/`migrate down` applies/reverts schema
// migrations (MySQL and the pgvector PostgreSQL, each with its own
// independent schema_migrations), `seed-admin` creates the first admin user
// from ADMIN_EMAIL/ADMIN_PASSWORD, `backfill-chunks` copies pre-migration
// chunks from MySQL into PostgreSQL (run it BEFORE `migrate up` drops the
// MySQL chunks table — see backfill.go).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-migrate/migrate/v4"
	mysqlmigrate "github.com/golang-migrate/migrate/v4/database/mysql"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/hibiken/asynq"

	"hify/internal/agent"
	"hify/internal/auth"
	"hify/internal/config"
	"hify/internal/conversation"
	hifydb "hify/internal/db"
	"hify/internal/knowledge"
	"hify/internal/mcp"
	"hify/internal/platform"
	"hify/internal/provider"
	"hify/internal/server"
	"hify/internal/user"
	"hify/internal/workflow"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hify: config error:", err)
		os.Exit(1)
	}
	logger := platform.NewLogger(cfg.Env)

	switch cmd {
	case "serve":
		if err := runServe(cfg, logger); err != nil {
			logger.Error("hify exited with error", "err", err)
			os.Exit(1)
		}
	case "migrate":
		direction := "up"
		if len(os.Args) > 2 {
			direction = os.Args[2]
		}
		if err := runMigrate(cfg, logger, direction); err != nil {
			logger.Error("migrate failed", "err", err)
			os.Exit(1)
		}
	case "seed-admin":
		if err := runSeedAdmin(cfg, logger); err != nil {
			logger.Error("seed-admin failed", "err", err)
			os.Exit(1)
		}
	case "backfill-chunks":
		if err := runBackfillChunks(cfg, logger); err != nil {
			logger.Error("backfill-chunks failed", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "hify: unknown command %q (expected: serve, migrate, seed-admin, backfill-chunks)\n", cmd)
		os.Exit(1)
	}
}

// buildApp wires every module (repository -> service -> handler, in layer
// order) onto the shared engine, per CLAUDE.md's DI convention. The
// returned asynq.Server is configured with the document-processing task
// handler but not yet started — runServe starts/stops it alongside the
// HTTP server. The returned cleanup func closes the DB pool, Redis client,
// and asynq client; callers must defer it.
func buildApp(cfg config.Config, logger *slog.Logger) (*gin.Engine, *asynq.Server, func(), error) {
	db, err := platform.NewMySQLPool(cfg.MySQLDSN)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect mysql: %w", err)
	}

	pgdb, err := platform.NewPostgresPool(cfg.PostgresDSN)
	if err != nil {
		db.Close()
		return nil, nil, nil, fmt.Errorf("connect postgres: %w", err)
	}

	rdb := platform.NewRedisClient(platform.RedisConfig{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})

	redisCfg := platform.RedisConfig{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB}
	asynqClient := platform.NewAsynqClient(redisCfg)

	cleanup := func() {
		db.Close()
		pgdb.Close()
		rdb.Close()
		asynqClient.Close()
	}

	router, v1 := server.New(server.Config{
		Logger:         logger,
		AllowedOrigins: cfg.CORSAllowedOrigins,
		DB:             db,
		PG:             pgdb,
		Redis:          rdb,
	})

	// Layer 0
	userRepo := user.NewRepository(db)
	userSvc := user.NewService(userRepo)
	userHandler := user.NewHandler(userSvc)
	user.RegisterRoutes(v1, userHandler, cfg.JWTSecret)

	// Layer 1
	authRepo := auth.NewRepository(db)
	authSvc := auth.NewService(authRepo, userSvc, rdb, cfg.JWTSecret, cfg.JWTAccessTTL, cfg.JWTRefreshTTL)
	authHandler := auth.NewHandler(authSvc, cfg.Env)
	auth.RegisterRoutes(v1, authHandler)

	providerRepo := provider.NewRepository(db)
	providerSvc, err := provider.NewService(providerRepo, cfg.EncryptionKey, rdb)
	if err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("build provider service: %w", err)
	}
	providerHandler := provider.NewHandler(providerSvc)
	provider.RegisterRoutes(v1, providerHandler, cfg.JWTSecret)

	mcpRepo := mcp.NewRepository(db)
	mcpSvc := mcp.NewService(mcpRepo)
	mcpHandler := mcp.NewHandler(mcpSvc)
	mcp.RegisterRoutes(v1, mcpHandler, cfg.JWTSecret)

	// Layer 2
	knowledgeRepo := knowledge.NewRepository(db, pgdb)
	knowledgeSvc := knowledge.NewService(knowledgeRepo, providerSvc, asynqClient, cfg.KnowledgeStorageDir)
	knowledgeHandler := knowledge.NewHandler(knowledgeSvc)
	knowledge.RegisterRoutes(v1, knowledgeHandler, cfg.JWTSecret)

	// Layer 3
	agentRepo := agent.NewRepository(db)
	agentSvc := agent.NewService(agentRepo, providerSvc, knowledgeSvc, mcpSvc)
	agentHandler := agent.NewHandler(agentSvc)
	agent.RegisterRoutes(v1, agentHandler, cfg.JWTSecret)

	// Layer 4
	conversationRepo := conversation.NewRepository(db)
	conversationSvc := conversation.NewService(conversationRepo, agentSvc, providerSvc, knowledgeSvc, mcpSvc)
	conversationHandler := conversation.NewHandler(conversationSvc)
	conversation.RegisterRoutes(v1, conversationHandler, cfg.JWTSecret)

	// Layer 5
	workflowRepo := workflow.NewRepository(db)
	workflowSvc := workflow.NewService(workflowRepo, providerSvc, knowledgeSvc, mcpSvc)
	workflowHandler := workflow.NewHandler(workflowSvc)
	workflow.RegisterRoutes(v1, workflowHandler, cfg.JWTSecret)

	// The worker runs in this same process (per the plan's deployment
	// architecture: "Hify 二进制...Gin API + asynq worker...同一个进程").
	// Start is non-blocking; runServe owns calling Shutdown() during
	// graceful shutdown, which is what actually waits for in-flight tasks
	// to finish — asynq.Server's own lifecycle satisfies CLAUDE.md's "谁负责
	// 等待 goroutine 结束" rule, no separate WaitGroup needed.
	mux := asynq.NewServeMux()
	mux.Handle(knowledge.TaskTypeProcessDocument, knowledge.NewTaskHandler(knowledgeSvc))
	asynqServer := platform.NewAsynqServer(redisCfg, cfg.AsynqConcurrency)
	if err := asynqServer.Start(mux); err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("start asynq worker: %w", err)
	}

	return router, asynqServer, cleanup, nil
}

func runServe(cfg config.Config, logger *slog.Logger) error {
	router, asynqServer, cleanup, err := buildApp(cfg, logger)
	if err != nil {
		return err
	}
	defer cleanup()
	defer asynqServer.Shutdown()

	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: router,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("hify starting", "addr", cfg.HTTPAddr, "env", cfg.Env)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case sig := <-stop:
		logger.Info("hify shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		logger.Info("hify stopped")
		return nil
	}
}

// runMigrate applies the direction to both databases: MySQL first (business
// tables), then the pgvector PostgreSQL (chunks). Each database keeps its
// own independent schema_migrations table — the two version numbers are
// never comparable, and `down` steps each database back by one migration
// of its own.
func runMigrate(cfg config.Config, logger *slog.Logger, direction string) error {
	if direction != "up" && direction != "down" {
		return fmt.Errorf("unknown migrate direction %q (expected: up, down)", direction)
	}
	if err := migrateMySQL(cfg, direction); err != nil {
		return err
	}
	logger.Info("migrate complete", "db", "mysql", "direction", direction)
	if err := migratePostgres(cfg, direction); err != nil {
		return err
	}
	logger.Info("migrate complete", "db", "postgres", "direction", direction)
	return nil
}

func migrateMySQL(cfg config.Config, direction string) error {
	source, err := iofs.New(hifydb.MigrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("load mysql migration source: %w", err)
	}

	// multiStatements=true is only added for this dedicated migration
	// connection, never the app's regular pool (platform.NewMySQLPool as
	// used by runServe) — a single migration file legitimately contains
	// several CREATE TABLE statements, but letting arbitrary application
	// queries execute multiple statements per round-trip is an
	// unnecessary SQL-injection blast-radius increase we don't need
	// since all app queries go through sqlc's parameterized statements.
	migrateDSN := cfg.MySQLDSN + "&multiStatements=true"
	driver, err := mysqlmigrate.WithInstance(mustOpenDB(migrateDSN), &mysqlmigrate.Config{})
	if err != nil {
		return fmt.Errorf("init mysql migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "mysql", driver)
	if err != nil {
		return fmt.Errorf("init mysql migrate: %w", err)
	}
	return runMigrateDirection(m, direction, "mysql")
}

// migratePostgres is also called directly by runBackfillChunks so the PG
// chunks table is guaranteed to exist before any backfill insert — that's
// what makes "backfill before `migrate up`" a safe order to require.
// PostgreSQL runs multi-statement files natively, no multiStatements
// analogue needed.
func migratePostgres(cfg config.Config, direction string) error {
	source, err := iofs.New(hifydb.PGMigrationsFS, "pgmigrations")
	if err != nil {
		return fmt.Errorf("load postgres migration source: %w", err)
	}

	pgdb, err := platform.NewPostgresPool(cfg.PostgresDSN)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}

	driver, err := pgxmigrate.WithInstance(pgdb, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("init postgres migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("init postgres migrate: %w", err)
	}
	return runMigrateDirection(m, direction, "postgres")
}

func runMigrateDirection(m *migrate.Migrate, direction, dbName string) error {
	var err error
	switch direction {
	case "up":
		err = m.Up()
	case "down":
		err = m.Steps(-1)
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("run %s migration (%s): %w", dbName, direction, err)
	}
	return nil
}

// mustOpenDB panics on failure: this only runs inside the `migrate`
// subcommand's short-lived process, where there's no request in flight to
// degrade gracefully for — a connection failure here is fatal to the whole
// invocation either way.
func mustOpenDB(dsn string) *sql.DB {
	db, err := platform.NewMySQLPool(dsn)
	if err != nil {
		panic(fmt.Errorf("hify migrate: %w", err))
	}
	return db
}

// runSeedAdmin is idempotent: re-running it after the admin already exists
// logs and returns cleanly instead of erroring, since that's the common
// case (deploy scripts calling it unconditionally on every start).
func runSeedAdmin(cfg config.Config, logger *slog.Logger) error {
	if cfg.AdminEmail == "" || cfg.AdminPassword == "" {
		return fmt.Errorf("seed-admin: ADMIN_EMAIL and ADMIN_PASSWORD must both be set")
	}

	db, err := platform.NewMySQLPool(cfg.MySQLDSN)
	if err != nil {
		return fmt.Errorf("connect mysql: %w", err)
	}
	defer db.Close()

	userSvc := user.NewService(user.NewRepository(db))

	_, err = userSvc.Create(context.Background(), cfg.AdminEmail, "Admin", cfg.AdminPassword, user.RoleAdmin)
	if err != nil {
		if errors.Is(err, user.ErrEmailTaken) {
			logger.Info("seed-admin: admin already exists, nothing to do", "email", cfg.AdminEmail)
			return nil
		}
		return fmt.Errorf("seed-admin: create admin: %w", err)
	}

	logger.Info("seed-admin: admin created", "email", cfg.AdminEmail)
	return nil
}
