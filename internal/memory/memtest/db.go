// Package memtest 为记忆子系统的测试提供隔离的 PostgreSQL schema。
package memtest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// dsnEnv 是测试库连接串的环境变量；它是测试基础设施，不属于运行期配置。
const dsnEnv = "PERSONA_MEMORY_TEST_DSN"

// setupTimeout 是建、删 schema 的超时。
const setupTimeout = 10 * time.Second

// NewDB 为当前测试建一个独立 schema，返回 search_path 指向它的 DSN；测试结束时删除该 schema。
// 未设置 PERSONA_MEMORY_TEST_DSN 时跳过测试。
func NewDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv(dsnEnv)
	if base == "" {
		t.Skip(dsnEnv + " 未设置，跳过数据库测试")
	}

	db, err := gorm.Open(postgres.Open(base), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("memtest: 连接测试库: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("memtest: 取连接池: %v", err)
	}

	schema := fmt.Sprintf("memtest_%d_%d", time.Now().UnixNano(), rand.Uint32())
	ctx, cancel := context.WithTimeout(t.Context(), setupTimeout)
	defer cancel()
	if err := db.WithContext(ctx).Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		_ = sqlDB.Close()
		t.Fatalf("memtest: 建 schema: %v", err)
	}

	t.Cleanup(func() {
		// t.Context() 在 Cleanup 前已取消，去掉取消信号后另设超时。
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), setupTimeout)
		defer cancel()
		if err := db.WithContext(ctx).Exec(`DROP SCHEMA ` + schema + ` CASCADE`).Error; err != nil {
			t.Errorf("memtest: 删 schema %s: %v", schema, err)
		}
		_ = sqlDB.Close()
	})

	dsn, err := withSearchPath(base, schema)
	if err != nil {
		t.Fatalf("memtest: %v", err)
	}
	return dsn
}

// Open 打开 dsn 指向的库，测试结束时关闭连接池；常与 NewDB 连用：Open(t, NewDB(t))。
func Open(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("memtest: 连接测试库: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("memtest: 取连接池: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// withSearchPath 给 URL 或 key=value 形式的 DSN 加上 search_path。
func withSearchPath(dsn, schema string) (string, error) {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn + " search_path=" + schema, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("解析 DSN: %w", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
