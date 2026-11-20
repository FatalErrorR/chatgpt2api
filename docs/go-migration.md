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

## 已知的 Python 行为差异（Go 版不照抄）

- `_decode_image_base64`：base64 短于 512 字符时先当文件路径检查，超过 255 字符会抛「File name too long」。只影响约 380 字节以下的极小图片。

- 换号重试的「已输出」判断（2026-11-20 用户确认，只改 Go，生产 Python 不动）：Python 的 `emitted_for_token`
  收到任何输出（含进度事件）就为真，流一开始断流、TLS、403 等就不再换号，导致 main 1162271 的断流重试实际很少触发。
  Go 版改为只有收到结果或消息才算已输出（参考上游 v2：除内容违规和无额度外一律换号）。换号生成关闭时的普通轮询超时
  仍按原判断，避免一次请求等五遍 600 秒。测试：`TestResetAfterProgressSwitchesAccount`、
  `TestPlainTimeoutAfterProgressStillDoesNotSwitch`、`TestDroppedStreamAfterProgressSwitchesAccount`。

## 不迁移

- 压缩优化（`main` 上已删除，会卡死容器）
- 上游 v2 自带的原生 JS 前端（`web/index.*`）：保留文件，但不作为我们的界面
- 跳转上游 GitHub 的链接和版本检查

## 进度

- [x] 建分支，上游代码编译和测试通过（89a8ad0，go 1.26.3）
- [x] Go 版上游地址可配置（`CHATGPT2API_UPSTREAM_BASE_URL`），用于假上游
- [x] 验收标准与测试方案（`docs/go-acceptance.md`），路由清单（94 个）
- [x] 0. 基础设施（d9d2717）
  - [x] 路由组装移到 `internal/app`，路由覆盖测试（1c43454）：93 个路由中上游已有 49 个，待实现 44 个
  - [x] 数据模型对照 `docs/contract/data-model.md`
  - [x] 假上游 `tools/fakeupstream`：Python 版（main 9b48b49 起可用 `CHATGPT2API_UPSTREAM_BASE_URL` 指向它）已跑通账号刷新、文生图、图生图 1/3/4 张参考图、chat；场景 ok/slow/notool/403/upload429/badtoken/nomodel/drop
  - [x] 本机 PostgreSQL 16（docker `c2a-pg`，127.0.0.1:55432，用户/密码/库 c2a），上游 Go 版在其上建表启动正常
  - [x] 契约对比 `tools/contract`：60 个场景，Python 版自身两次录制 60/60 一致。**起点：上游 Go 原样 9/60 一致**
  - [x] PostgreSQL：`tools/contract/run-go.sh` 每次新建 `c2a_contract` 库，Go 版在 PostgreSQL 上跑契约
- [ ] 1. 数据层与迁移脚本
- [x] 2. 鉴权与设置
  - 路由结构：`app.Engine` 先挂兼容层（`internal/compat`，Python 版接口），没匹配到的请求转给 `app.Upstream`（上游 v2 的全部路由），迁移期间服务始终可用
  - `internal/settings`：直接读写 Python 格式的 `config.json`；用 Python `ConfigStore` 生成的 656 组向量逐一比对（`tools/vectors/settings_vectors.py`）
  - `internal/compat`：管理员 key + 用户 key（只存 SHA-256，兼容迁移过来的 `auth_keys.json`）、`/auth/login`、`/version`、`/api/settings`、`/api/auth/users*`、`/api/storage/info`、`/api/third-party-apps`
  - 契约：**18/60 一致**，1 个已知差异（storage-info 返回数据库后端结构）。`/health` 依赖号池统计，放到第 3 步
- [x] 3. 号池
  - `internal/pool`：账号以 JSON 文档存 PostgreSQL（`compat_accounts`，保留字典顺序，只写变化的账号），轮询选号、并发位、暂避、上传冷却、token 刷新与轮换、批量刷新（10 并发）、重登、测试生图、导出、统计
  - `internal/upstream/userinfo.go`：与 Python `get_user_info` 相同的三个并发请求和解析；`internal/compat/remote.go` 把号池接到上游客户端，OAuth 刷新 token
  - `internal/logs`：写日志并按 UTC+8 累计每日生图数；`internal/pyval`、`internal/proxylist`（`proxy_key`）
  - `/api/accounts*`（除 CPA/sub2api/OAuth/协议登录）、`/health`（含 HTML 监控页）
  - 契约：**29/60 一致**，2 个已知差异（存储后端结构）。导入时按代理平均分配依赖第 5 步的代理清单
  - 测试对照：`docs/contract/test-map.md`
- [x] 4. 生图链路
  - [x] `internal/imagegen`：单次尝试、换号重试主循环（6 类策略 + 并发位归还）、n 张并行、结果格式化、token 计数（105 组 Python 向量一致）；`test_image_stream_retry.py` 23 个测试全部移植通过
  - [x] `internal/chatgptweb`：用上游 Go 客户端的传输发送 Python 版的请求体（上传、sentinel、prepare、picture_v2 对话、SSE 硬上限、轮询/二次确认/内容违规、下载重试、Cloudflare 重试与代理健康）
  - [x] `/v1/images/generations`、`/v1/images/edits`（multipart/JSON/data URL/远程 URL/蒙版）、内容过滤与 AI 审核、调用日志、`/v1/*` 错误格式；假上游实测文生图、图生图 1/3/4 张参考图
  - [x] 参考图压缩 `internal/imagefit`（含 EXIF 转正；无 WebP 编码器，带透明的图先试 PNG）、出图放大与精确尺寸 `internal/imagesize`（生产开着精确尺寸；model 方式按 Lanczos）、Codex 线路、账号测试生图、GIF
  - [x] 端到端：进程内假上游 + 真实号池/重试/协议层，10 个场景，每个都检查并发位归零（`internal/chatgptweb/e2e_test.go`）
  - 契约：**44/60 一致**，2 个已知差异
- [x] 5. 代理池
  - `internal/proxylist`（清单、健康、预留、按负载最少分配、批量添加与厂商命名）、`internal/proxyops`（检测、停用并迁移、启用、重新分配、代理商故障判定）、`internal/docstore`（proxies.json 等整份 JSON 文档）
  - `/api/proxies*`、`/api/accounts/reassign-proxy`、`/api/proxy/test`、`/api/proxy/runtime`；导入时按代理平均分配已接通
  - 契约：**39/60 一致**，2 个已知差异；Python 的 48 个代理测试中 34 个已对应（其余在第 4、10 步）
- [ ] 6. 日志与统计
- [ ] 7. 图片管理
- [ ] 8. 文本接口
- [ ] 9. 任务类
- [ ] 10. 账号来源与备份
- [ ] 11. 前端适配与走查
- [ ] 12. 完整测试
- [ ] 13. 并行上线
