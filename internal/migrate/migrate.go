package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
)

//go:embed sql
var fs embed.FS

// Dialects 支持的后端方言
var Dialects = []string{"postgres", "sqlite"}

// Apply 应用指定方言的迁移（幂等：schema_migrations 记录已应用版本）
func Apply(ctx context.Context, db *sql.DB, dialect string) error {
	if !contains(Dialects, dialect) {
		return fmt.Errorf("unknown dialect %q", dialect)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	var applied bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = 1)`).Scan(&applied); err != nil {
		return err
	}
	if applied {
		return nil
	}

	file := fmt.Sprintf("sql/%s/001_init.sql", dialect)
	b, err := fs.ReadFile(file)
	if err != nil {
		return fmt.Errorf("migration %s: %w", file, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, s := range splitStatements(string(b)) {
		if strings.TrimSpace(s) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, s); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration stmt failed: %v\n%s", err, s)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (1)`); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func splitStatements(sql string) []string {
	return strings.Split(sql, ";")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
