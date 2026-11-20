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

- 文本请求的全局系统提示词：Python 在 `text_chat_parts` 和 `conversation_events` 各调一次 `normalize_messages`，
  设置了 `global_system_prompt` 时上游会收到两遍；Go 版只加一次。
- 文本缓存：Python 的 chat 非流式与 Responses 在请求体刚好对上时共用一个缓存键（一个存 JSON、一个当流式分片读，会报错）；
  Go 版把两类条目分开存。工具 schema 写进提示词时 Go 的 JSON 键按字母序（Python 按传入顺序）。
- 文本接口里的生图（chat 的 image 模型、Responses 的 image_generation 工具）：Go 版等全部图片完成后一次性输出流式分片，
  与 `/v1/images/*` 的流式一致；Python 边生成边推进度。
- 续轮询：Python 用不带 token 的客户端去轮询，必然失败；Go 版记下出错的账号邮箱，用同一个账号续轮询。
- 提交生图任务的返回值：Python 在后台线程启动后才序列化共享的任务 dict，多数时候返回 running；Go 返回提交时的 queued 快照。
- 未知路径的 404：Python 是 FastAPI 的 `{"detail":"Not Found"}`，Go 目前走上游路由的纯文本 404，第 11 步接前端静态文件时一并处理。
- 保存图片前的过期清理：Python 每保存一张都遍历一次图片目录；Go 版最多每分钟一次（结果相同，另有每 30 分钟的定时清理）。

## 与上游 v2 的取舍

- 上游的定时器（`service.Scheduler`）不启动：它刷新的是上游自己的 accounts 表（兼容层不用），并按文件修改时间
  滚动清理 `data/images`，会破坏「按自然日保留」。改为启动兼容层的两个后台任务：账号巡检 `pool.Watch`
  （`start_limited_account_watcher`）和图片清理 `imagestore.Run`（`_auto_cleanup_worker`，启动时先清一次）。
  上游任务表的过期清理留到第 9 步。

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
- [x] 6. 日志与统计
  - `internal/logs`：`List`（类型、起止日期按北京时间日期，最新在前，最多 200 条；按写入顺序 `seq` 排，同一秒内也不乱）、`Delete`、`ImageOutputCounts`（今天/昨天/近 7/14/15/30 天/全部，按每日合计）
  - `/api/logs`、`/api/logs/delete`、`/api/logs/image-counts`
  - 契约：061 一致；060 只差在 Python 最新一条是文本接口的调用日志（第 8 步接上后一致），生图与账号日志的字段两边相同
- [x] 7. 图片管理
  - `internal/imagestore`：索引表 `compat_images`、标签表 `compat_image_tags`，文件仍在 `data/images`、`data/image_thumbnails`；
    保存（local/webdav/both，WebDAV 失败则本次出图失败，同 Python）、每分钟最多一次与磁盘对账、分页与标签筛选、批量删除
    （文件、缩略图、索引、标签）、按自然日清理、缩略图（320 内，EXIF 转正）、ZIP 下载、存储统计、清理到目标空间、WebDAV 测试与同步
  - 路由：`/api/images`、`/api/images/paths`、`/images/*`、`/image-thumbnails/*`、`/api/images/delete`、`/api/images/download`、
    `/api/images/download/*`、`/api/images/tags`（GET/POST/DELETE）、`/api/images/storage`、`/api/images/storage/cleanup-to-target`、
    `/api/image-storage/test`、`/api/image-storage/sync`
  - 顺带补上账号巡检 `pool.Watch`（之前没迁，上游定时器刷的是另一张表）
  - 契约：**53/60 一致**，2 个已知差异；剩下 5 个属于第 8、9 步（054–056 文本接口，060 依赖文本调用日志，062 生图任务）
- [x] 8. 文本接口
  - `internal/textapi`：消息规整（含远程图片、data URL、base64、Anthropic source）、token 计数、无效 token 换号重试、
    回答缓存与在途去重、联网搜索工具、模型目录（按账号类型路由，匿名线路兜底）、`/v1/chat/completions`（文本/生图/搜索）、
    `/v1/responses`（文本/生图/搜索）、`/v1/messages`（XML 工具调用）、`/v1/search`
  - `internal/chatgptweb/text.go`：文本对话、模型列表、搜索的请求体；上游 Go 客户端加了匿名线路（`/backend-anon/*`，不带 Authorization）
  - 流式：拿到第一个分片才开始写 SSE，之前的错误按普通错误返回（同 `LoggedCall.run`），之后逐片 flush
  - 契约：**57/60 一致**，2 个已知差异；只剩 062（第 9 步）
- [x] 9. 任务类
  - [x] 生图任务 `internal/imagetasks`（表 `compat_image_tasks`）：提交去重、按 key 隔离、重启后未完成的标为失败、按保留天数清理、进度、调用日志、续轮询；
    `/api/image-tasks`、`/api/image-tasks/generations`（带参考图转 edits）、`/api/image-tasks/edits`、`/api/image-tasks/{id}/resume-poll`
  - 契约：**60/60 一致**（含 2 个已知差异）
  - [x] PPT/PSD 可编辑文件任务 `internal/filetasks`（表 `compat_editable_file_tasks`，文件在 `data/files/<kind>/<id>/`）与导出流程
    `chatgptweb/editable.go`（上传、prepare、thinking 对话、轮询产物、四种下载地址依次尝试）；`/v1/ppt/generations`、`/v1/psd/generations`、
    `/v1/editable-file-tasks`、`/files/*`
  - 契约新增 063–069（任务提交、缺字段、PSD/PPT、文件 404、搜索空 prompt），**67/67 一致**，3 个已知差异
  - 测试环境：`tools/contract/setup.sh` 一键重建（PostgreSQL、假上游、Python 参照实例、tiktoken 缓存）
- [ ] 10. 账号来源与备份
  - 路由检查收紧：只算兼容层注册的路由（之前转给上游的同名路由也算"已实现"，但上游用的是另一套账号表），因此 CPA、sub2api、OAuth、clearance 共 18 个路由重新列入待办
  - [x] 登录 `internal/openailogin`：密码登录（authorize、sentinel 工作量证明、密码校验、TOTP 二步验证、手动跟随重定向、换 token）、PKCE、TOTP、手动 OAuth 桥；
    每次登录独立的 Chrome 指纹客户端和 cookie，走账号代理（没有则全局代理）
  - [x] 密码重新登录 `pool.PasswordRelogin`：**之前 Go 版的 Relogin 钩子没接，手动"重新登录"和 app_session_terminated 自动重登都不会执行**，现已接上
  - [x] 批量协议登录 `internal/protocollogin`、`/api/accounts/protocol-login*`、`/api/accounts/oauth/start|finish`
  - 契约新增 082–087，**73/73 一致**，3 个已知差异
  - [x] CPA、sub2api `internal/sources`（配置仍是整份 JSON 文档 cpa_config.json / sub2api_config.json，导入任务、敏感字段过滤、sub2api 登录令牌缓存、分页）、`/api/proxy/clearance/test`
  - 契约新增 088–089g，**82/82 一致**，3 个已知差异
  - [ ] 备份（6 个路由）
- [ ] 11. 前端适配与走查
- [ ] 12. 完整测试
- [ ] 13. 并行上线
