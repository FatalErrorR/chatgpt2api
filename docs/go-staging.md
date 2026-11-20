# 第 12 步测试实例与第 13 步回切方案（2026-11-20）

本文写的是方案，生产机上的任何操作都要用户另行确认后才做。
生产环境：`vps-192-210-231-240` 的 `/opt/chatgpt2api`，Docker Compose 部署，服务为 `app` 和 `sentinel-solver`，端口 3000，`config.json` 与 `data/` 都是绑定挂载。

## 一、核对原方案后发现的问题

原方案只写了「独立端口、独立 PostgreSQL 库」，见 `go-acceptance.md` E 节。
Go 版有几个行为，在这个前提下会碰到生产数据，或者部署方式本身行不通：

| # | 行为 | 后果 | 本方案的处理 |
| --- | --- | --- | --- |
| 1 | `config.json` 原地读写；保存时先写 `config.json.tmp`，再 rename 覆盖 | 共用会改写生产设置；**单文件绑定挂载时 rename 失败**（`device or resource busy`，HTTP 500，本机 Docker 已复现） | 挂载独立的**设置目录** `conf/`，设 `CHATGPT2API_CONFIG_FILE=/app/conf/config.json` |
| 2 | `data/images` 原地读取；启动后立即按保留天数删图，磁盘不足时执行 `DeleteToTarget` | 删除生产图片 | 独立的图片目录副本 |
| 3 | 启动后立即运行 `pool.Watch`，刷新账号并保活 refresh token（会**轮换** refresh token）；管理页的「刷新」也会直接请求固定的 `https://auth.openai.com/oauth/token`，不受上游地址设置影响 | 生产 Python 手里的 refresh token 失效，账号掉线 | 演练实例只在**没有出口的内部网络**里；借用账号测试期间从生产号池移出 |
| 4 | 定时备份按配置上传到 R2，并轮换（删除）旧备份 | 删除或覆盖生产备份 | 副本配置关闭备份；演练实例本来就出不去 |
| 5 | 图片存储为 WebDAV 或 both 时，会同步、删除远端 | 改动生产 WebDAV | 副本配置改为 local |
| 6 | 停机等待 `image_poll_timeout_secs + 60` 秒；Docker 默认 10 秒后 SIGKILL | 停机时截断生成中的请求 | `stop_grace_period` 大于 `CHATGPT2API_SHUTDOWN_TIMEOUT_SECS` |
| 7 | 迁移命令中写的 `"$DATABASE_URL"` 由宿主 shell 展开 | 宿主机上的变量可能指向别的库 | 迁移在对应服务的容器里执行，不写 `-database-url`；工具先打印实际连到的库，`-expect-db` 不符即拒绝 |

`CHATGPT2API_BACKGROUND_JOBS=off`（e2012f1）只关闭定时任务，挡不住手动刷新，因此只作为辅助防线，真正的隔离在网络层。

## 二、第 12 步：测试实例

文件：`deploy/staging/docker-compose.yml`；本机验证脚本：`deploy/staging/verify-local.sh`（只用合成数据）。

### 两个实例，各自的库和网络

| | staging（真实上游测试） | rehearsal（迁移演练） |
| --- | --- | --- |
| 数据 | 生产快照，`accounts.json` 只保留借用的 3–5 个账号 | 生产 `data/` 的完整快照 |
| PostgreSQL | 服务 `pg`，库 `c2a_staging`，独立数据卷 | 服务 `pg-rehearsal`，库 `c2a_rehearsal`，独立数据卷 |
| 网络 | `staging`，可以出网 | `rehearsal`（`internal: true`）：没有默认路由，解析不了外部域名 |
| 管理入口 | `127.0.0.1:3401` | `rehearsal-gate`（socat）只转发 `127.0.0.1:3402` → `rehearsal:80` |
| 后台任务 | 照常（E6 要测账号刷新） | `CHATGPT2API_BACKGROUND_JOBS=off`，上游地址指向 `127.0.0.1:1`（辅助） |
| 启动 | `docker compose up -d` | `docker compose --profile rehearsal up -d rehearsal-gate`，**只在需要看页面时启动** |

纯迁移和对账用命令行完成，不需要启动 rehearsal 服务。
rehearsal 和 staging 互相连不到对方的 PostgreSQL。

### 目录（与 `/opt/chatgpt2api` 完全分开）

