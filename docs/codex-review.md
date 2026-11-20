# Go 版移植：评审说明（2026-11-20）

给评审者（Codex）的入口。分支 `go-v2`，从上游 `upstream/main`（89a8ad0，Go v2 重写）切出；生产仍是 Python 的 `main`，本分支未上线。

## 目标

把我们 fork 的 Python 版（`main`）的全部后端功能移植到上游 Go v2 上，**接口与 Python 版保持一致**，现有 Next.js 前端不改动即可使用。
数据库用 PostgreSQL。计划与进度见 `docs/go-migration.md`，验收标准见 `docs/go-acceptance.md`。

## 现状

- 已完成：第 0、2–10 步（基础设施、鉴权与设置、号池、生图、代理池、日志、图片管理、文本接口、任务类、账号来源与备份）
- 未做：第 12 步完整测试（含真实账号测试实例、压测）、第 13 步并行上线（第 1 步迁移脚本、第 11 步前端走查已完成，见 go-migration.md）
- Python 的 93 个路由全部由兼容层实现（`docs/contract/pending-routes.txt` 已空，`TestPythonRoutesAreServed` 只认兼容层注册的路由）
- 契约对比 88 个场景：**88/88 一致**，3 个已登记差异（`tools/contract/scenarios.json` 的 `known_diff`）
- 第一轮评审（基准 4074cb3）的修复见 `docs/go-migration.md` 进度中的「Codex 评审修复」；测试实例与回切方案见 `docs/go-staging.md`
- `go vet ./...`、`go test ./...` 全过；并发相关包用 `-race` 跑过

## 架构

- `internal/app`：组装。`app.Engine` 先挂兼容层（`internal/compat`，Python 版接口），未匹配的请求转给 `app.Upstream`（上游 v2 原有路由）。
  上游的定时器 `service.Scheduler` **故意不启动**（它刷新上游自己的账号表、按文件修改时间删图），由 `pool.Watch`、`imagestore.Run`、`backup.Start` 代替。
- `internal/compat`：各路由的处理函数，负责参数校验（仿 pydantic 的 422）、错误格式（`/v1/*` 用 OpenAI 格式，`/v1/messages` 用 Anthropic 格式）、调用日志。
- 业务包（均为 Python 对应文件的移植，包注释写了对应关系）：

| 包 | 对应 Python | 存储 |
| --- | --- | --- |
| `settings` | services/config.py | config.json（与 Python 同一格式） |
| `pool` | account_service.py | 表 `compat_accounts`（整条账号存 JSON，position 保持 dict 顺序） |
| `proxylist`、`proxyops`、`docstore` | proxy_list_service 等 | 表 `compat_documents`（整份 JSON 文档） |
| `imagegen`、`chatgptweb`、`imagefit`、`imagesize` | protocol/conversation.py、openai_backend_api.py、utils/image_* | — |
| `logs` | log_service.py | `compat_logs`、`compat_image_output_counts` |
| `imagestore` | image_storage_service、image_service、image_tags_service | `compat_images`、`compat_image_tags`，文件仍在 data/images |
| `textapi` | protocol 下的文本接口、chat_completion_cache、model_service | — |
| `imagetasks`、`filetasks` | image_task_service、editable_file_task_service | `compat_image_tasks`、`compat_editable_file_tasks` |
| `openailogin`、`protocollogin` | account_service._login_with_password、protocol_login_service、oauth_login_service、utils/sentinel、totp、pkce | — |
| `sources` | cpa_service、sub2api_service | 文档 cpa_config.json、sub2api_config.json |
| `backup` | backup_service.py | 文档 backup_state.json；压缩包布局与 Python 相同 |

- 对上游 Go 代码的改动只有：`internal/upstream/exported.go`（导出传输层）、上游客户端加匿名线路（`/backend-anon/*`）、上游地址可配置（`CHATGPT2API_UPSTREAM_BASE_URL`）。

## 有意与 Python 不同的地方

全部列在 `docs/go-migration.md` 的「已知的 Python 行为差异」，评审时请按那一节核对，不要当成 bug；如果认为某条不该改，也请指出。要点：
换号重试的「已输出」判断、续轮询用原账号、全局系统提示词只加一次、chat 与 Responses 缓存分开、提交任务返回 queued 快照、
文本接口里生图一次性输出、保存前清理最多每分钟一次、未知路径 404 格式（第 11 步处理）。

## 希望重点看的地方

1. **并发与资源**：生图并发位的占用与归还（`imagegen/generate.go`，每个端到端测试都检查归零）、各后台任务的 goroutine 与 `Wait`、
   `pool` 的锁、`textapi.Cache` 的在途去重、流式响应在客户端断开时是否会泄漏（`compat/text.go` 的 `runCall`）。
2. **与 Python 的一致性**：参数校验和错误文本、日志字段、账号状态流转（`pool`、`pool/relogin.go`）、重试规则（`imagegen`）。
3. **安全**：路径穿越（`imagestore.SafeRel`、`filetasks.PublicFile`、`/images/*`、`/files/*`）、响应里不得出现密钥
   （CPA secret、sub2api 密码/api_key、备份 secret/口令、账号密码与 TOTP）、协议登录进度里不得出现密码。
4. **没有在真实上游验证过的部分**（只测了假服务）：密码协议登录与两步验证、OAuth 换 token、PPT/PSD 导出、CPA/sub2api、R2 备份、WebDAV。
   请特别检查请求体、请求头与 Python 版是否一致。
5. 本轮评审中发现的已修问题供参考：Relogin 钩子之前没接（a0ea66e）；几个辅助函数把空值打成文字 `<nil>`（a0ea66e）；
   图片解码器只在某些包里注册导致 token 计数为 0（dacd01a）。

## 怎么跑

```
go vet ./... && go test ./...                 # 单元与端到端（进程内假上游、SQLite）
tools/contract/setup.sh                       # 重建对比环境：PostgreSQL 容器、假上游、Python 参照实例（需要 Docker 与 ../chatgpt2api）
tools/contract/run-python.sh /tmp/c2a-contract-py
tools/contract/run-go.sh /tmp/c2a-contract-go
python3 tools/contract/contract.py diff /tmp/c2a-contract-py /tmp/c2a-contract-go
```

Python 测试与 Go 测试的对照见 `docs/contract/test-map.md`；Python 没有单元测试的模块（CPA、sub2api、备份、可编辑文件）是新写的 Go 测试。

## 不在范围内

生产（Python main）不改；前端不改；上游 v2 自带的原生 JS 前端不用。
