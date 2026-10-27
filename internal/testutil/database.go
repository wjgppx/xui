// Package testutil provisions isolated schemas in an explicitly supplied test database.
package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wjgppx/xui/internal/store"
)

func Database(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL is required; run scripts/test.sh (integration tests are not silently skipped)")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + store.ID()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	s := &store.Store{DB: db}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}
func User(t *testing.T, s *store.Store) string {
	t.Helper()
	id := store.ID()
	if _, err := s.DB.Exec(context.Background(), `INSERT INTO users(id,email,password_hash) VALUES($1,$2,'unused')`, id, id+"@example.test"); err != nil {
		t.Fatal(err)
	}
	return id
}