```
/opt/chatgpt2api-go-staging/
  docker-compose.yml        # deploy/staging/docker-compose.yml，项目名 chatgpt2api-go-staging
  .env                      # PG_PASSWORD=<新随机值>；STAGING_PORT、REHEARSAL_PORT、SHUTDOWN_SECS、STOP_GRACE 可选
  conf/config.json          # staging 设置：生产 config.json 的副本，按下表修改
  rehearsal-conf/config.json
  data/                     # staging 的 images、image_thumbnails、files 副本
  rehearsal-data/           # rehearsal 的图片副本（只看页面时需要，可以为空）
  source/staging/           # 只含借用账号的快照（只读挂载）
  source/full/              # 完整快照（只读挂载，只有 rehearsal 能看到）
```

复制方式如下。生产目录只读，不写入。

```
cd /opt/chatgpt2api-go-staging
cp /opt/chatgpt2api/config.json conf/config.json            # 再按下表修改
cp /opt/chatgpt2api/config.json rehearsal-conf/config.json  # 同上，另换 auth-key
rsync -a --exclude images --exclude image_thumbnails --exclude files /opt/chatgpt2api/data/ source/full/
rsync -a source/full/ source/staging/                       # 再把 accounts.json 换成借用账号
rsync -a /opt/chatgpt2api/data/{images,image_thumbnails,files} data/
```

复制图片前，先看 `du -sh /opt/chatgpt2api/data/images` 和 `df`。
如果副本会让磁盘可用空间低于 500 MB（Go 写死为 500 MB；Python 读 `image_min_free_mb`，未设置时也是 500），生产 Python 自己也会开始删图。
这种情况下只复制最近几天的图片，或者让 staging 从空的图片目录启动。

### 设置副本的修改（`conf/` 与 `rehearsal-conf/` 两份）

| 键 | 值 | 原因 |
| --- | --- | --- |
| `auth-key` | 新的随机值，两份不同 | 与生产分开 |
| `backup.enabled` | `false` | 不上传、不轮换生产的 R2 备份 |
| `image_storage.mode` | `local` | 不同步、不删除 WebDAV |
| `auto_remove_invalid_accounts` | `false` | 测试期间不自动删号 |
| `image_poll_timeout_secs` | 保持生产的值 | `SHUTDOWN_SECS` 取这个值 + 60，`STOP_GRACE` 再多 15 秒 |

### 操作顺序

1. **先在生产机上用合成数据验证隔离**：执行 `deploy/staging/verify-local.sh`。
   - 它使用自己的项目名 `c2a-staging-local` 和临时目录，端口为 3401–3403，所以要在正式的 staging 启动前运行。
   - 本机 Docker Desktop 上已经全部通过，但 Linux 服务器的 Docker 网络规则要在服务器上再确认一次。
   - 任一项失败就停下。
2. 按上文准备目录。借用账号前，先在生产管理页导出这些账号，再从生产号池删除；这一步改动生产，需要确认。
3. 迁移，每个库各执行一次，都在对应服务的容器里执行：
   ```
   docker compose up -d --wait pg
   docker compose run --rm --no-deps app ./chatgpt2api-migrate -data /source -expect-db c2a_staging -dry-run
   docker compose run --rm --no-deps app ./chatgpt2api-migrate -data /source -expect-db c2a_staging
   docker compose --profile rehearsal up -d --wait pg-rehearsal
   docker compose --profile rehearsal run --rm --no-deps rehearsal ./chatgpt2api-migrate -data /source -expect-db c2a_rehearsal
   ```
   - 每次的第一行是「目标库：…（用户、服务器）」，最后一行列出库里的账号。
   - 核对：`c2a_staging` 只有借用的账号；`c2a_rehearsal` 的账号数与生产一致。
   - `-expect-db` 写错或连到别的库时，命令会以退出码 2 退出，不做任何改动。
4. 启动 staging：执行 `docker compose up -d`，通过 SSH 隧道打开 3401，测试 E1–E7。
5. 需要在完整副本上看页面时，执行 `docker compose --profile rehearsal up -d rehearsal-gate`。看完后执行 `docker compose stop rehearsal rehearsal-gate`。

### 本机验证结果（`deploy/staging/verify-local.sh`，合成数据，退出码 0）

- **迁移**
  - staging 服务执行 `-expect-db c2a_rehearsal` 时被拒绝，并打印实际库 `c2a_staging`；
  - `c2a_staging` 只有 3 个借用账号；`c2a_rehearsal` 有 130 个账号；
  - staging 的 PostgreSQL 里没有演练库。
