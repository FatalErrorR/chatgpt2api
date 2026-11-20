# Go 版迁移计划（go-v2 分支）

## 目标

把生产上的 Python 版（`main`）迁到上游 Go v2.0，同时保留我们 fork 上做的功能。
迁完之前生产一直跑 `main`，本分支不部署。

## 分支约定

- `go-v2` 基于 `upstream/main`（basketikun/chatgpt2api，Go v2.0.0）。上游的修复用
  `git merge upstream/main` 合进来，不要 rebase（这个分支已推送到 origin）。
- Python 版的功能以 `main` 为准。移植某个功能前，先读 `main` 上对应的代码和测试，
  把行为和测试用例一起搬过来。
- 本地工作目录是 `../chatgpt2api-go`（git worktree）。`main` 的目录不动。
- 上游 CI（`.github/workflows/docker-publish.yml`）只在推 tag 时构建和发布镜像。
  不要在这个分支上打 tag。
- 跑测试用 `go test ./...`。集成测试要 `TEST_CHATGPT_ACCESS_TOKEN` 才会跑，
  默认跳过；不要用生产账号跑，会消耗额度。

## 上游 v2.0 已有

账号单独代理、单账号生图并发（`image.account_concurrency`）、换号重试
（`image.account_retries`）、密码重登、WebDAV、图片保留天数、sentinel/turnstile、
语音合成与转写、Codex 生图线路、创作台（原生 JS 前端 `web/index.*`）。

已知问题：#426 三张以上参考图 prepare 返回 422；#430 VP8X WebP 参考图解码失败。

## 要移植的功能（按顺序）

范围和验收标准见 `docs/go-acceptance.md`：全部功能，数据库用 PostgreSQL。
后端按 Python 版的接口格式实现，前端保持 `main` 的 Next.js 界面不变。
每一项做完都要有 Go 测试，并在 `进度` 里打勾、写上提交号。

0. **基础设施**：路由覆盖测试（对照 `docs/contract/python-routes.txt`）、
   假上游 `tools/fakeupstream`、契约对比 `tools/contract`、PostgreSQL 集成测试。
1. **数据层**：按 Python 版的数据结构建表（账号、代理、key、图片索引、标签、
   每日计数、日志、任务），以及迁移脚本（近 30 天日志加每日计数）。
2. **鉴权与设置**：`/auth/login`、`/api/auth/*`、`/api/settings`、`/api/storage`、`/version`、`/health`。
3. **号池**（`api/accounts.py`、`services/account_service.py`）：列表、筛选所需字段、
   导入、刷新、重登、测试生图、导出、`proxy_key`、`image_inflight`。
4. **生图链路**（`services/protocol/conversation.py`、`services/openai_backend_api.py`）：
   web 和 Codex 两条线路、参考图压缩、放大、GIF、`image_generation_settings`，以及全部重试策略：
   - 换号生成：每个号最多等 `image_failover_secs`，总时长不超过 `image_poll_timeout_secs`
   - 403：冷却该号 10 分钟，本次请求避开同出口的号
   - curl 92 / HTTP/2 断流：换号最多 2 次；5 分钟内全站超过 10 次就熔断
   - 上传限流（`upload_protection_limit_exceeded`）冷却、模型不存在暂避 10 分钟、
     回文字时重试
   - 每条路径都要释放账号的并发位（`main` 上 68fc542 修过泄漏）
5. **代理池**（`services/proxy_list_service.py`、`services/proxy_reassign_service.py`、
   `api/proxies.py`、`/api/proxy/*` 运行时）：清单、批量添加和厂商命名、一轮检测后停用并迁走账号、
   单代理上限 `proxy_max_accounts`、按负载最小重新分配。
6. **日志与统计**：`/api/logs*`、今日生图分时段计数、总览所需数据。
7. **图片管理**：列表与分页、标签、分批删除、按自然日清理、按日期预览、缩略图、下载、WebDAV。
8. **文本接口**：`/v1/chat/completions`（含缓存）、`/v1/responses`、`/v1/messages`、`/v1/models`、`/v1/search`。
9. **任务类**：`/api/image-tasks`、`/v1/editable-file-tasks`、`/v1/ppt`、`/v1/psd`、`/files/*`。
10. **账号来源与备份**：CPA、sub2api、OAuth、协议登录、备份、第三方应用。
11. **前端适配与走查**（标准 C）。
12. **完整测试**：契约、迁移演练、真实上游测试实例、压测（标准 A–F）。
13. **并行上线**（标准 G）。

## 不迁移

- 压缩优化（`main` 上已删除，会卡死容器）
- 上游 v2 自带的原生 JS 前端（`web/index.*`）：保留文件，但不作为我们的界面
- 跳转上游 GitHub 的链接和版本检查

## 进度

- [x] 建分支，上游代码编译和测试通过（89a8ad0，go 1.26.3）
- [x] 验收标准与测试方案（`docs/go-acceptance.md`），路由清单（94 个）
- [ ] 0. 基础设施
  - [x] 路由组装移到 `internal/app`，路由覆盖测试（1c43454）：93 个路由中上游已有 49 个，待实现 44 个
  - [x] 数据模型对照 `docs/contract/data-model.md`
  - [ ] 假上游 `tools/fakeupstream`
  - [ ] 契约对比 `tools/contract`
  - [ ] PostgreSQL 集成测试（本机需启动 Docker Desktop）
- [ ] 1. 数据层与迁移脚本
- [ ] 2. 鉴权与设置
- [ ] 3. 号池
- [ ] 4. 生图链路
- [ ] 5. 代理池
- [ ] 6. 日志与统计
- [ ] 7. 图片管理
- [ ] 8. 文本接口
- [ ] 9. 任务类
- [ ] 10. 账号来源与备份
- [ ] 11. 前端适配与走查
- [ ] 12. 完整测试
- [ ] 13. 并行上线
