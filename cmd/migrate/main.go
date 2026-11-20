// Command migrate copies the Python version's data directory into the Go version's database.
//
//	go run ./cmd/migrate -data /opt/chatgpt2api/data -database-url "$DATABASE_URL"
//
// The source files are only read. Run it again to bring the database back in line with the
// files (snapshot tables are replaced, logs and daily counts are upserted). Exit status 1 when a
// check fails.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/basketikun/chatgpt2api/internal/migrate"
	"github.com/basketikun/chatgpt2api/internal/repository"
)

func main() {
	dataDir := flag.String("data", "data", "the Python version's data directory (read only)")
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "target database (default $DATABASE_URL)")
	logDays := flag.Int("log-days", 30, "migrate log lines from the last N days; 0 for all")
	flag.Parse()
	if *databaseURL == "" {
		fmt.Fprintln(os.Stderr, "需要 -database-url 或环境变量 DATABASE_URL")
		os.Exit(2)
	}
	if info, err := os.Stat(*dataDir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "数据目录不存在：%s\n", *dataDir)
		os.Exit(2)
	}
	repo, err := repository.Open(*databaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接数据库失败：", err)
		os.Exit(1)
	}
	defer repo.Close()
	opts := migrate.Options{DataDir: *dataDir, LogDays: *logDays, Now: time.Now()}
	started := time.Now()
	report, err := migrate.Run(repo.DB(), opts)
	if report != nil {
		for _, item := range report.Items {
			if item.Skipped != "" {
				fmt.Printf("%-24s 源 %6d  写入 %6d  %s\n", item.Name, item.Source, item.Written, item.Skipped)
			} else {
				fmt.Printf("%-24s 源 %6d  写入 %6d\n", item.Name, item.Source, item.Written)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "迁移失败：", err)
		os.Exit(1)
	}
	fmt.Printf("迁移完成，用时 %s\n\n对账：\n", time.Since(started).Round(time.Millisecond))
	failed := 0
	for _, check := range migrate.Verify(repo.DB(), opts, report) {
		mark := "通过"
		if !check.OK {
			mark, failed = "不一致", failed+1
		}
		fmt.Printf("  %-6s %-30s 期望 %6d  实际 %6d  %s\n", mark, check.Name, check.Expected, check.Actual, check.Detail)
	}
	if failed > 0 {
		os.Exit(1)
	}
}
