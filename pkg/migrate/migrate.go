// Package migrate 按文件名顺序应用 SQL 迁移，并以 schema_migrations
// 记录已执行文件，保证重复运行幂等。供 cmd/migrate 与集成测试共用。
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Apply 依次执行 dir 下尚未记录的 *.sql。已应用的文件跳过。
// 返回值是本次实际应用的文件名列表（已排序）。
func Apply(ctx context.Context, db *sql.DB, dir string) ([]string, error) {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   VARCHAR(255) NOT NULL,
			applied_at DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			PRIMARY KEY (filename)
		) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	files, err := ListSQL(dir)
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}

	var applied []string
	for _, path := range files {
		name := filepath.Base(path)
		var exists int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE filename = ?`, name).Scan(&exists); err != nil {
			return applied, fmt.Errorf("check %s: %w", name, err)
		}
		if exists > 0 {
			continue
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return applied, fmt.Errorf("read %s: %w", name, err)
		}
		for i, stmt := range SplitStatements(string(raw)) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return applied, fmt.Errorf("apply %s statement %d: %w", name, i+1, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (filename) VALUES (?)`, name); err != nil {
			return applied, fmt.Errorf("record %s: %w", name, err)
		}
		applied = append(applied, name)
	}
	return applied, nil
}

// ListSQL 返回目录下全部 .sql 文件，按文件名排序。
func ListSQL(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".sql") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// SplitStatements 把 SQL 文件拆成单条语句：跳过行注释，忽略引号内的分号。
// 本项目的迁移只含 DDL，不依赖存储过程分隔符。
func SplitStatements(sqlText string) []string {
	var (
		out     []string
		buf     strings.Builder
		inQuote rune
		escaped bool
	)
	flush := func() {
		stmt := strings.TrimSpace(buf.String())
		if stmt != "" {
			out = append(out, stmt)
		}
		buf.Reset()
	}

	lines := strings.Split(sqlText, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inQuote == 0 && strings.HasPrefix(trimmed, "--") {
			continue
		}
		for _, r := range line {
			if escaped {
				buf.WriteRune(r)
				escaped = false
				continue
			}
			if inQuote == 0 {
				switch r {
				case '\'', '"', '`':
					inQuote = r
					buf.WriteRune(r)
					continue
				case '\\':
					escaped = true
					buf.WriteRune(r)
					continue
				case ';':
					flush()
					continue
				}
				buf.WriteRune(r)
				continue
			}
			if r == '\\' {
				escaped = true
			} else if r == inQuote {
				inQuote = 0
			}
			buf.WriteRune(r)
		}
		buf.WriteRune('\n')
	}
	flush()
	return out
}
