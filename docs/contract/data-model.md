# 数据模型对照（Python `main` → Go `go-v2`）

依据：2026-11-20 生产数据的字段名（未读取任何值），以及 `main` 上的 `services/*.py`。
迁移脚本、表结构和接口序列化都以这份对照为准。

## 配置：`config.json` → Go

前端设置页读写的是 Python 版 `/api/settings` 的 44 个键。Go v2 用 `config.yaml`，键名和层级都不同。

**决定**：Go 版直接读写 `config.json`（与 Python 版同格式、同默认值、同校验），
内部再映射到上游代码需要的 `config.Config` 字段。`config.yaml` 不再使用。
这样生产切换时 `config.json` 原样沿用，回滚也不用转换。

44 个键：`ai_review` `auto_relogin_after_refresh` `auto_remove_invalid_accounts`
`auto_remove_rate_limited_accounts` `backup` `base_url` `chat_completion_cache`
`default_thinking_effort` `default_upstream_model_name` `global_system_prompt`
`image_account_concurrency` `image_check_before_hit_enabled` `image_edit_upload_cooldown_enabled`
`image_exact_size_enabled` `image_failover_enabled` `image_failover_secs` `image_min_free_mb`
`image_parallel_generation` `image_poll_initial_wait_secs` `image_poll_interval_secs`
`image_poll_timeout_secs` `image_ref_fit_budget_bytes` `image_ref_fit_enabled` `image_ref_fit_max_edge`
`image_remove_conversation_after_result` `image_remove_conversation_always` `image_req_fit_budget_bytes`
`image_req_fit_enabled` `image_retention_days` `image_settle_enabled` `image_settle_secs`
`image_storage` `image_timeout_retry_secs` `image_upscale_enabled` `image_upscale_method`
`image_upscale_target_edge` `log_levels` `proxy` `proxy_max_accounts` `proxy_runtime`
`refresh_account_interval_minute` `sensitive_words` `sentinel_solver_url` `third_party_apps`

另有非设置键：`auth-key`（管理员密钥）、存储后端等，迁移时原样保留。

## 账号：`accounts.json` → 表 `accounts`

生产 139 个账号，每个都有下面 23 个字段。

| Python 字段 | Go v2 列 | 处理 |
| --- | --- | --- |
| access_token | AccessToken | 直接对应 |
| email / user_id / type / status / quota / proxy / success / fail / created_at / last_used_at | 同名列 | 直接对应 |
| restore_at | ResetAfter | 改名对应，接口输出仍叫 `restore_at` |
| default_model_slug | — | 新增列 |
| edit_upload_blocked_until | — | 新增列（上传限流冷却） |
| invalid_count / last_invalid_at | — | 新增列 |
| last_refresh_error / last_refresh_error_at | LastError（部分） | 新增两列，保留时间 |
| last_token_refresh_at / last_token_refresh_error / last_token_refresh_error_at | LastRefreshAt（部分） | 新增列 |
| limits_progress | — | 新增 JSON 列 |
| source_type | — | 新增列 |
| （运行时）image_inflight、proxy_key | ImageInflight / — | `proxy_key` 由代理地址计算，不入库 |

Go v2 还有 RefreshToken、IDToken、AccountID、IsCodex、Remaining，保留，迁移时留空或按默认值。

## 代理：`proxies.json` → 新表 `proxies`

条目字段：`url`、`name`、`disabled`。Go v2 没有代理清单表，需要新建。
健康状态只存在内存里（与 Python 版一致）。

## API key：`auth_keys.json` → 表 `api_keys`

条目字段：`id`、`name`、`role`、`enabled`、`created_at`、`last_used_at`、`key_hash`。

Python 版只存哈希，Go v2 存明文 `Key` 并按明文查找。**新增 `KeyHash` 列，鉴权改为按哈希比对**，
否则已发出去的用户 key 迁移后无法使用。管理员密钥仍然来自配置里的 `auth-key`。

## 日志：`logs.jsonl` → 表 `logs`

每行：`id`、`time`、`type`（call / account 等）、`summary`、`detail`（JSON，按类型字段不同）。
Go v2 的 `logs` 表没有 `type` 列，需要新增。按决定只迁近 30 天。

## 每日生图计数：`image_output_counts.json` → 新表 `image_output_counts`

`{"days": {"YYYY-MM-DD": {"edits": n, "generations": n}}}`，从 2026-09-09 起全部迁移。

## 图片：`image_index.json`、`image_tags.json`

- 索引条目：`rel` `path` `name` `date` `size` `created_at` `storage` `local` `webdav` `remote_url` `width` `height`。
  生产约 5,500 条。
- 标签：`{rel: [tag, …]}`。
- 图片文件和缩略图仍在 `data/images`、`data/image_thumbnails`，不迁移，原地沿用。

Go v2 用 `files` 表管理文件，结构不同。为了保持索引与磁盘对账的行为一致，新建 `compat_images` 表（rel 主键，整条索引存 JSON，
date/created_at/webdav 另存列用于筛选排序，seq 保持 Python dict 的顺序）和 `compat_image_tags` 表（rel、tags JSON、seq）。

## 生图任务：`image_tasks.json` → 表 `tasks`

条目字段：`id` `owner_id` `mode` `model` `size` `quality` `status` `progress` `data` `error` `usage`
`duration_ms` `created_at` `created_ts` `started_ts` `updated_at` `updated_ts`。生产有 89 条。
与 Go v2 的 `Task` / `TaskResult` 对照后决定复用还是新建（在第 9 步处理）。
