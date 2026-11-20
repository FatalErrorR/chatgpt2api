// Command migrate copies the Python version's data directory into the Go version's database.
//
//	go run ./cmd/migrate -data /opt/chatgpt2api/data -database-url "$DATABASE_URL"
//
// The source files are only read. Everything is written in one transaction and verified against
// the files before it commits: a malformed source or a failed check leaves the database as it
// was, with exit status 1. Running it again mirrors the files again (it replaces what the Go
// version wrote since; do not rerun after the Go version has taken writes).
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
			fmt.Printf("%-24s 源 %6d  写入 %6d  %s\n", item.Name, item.Source, item.Written, item.Note)
		}
		if len(report.Checks) > 0 {
			fmt.Println("\n对账（与源文件逐项比对，在同一个事务里，不一致则整体回滚）：")
			for _, check := range report.Checks {
				mark := "通过"
				if !check.OK {
					mark = "不一致"
				}
				fmt.Printf("  %-6s %-34s 期望 %6d  实际 %6d  %s\n", mark, check.Name, check.Expected, check.Actual, check.Detail)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n迁移失败，目标库未改动：", err)
		os.Exit(1)
	}
	fmt.Printf("\n迁移完成并已提交，用时 %s\n", time.Since(started).Round(time.Millisecond))
}