- **设置**
  - 在设置页保存「出图慢就换」77 秒，HTTP 200，宿主机上的 `conf/config.json` 已经写入；
  - `--force-recreate` 重建容器后，`/api/settings` 仍然读到 77 秒；
  - 对照实验：单文件挂载时保存返回 HTTP 500，错误为 `rename /app/config.json.tmp /app/config.json: device or resource busy`。
- **演练隔离**
  - 演练容器没有默认路由；
  - 解析不了 `auth.openai.com`；
  - 连不上 `1.1.1.1:443`；
  - 能连 `pg-rehearsal`，连不到 staging 的 `pg`；
  - 经 gate 可以使用管理接口，看到 130 个账号。
  - 合成账号的 access token 是已过期的 JWT，带 refresh token，不走代理。手动刷新时，它直接请求 `https://auth.openai.com/oauth/token`，结果是 `lookup auth.openai.com on 127.0.0.11:53: server misbehaving`，token 没有变化。
  - 对照：staging 容器能连 `auth.openai.com:443`，说明这个检测方法确实能发现出网。

### 测试结束

1. 执行 `docker compose --profile rehearsal down`；数据卷保留到确认不再需要为止。
2. 从 staging 导出借用的账号（token 可能已经轮换），加回生产号池。
3. 删除 `/opt/chatgpt2api-go-staging`，生产目录不受影响。

## 三、第 13 步：并行期间的新写入如何在回切 Python 时保留

### 现状（如实说明）

- Go 只写 PostgreSQL，Python 只读 `data/*.json`、`logs.jsonl`。**目前没有任何把 Go 的写入带回 Python 的工具**。
- 现在回切，Go 期间的写入全部丢失：
  - 账号状态、额度、新加或删除的账号、代理变更；
  - 调用日志和每日出图数；
  - 新图片的索引和标签；
  - 生图任务和 API key。
- 更严重的是，Go 轮换过的 refresh token 不会回到 Python，相关账号在 Python 侧会失效。
- 备份压缩包与 Python 布局相同，但只有账号快照（`snapshots/accounts.json`），不能当作完整的回切工具。

### `go-acceptance.md` G1 的写法需要修改

「两个版本并行，切 10% 流量」如果两边共用同一批账号，问题和第 12 步借用账号相同：

- 两边各自刷新 token，互相使对方的 token 失效；
- 冷却、额度、异常状态两边各算各的；
- 管理操作只落在一边。

### 建议方案

1. **按账号分区，不共享**
   - 并行期间，从生产号池移出约 10% 的账号交给 Go，流量按同样比例切分；两边没有共同的账号。
   - 管理操作（加号、代理、设置、API key）只在 Python 上做。Go 期间冻结这些操作，否则回切时要逐项合并。
2. **反向导出工具**
   - 在第 13 步之前写好 `chatgpt2api-migrate -export`：从 PostgreSQL 写出 Python 格式的文件，按以下规则合并进 Python 的 `data/`。

     | 数据 | 合并规则 |
     | --- | --- |
     | 账号 | 按 token 和邮箱匹配，用 Go 的当前值（含轮换后的 token）替换 Go 分区里的账号 |
     | 日志 | 把切换时刻之后 Go 写入的行按 id 去重，追加到 `logs.jsonl` |
     | 每日出图数 | 把 Go 期间的每日数加到 `image_output_counts.json` |
     | 图片 | 把 Go 的图片目录中新增的文件复制回来，并合并 `image_index.json`、`image_tags.json` |
     | 生图任务、可编辑文件任务 | 按 id 合并 |

   - 对账沿用这次迁移的思路：先从源数据独立推算出应有结果，再与合并后的文件比对。
   - 往返测试：Python 数据 → 迁移 → Go 写入 → 导出合并 → 再迁移，与 Go 库逐项一致。
3. **回切步骤**（对应 G2「一条命令」）：
   1. 把流量切回 Python；
   2. 停止 Go，Go 会等进行中的请求结束；
   3. 执行导出；
   4. 短暂停止 Python；
   5. 合并并对账；
   6. 启动 Python。

   对账失败时不启动合并结果，Python 用合并前的备份启动（G3）。
4. **先演练**：先在本机用走查数据演练，再在 staging 上演练一次，然后才进入第 13 步。

### 结论

第 13 步之前还需要做三件事：

1. 反向导出与合并工具及其测试；
2. 账号分区和冻结管理操作的流程；
3. 回切演练。

这三件事目前都没有做，不能直接进入第 13 步。
