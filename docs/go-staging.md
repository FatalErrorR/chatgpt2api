# 第 12 步测试实例与第 13 步回切方案（2026-11-20）

本文写的是方案，生产机上的任何操作都要用户另行确认后才做。
生产环境：`vps-192-210-231-240` 的 `/opt/chatgpt2api`，Docker Compose 部署，服务为 `app` 和 `sentinel-solver`，端口 3000，`config.json` 与 `data/` 都是绑定挂载。

## 一、核对原方案后发现的问题

原方案只写了「独立端口、独立 PostgreSQL 库」，见 `go-acceptance.md` E 节。
Go 版有几个行为，在这个前提下会碰到生产数据：

| # | 行为 | 若共用生产资源的后果 | 本方案的处理 |
| --- | --- | --- | --- |
| 1 | `config.json` 原地读写，格式与 Python 相同；设置页保存会直接写这个文件 | 改写生产设置 | 使用独立副本，读写挂载，容器重启后仍保留 |
| 2 | `data/images` 原地读取；启动后立即执行 `CleanupOldImages` 按保留天数删图，磁盘不足时还会执行 `DeleteToTarget` | 删除生产图片 | 使用独立的图片目录副本 |
| 3 | 启动后立即运行 `pool.Watch`，刷新账号并保活 refresh token（会**轮换** refresh token） | 生产 Python 手里的 refresh token 失效，账号掉线 | 两个库分开；演练库关闭后台任务；借用的账号在测试期间从生产号池移出 |
| 4 | 定时备份会按配置上传到 R2，并轮换（删除）旧备份 | 删除或覆盖生产备份 | 副本配置中关闭备份 |
| 5 | 图片存储为 WebDAV 或 both 时，会同步到 WebDAV，删图时也会删远端 | 改动生产 WebDAV | 副本配置改为 local |
| 6 | 默认停机等待时间为 `image_poll_timeout_secs + 60` 秒；Docker 默认 10 秒后发送 SIGKILL | 停机时截断正在生成的请求 | 设置 `stop_grace_period` |

第 3 项新增了开关 `CHATGPT2API_BACKGROUND_JOBS=off`（e2012f1）。
开关打开时，账号巡检、图片清理和定时备份都不启动。
`TestBackgroundJobsCanBeTurnedOff` 证明：关闭后启动不产生任何上游请求；去掉该开关后测试失败，因为启动后立即产生了 3 个上游请求。

## 二、第 12 步：测试实例

### 两个库，用途分开

| 库 | 内容 | 用途 | 能否连真实上游 |
| --- | --- | --- | --- |
| `c2a_rehearsal` | 生产 `data/` 的完整副本 | 迁移演练与对账（D1–D4）；需要对比页面时，在本机回环地址起一个 Go 实例 | **不能**：必须设置 `CHATGPT2API_BACKGROUND_JOBS=off` 和 `CHATGPT2API_UPSTREAM_BASE_URL=http://127.0.0.1:1` |
| `c2a_staging` | 同一份副本，但 `accounts.json` 只保留借用的 3–5 个账号 | E1–E7 真实上游测试 | 可以，后台任务照常运行（E6 要测账号刷新） |

两个库放在独立的 PostgreSQL 容器里，使用独立的数据卷，只在 compose 网络内可见，不对外暴露端口。

### 目录（与 `/opt/chatgpt2api` 完全分开）

```
/opt/chatgpt2api-go-staging/
  docker-compose.yml        # 独立的 compose 项目名，见下
  config.json               # 生产 config.json 的副本，按下表修改；读写挂载
  source/                   # 生产 data/ 的只读快照（迁移的输入）
  data/                     # Go 实例的 data：images、image_thumbnails、files 的副本
```

复制方式如下。生产目录只读，不写入。

```
cp -a /opt/chatgpt2api/config.json   config.json
rsync -a --exclude images --exclude image_thumbnails --exclude files /opt/chatgpt2api/data/ source/
rsync -a /opt/chatgpt2api/data/{images,image_thumbnails,files} data/
```

复制图片前，先看 `du -sh /opt/chatgpt2api/data/images` 和 `df`。
如果副本会让磁盘可用空间低于 500 MB（Go 写死为 500 MB；Python 读 `image_min_free_mb`，未设置时也是 500），生产 Python 自己也会开始删图。
这种情况下不复制全部图片，只复制最近几天的；或者让 staging 从空的图片目录启动，索引会在第一次对账时清掉不存在的条目。

### `config.json` 副本的修改

| 键 | 值 | 原因 |
| --- | --- | --- |
| `auth-key` | 新的随机值 | 与生产分开，泄露也不影响生产 |
| `backup.enabled` | `false` | 不上传、不轮换生产的 R2 备份 |
| `image_storage.mode` | `local` | 不同步、不删除 WebDAV |
| `auto_remove_invalid_accounts` | `false` | 测试期间不自动删号 |
| `image_poll_timeout_secs` | 保持生产的值 | `stop_grace_period` 按这个值算 |

### 借用账号（E 节）

两个实例同时持有同一个账号，就会各自刷新 token。refresh token 轮换后，另一边持有的旧 token 失效。因此按以下步骤操作：

1. 在生产管理页导出这 3–5 个账号，然后**从生产号池删除**。这一步改动生产，需要确认。
2. 用导出的账号生成 `source/accounts.json`，迁移到 `c2a_staging`。
3. 测试结束后，从 staging 导出这些账号（此时 token 可能已经轮换），再加回生产号池。

### compose 草案

```yaml
name: chatgpt2api-go-staging        # 独立项目名，up/down 不会碰到生产的 app、sentinel-solver
services:
  pg:
    image: postgres:16-alpine
    environment: { POSTGRES_USER: c2a, POSTGRES_PASSWORD: <新密码>, POSTGRES_DB: c2a_staging }
    volumes: [ pgdata:/var/lib/postgresql/data ]
  sentinel-solver:                  # 自带一份，不连生产的那个
    build: /opt/chatgpt2api/sentinel_solver
  app:
    image: chatgpt2api-go:staging   # 从 go-v2 构建
    ports: [ "127.0.0.1:3401:80" ]  # 只监听本机，通过 SSH 隧道访问，不对外
    environment:
      DATABASE_URL: postgres://c2a:<新密码>@pg:5432/c2a_staging?sslmode=disable
      CHATGPT2API_SENTINEL_SOLVER_URL: http://sentinel-solver:8080
      CHATGPT2API_SHUTDOWN_TIMEOUT_SECS: "<image_poll_timeout_secs + 60>"
    volumes:
      - ./config.json:/app/config.json
      - ./data:/app/data
    stop_grace_period: <上面的秒数 + 15>s
    depends_on: [ pg, sentinel-solver ]
volumes:
  pgdata:
```

迁移命令。`source/` 是只读快照，迁移只读不写：

```
docker compose run --rm -v ./source:/source:ro app ./chatgpt2api-migrate -data /source -database-url "$DATABASE_URL"
```

`c2a_rehearsal` 用完整的 `source/` 迁移，需要起实例时另起一个服务。这个服务除了上面的设置外，还要加 `CHATGPT2API_BACKGROUND_JOBS=off` 和 `CHATGPT2API_UPSTREAM_BASE_URL=http://127.0.0.1:1`。

### 测试结束

1. 执行 `docker compose down`；数据卷保留到确认不再需要为止。
2. 按上文把借用的账号加回生产。
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
