// Command migrate copies the Python version's data directory into the Go version's database.
//
//	chatgpt2api-migrate -data /source -expect-db c2a_staging            # target from $DATABASE_URL
//	chatgpt2api-migrate -data /source -expect-db c2a_staging -dry-run   # validate only, write nothing
//
// It prints the database it is connected to before doing anything, and with -expect-db refuses to
// write anywhere else (required for PostgreSQL), so a URL expanded in the wrong shell cannot send a
// production copy into the wrong database.
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
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/basketikun/chatgpt2api/internal/migrate"
	"github.com/basketikun/chatgpt2api/internal/repository"
)

func main() {
	dataDir := flag.String("data", "data", "the Python version's data directory (read only)")
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "target database (default $DATABASE_URL)")
	logDays := flag.Int("log-days", 30, "migrate log lines from the last N days; 0 for all")
	expectDB := flag.String("expect-db", "", "the database name the target must have (required for PostgreSQL)")
	dryRun := flag.Bool("dry-run", false, "read and validate the source and show the target, write nothing")
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
	target := describeTarget(repo.DB())
	fmt.Printf("目标库：%s\n源目录：%s\n\n", target.text, *dataDir)
	if target.postgres && *expectDB == "" {
		fmt.Fprintln(os.Stderr, "PostgreSQL 目标必须用 -expect-db 写明库名")
		os.Exit(2)
	}
	if *expectDB != "" && target.name != *expectDB {
		fmt.Fprintf(os.Stderr, "目标库是 %q，不是 -expect-db 指定的 %q，未做任何改动\n", target.name, *expectDB)
		os.Exit(2)
	}
	opts := migrate.Options{DataDir: *dataDir, LogDays: *logDays, Now: time.Now()}
	if *dryRun {
		report, err := migrate.Validate(opts)
		if report != nil {
			for _, item := range report.Items {
				fmt.Printf("%-24s 源 %6d  %s\n", item.Name, item.Source, item.Note)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "\n源数据有问题：", err)
			os.Exit(1)
		}
		fmt.Println("\n预演：源数据校验通过，未写入")
		return
	}
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
	printAccounts(repo.DB(), target.text)
}

type targetInfo struct {
	name, text string
	postgres   bool
}

// describeTarget is the database the connection actually reached, as the server reports it.
func describeTarget(db *gorm.DB) targetInfo {
	if db.Dialector.Name() != "postgres" {
		return targetInfo{name: "", text: db.Dialector.Name() + "（本地文件库）"}
	}
	var row struct {
		Name, Usr, Addr string
		Port            int
	}
	db.Raw("select current_database() as name, current_user as usr, coalesce(host(inet_server_addr()), 'socket') as addr, coalesce(inet_server_port(), 0) as port").Scan(&row)
	return targetInfo{name: row.Name, text: fmt.Sprintf("%s（用户 %s，服务器 %s:%d）", row.Name, row.Usr, row.Addr, row.Port), postgres: true}
}

// printAccounts shows what landed, so each database can be checked for holding only what it should.
func printAccounts(db *gorm.DB, target string) {
	var emails []string
	column := "json_extract(data, '$.email')"
	if db.Dialector.Name() == "postgres" {
		column = "data::json->>'email'"
	}
	db.Table("compat_accounts").Order("position").Pluck(column, &emails)
	fmt.Printf("%s 现有账号 %d 个", target, len(emails))
	if len(emails) <= 10 {
		fmt.Printf("：%s", strings.Join(emails, "、"))
	}
	fmt.Println()
}
