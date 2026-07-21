// Package testutil provides the shared integration-test database bootstrap:
// per-package throwaway MySQL / PostgreSQL databases (hify_test_<name>) on
// the docker-compose containers, migrated to head, recreated fresh on every
// test run. Only ever imported from _test.go files.
//
// 每个包用独立的 hify_test_<name> 库——`go test ./...` 会并行跑多个包的测试
// 二进制，共用一个测试库会互相 DROP。绝不触碰开发库 hify。
package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	mysqlmigrate "github.com/golang-migrate/migrate/v4/database/mysql"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	hifydb "hify/internal/db"
	"hify/internal/platform"
)

// DSN 常量和 docker-compose.yml / .env.example 保持一致（3307/5433 是这台
// 机器的端口避让约定；CI 里把容器映射到相同端口即可复用）。
const (
	mysqlAdminDSN  = "root:hify_root_dev@tcp(127.0.0.1:3307)/?parseTime=true"
	mysqlDSNFormat = "root:hify_root_dev@tcp(127.0.0.1:3307)/hify_test_%s?parseTime=true&loc=UTC&charset=utf8mb4"
	pgAdminDSN     = "postgres://hify:hify_dev@127.0.0.1:5433/hify?sslmode=disable"
	pgDSNFormat    = "postgres://hify:hify_dev@127.0.0.1:5433/hify_test_%s?sslmode=disable"
)

var errDBUnavailable = errors.New("integration databases unreachable")

type dbResult struct {
	db  *sql.DB
	err error
}

var (
	mu    sync.Mutex
	cache = map[string]dbResult{}
)

// MySQL returns an app-style pool onto a freshly recreated, fully migrated
// hify_test_<name> MySQL database. Skips the test when the docker-compose
// containers aren't running.
func MySQL(t *testing.T, name string) *sql.DB {
	t.Helper()
	return cached(t, "mysql:"+name, func() (*sql.DB, error) { return buildMySQL(name) })
}

// Postgres is MySQL's pgvector counterpart (chunks store).
func Postgres(t *testing.T, name string) *sql.DB {
	t.Helper()
	return cached(t, "pg:"+name, func() (*sql.DB, error) { return buildPostgres(name) })
}

func cached(t *testing.T, key string, build func() (*sql.DB, error)) *sql.DB {
	t.Helper()
	mu.Lock()
	res, ok := cache[key]
	if !ok {
		db, err := build()
		res = dbResult{db: db, err: err}
		cache[key] = res
	}
	mu.Unlock()

	if errors.Is(res.err, errDBUnavailable) {
		t.Skipf("跳过集成测试（先 make db-up 起容器）: %v", res.err)
	}
	if res.err != nil {
		t.Fatalf("testutil: %v", res.err)
	}
	return res.db
}

func buildMySQL(name string) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	admin, err := sql.Open("mysql", mysqlAdminDSN)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errDBUnavailable, err)
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("%w: mysql: %v", errDBUnavailable, err)
	}

	dbName := "hify_test_" + name
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
		return nil, fmt.Errorf("drop %s: %w", dbName, err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		return nil, fmt.Errorf("create %s: %w", dbName, err)
	}

	dsn := fmt.Sprintf(mysqlDSNFormat, name)
	if err := migrateMySQL(dsn); err != nil {
		return nil, err
	}
	// 应用连接池不带 multiStatements，和生产一致（见 cmd/hify/main.go 的
	// SQL 注入爆炸半径说明）。
	return platform.NewMySQLPool(dsn)
}

func migrateMySQL(dsn string) error {
	source, err := iofs.New(hifydb.MigrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("load mysql migrations: %w", err)
	}
	mdb, err := sql.Open("mysql", dsn+"&multiStatements=true")
	if err != nil {
		return fmt.Errorf("open mysql for migration: %w", err)
	}
	driver, err := mysqlmigrate.WithInstance(mdb, &mysqlmigrate.Config{})
	if err != nil {
		return fmt.Errorf("init mysql migrate driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "mysql", driver)
	if err != nil {
		return fmt.Errorf("init mysql migrate: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("mysql migrate up: %w", err)
	}
	return mdb.Close()
}

func buildPostgres(name string) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	admin, err := platform.NewPostgresPool(pgAdminDSN)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errDBUnavailable, err)
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("%w: postgres: %v", errDBUnavailable, err)
	}

	dbName := "hify_test_" + name
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)"); err != nil {
		return nil, fmt.Errorf("drop %s: %w", dbName, err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		return nil, fmt.Errorf("create %s: %w", dbName, err)
	}

	pgdb, err := platform.NewPostgresPool(fmt.Sprintf(pgDSNFormat, name))
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", dbName, err)
	}
	if err := migratePG(pgdb); err != nil {
		return nil, err
	}
	return pgdb, nil
}

func migratePG(pgdb *sql.DB) error {
	source, err := iofs.New(hifydb.PGMigrationsFS, "pgmigrations")
	if err != nil {
		return fmt.Errorf("load pg migrations: %w", err)
	}
	driver, err := pgxmigrate.WithInstance(pgdb, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("init pg migrate driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("init pg migrate: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("pg migrate up: %w", err)
	}
	return nil
}
