package main

import (
	"context"
	"flag"
	"log"
	"os"
	"strings"

	"github.com/daqiaoliang-coder/agentrix/pkg/migrate"
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

	applied, err := migrate.Apply(context.Background(), db, *dir)
	if err != nil {
		log.Fatalf("[migrate] %v", err)
	}
	for _, name := range applied {
		log.Printf("[migrate] applied %s", name)
	}
	log.Printf("[migrate] done, %d file(s) applied", len(applied))
}
