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

每一项做完都要有 Go 测试，并在 `进度` 里打勾、写上提交号。

1. **数据迁移脚本**：读生产的 `accounts.json`、`proxies.json`、`auth_keys.json`、
   `image_index.json`、`image_tags.json`，导入 v2 的数据库。日志历史可以不迁。
   先拿本地数据的副本试，不碰生产文件。
2. **生图重试策略**（`main` 的 `services/protocol/conversation.py` 中的
   `_generate_single_image`，测试在 `test/test_image_stream_retry.py`）：
   - 换号生成：每个号最多等 `image_failover_secs`，总时长不超过 `image_poll_timeout_secs`
   - 403：冷却该号 10 分钟，本次请求避开同出口的号
   - curl 92 / HTTP/2 断流：换号最多 2 次；5 分钟内全站超过 10 次就熔断
   - 上传限流（`upload_protection_limit_exceeded`）冷却、模型不存在暂避 10 分钟、
     回文字时重试
   - 每条路径都要释放账号的并发位（`main` 上 68fc542 修过泄漏）
3. **代理池**（`services/proxy_list_service.py`、`services/proxy_reassign_service.py`、
   `api/proxies.py`）：代理清单、批量添加和厂商命名、一轮检测后停用失败代理并迁走账号、
   单代理账号上限 `proxy_max_accounts`、按负载最小重新分配。
4. **统计与总览**：今日生图分时段计数（`services/log_service.py` 的
   `image_output_counts`）、待处理事项、今天失败的调用。
5. **图片管理**：标签、按路径分批删除（索引一次写完）、按自然日清理规则、
   按日期删除预览。
6. **前端**：二选一，先做决定再动手——
   a. 把 `main` 的 Next.js 前端（`web/`）接到 Go 的接口上，需要逐页适配接口形状；
   b. 用上游的创作台，把总览、代理页、图片管理等功能加进去。
7. **其他按需**：参考图压缩（`image_ref_fit`）、放大、GIF、`image_generation_settings`
   比例字段（上游已有）。
8. **上线**：先和 Python 版并行，切少量流量观察，保留回滚到 `main` 的镜像。

## 不迁移

- 压缩优化（`main` 上已删除，会卡死容器）
- 跳转上游 GitHub 的链接和版本检查

## 进度

- [x] 建分支，上游代码编译和测试通过（89a8ad0，go 1.26.3）
- [ ] 1. 数据迁移脚本
- [ ] 2. 生图重试策略
- [ ] 3. 代理池
- [ ] 4. 统计与总览
- [ ] 5. 图片管理
- [ ] 6. 前端（先定 a 或 b）
- [ ] 7. 其他
- [ ] 8. 并行上线
