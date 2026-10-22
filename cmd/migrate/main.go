package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daqiaoliang-coder/agentrix/pkg/session"
)

// migrate 按文件名顺序应用 migrations 目录下的 SQL，并以
// schema_migrations 记录已执行文件，重复运行是幂等的。
//
// 用法：
//
//	go run ./cmd/migrate -dsn "user:pass@tcp(127.0.0.1:3306)/agentrix?parseTime=true" -dir ./migrations
func main() {
	dsn := flag.String("dsn", os.Getenv("AGENTRIX_MYSQL_DSN"), "MySQL DSN（默认读 AGENTRIX_MYSQL_DSN）")
	dir := flag.String("dir", "./migrations", "SQL 迁移目录")
	flag.Parse()

	if strings.TrimSpace(*dsn) == "" {
		log.Fatal("dsn is required (flag -dsn or env AGENTRIX_MYSQL_DSN)")
	}

	db, err := session.OpenMySQLDB(*dsn)
	if err != nil {
		log.Fatalf("connect mysql: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   VARCHAR(255) NOT NULL,
			applied_at DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			PRIMARY KEY (filename)
		) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4`); err != nil {
		log.Fatalf("create schema_migrations: %v", err)
	}

	files, err := listSQL(*dir)
	if err != nil {
		log.Fatalf("list migrations: %v", err)
	}
	if len(files) == 0 {
		log.Printf("[migrate] no .sql files in %s", *dir)
		return
	}

	applied := 0
	for _, path := range files {
		name := filepath.Base(path)
		var exists int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE filename = ?`, name).Scan(&exists)
		if err != nil {
			log.Fatalf("[migrate] check %s: %v", name, err)
		}
		if exists > 0 {
			log.Printf("[migrate] skip %s (already applied)", name)
			continue
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("[migrate] read %s: %v", name, err)
		}
		for i, stmt := range splitStatements(string(raw)) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				log.Fatalf("[migrate] apply %s statement %d: %v", name, i+1, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (filename) VALUES (?)`, name); err != nil {
			log.Fatalf("[migrate] record %s: %v", name, err)
		}
		log.Printf("[migrate] applied %s", name)
		applied++
	}
	log.Printf("[migrate] done, %d file(s) applied", applied)
}

func listSQL(dir string) ([]string, error) {
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

// splitStatements 把 SQL 文件拆成单条语句：跳过行注释，忽略引号内的分号。
// 本项目的迁移只含 DDL，不依赖存储过程分隔符。
func splitStatements(sqlText string) []string {
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
