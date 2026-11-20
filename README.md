<h1 align="center">ChatGPT2API</h1>

<p align="center">把 ChatGPT 网页端的生图、改图和文本对话封装成 OpenAI 兼容 API，并提供号池、代理池、图片管理和后台面板的自托管服务。</p>

本仓库在开源项目 chatgpt2api（MIT，见 [LICENSE](./LICENSE)）的基础上二次开发，生产环境按本仓库 `main` 分支部署。

> [!WARNING]
> 免责声明：
>
> 本项目涉及对 ChatGPT 官网文本生成、图片生成与图片编辑等相关接口的逆向研究，仅供个人学习、技术研究与非商业性技术交流使用。
>
> - 严禁将本项目用于任何商业用途、盈利性使用、批量操作、自动化滥用或规模化调用。
> - 严禁将本项目用于破坏市场秩序、恶意竞争、套利倒卖、二次售卖相关服务，以及任何违反 OpenAI 服务条款或当地法律法规的行为。
> - 严禁将本项目用于生成、传播或协助生成违法、暴力、色情、未成年人相关内容，或用于诈骗、欺诈、骚扰等非法或不当用途。
> - 使用者应自行承担全部风险，包括但不限于账号被限制、临时封禁或永久封禁以及因违规使用等所导致的法律责任。
> - 使用本项目即视为你已充分理解并同意本免责声明全部内容；如因滥用、违规或违法使用造成任何后果，均由使用者自行承担。
> - 本项目基于对 ChatGPT 官网相关能力的逆向研究实现，存在账号受限、临时封禁或永久封禁的风险。请勿使用你自己的重要账号、常用账号或高价值账号进行测试。

## 快速开始

### Docker 运行

```bash
git clone git@github.com:FatalErrorR/chatgpt2api.git
cd chatgpt2api
docker compose up -d
```

- 启动前在 `config.json` 里设置 `auth-key`（后台管理员密钥），也可以用环境变量 `CHATGPT2API_AUTH_KEY` 覆盖
- Web 面板：`http://localhost:3000`，API 地址：`http://localhost:3000/v1`
- 数据目录：`./data`（账号、日志、图片、任务记录）
- `docker-compose.yml` 使用本仓库发布的镜像 `ghcr.io/fatalerrorr/chatgpt2api:latest`（每次推送 `main` 由 GitHub Actions 构建），
  并在本机构建 `sentinel-solver`（生图遇到 Turnstile 挑战时使用，只在内部网络，不要把它的 8080 端口映射到公网）

从源码构建镜像：

```bash
docker compose -f docker-compose.local.yml up -d --build   # 端口 8000，默认 SQLite 存储
```

升级：

```bash
git pull
docker compose pull && docker compose up -d
```

升级前备份 `config.json` 和 `data/`。更完整的部署、升级和迁移说明见 [部署与升级指南](./docs/deployment.md)。

### WARP / FlareSolverr 稳定代理

图片链路经常遇到 Cloudflare 拦截时，可以使用附带的 WARP + Privoxy + FlareSolverr 方案：

```bash
cp .env.example .env
docker compose -f docker-compose.warp.yml up -d --build
```

会启动 `warp-proxy`（WARP SOCKS5 出口）、`privoxy`（转成 HTTP 代理）、`flaresolverr`（刷新 Cloudflare clearance）、
`init-config`（幂等写入 `proxy_runtime` 默认配置）和主服务。默认只有发往 ChatGPT 的请求走稳定代理。
代理优先级：账号自己的代理 > 稳定代理运行时 > 全局代理。端口和参数在 `.env` 里调整，也可以在设置页的「稳定代理运行时」里保存和测试。

### 本地开发

```bash
uv sync
uv run main.py            # 后端，http://localhost:8000

cd web
bun install
bun run dev               # 前端
```

测试：

```bash
uv run python -m unittest discover -s test -t .
cd web && npx tsc --noEmit -p . && node --test test/*.test.ts
```

默认测试不依赖真实上游（测试里用假上游，`CHATGPT2API_UPSTREAM_BASE_URL` 可把客户端指向它）；需要真实上游的集成测试要先在
`localhost:8000` 启动服务、准备 `assets/image.png`，再设置 `CHATGPT2API_INTEGRATION=1`。每次 push 和 PR 都会由 GitHub Actions 的
`Test` 工作流运行上述检查，推送 `main` 还会构建并发布镜像。

### 存储后端

账号和 API key 的存储方式由环境变量 `STORAGE_BACKEND` 决定：`json`（默认，本地文件）、`sqlite`、`postgres`（配合 `DATABASE_URL`）、
`git`（配合 `GIT_REPO_URL`、`GIT_TOKEN`）。日志、图片索引、代理清单等其它数据始终在 `data/` 目录。

## 功能

### 后台页面

| 页面 | 内容 |
|:--|:--|
| 总览 | 号池状态（可用、限流、异常、额度）、今日 / 近 7 天 / 近 30 天出图数、账号列表与管理 |
| 代理 | 代理清单：批量添加、按厂商命名、健康检测、停用并把账号迁走、重新平均分配、每个代理挂多少账号的上限 |
| 生图 | 在线生图 / 改图 / 多图编辑，进度追踪，超时后可继续等待，本地保存会话历史 |
| 图片 | 服务端保存的图片：按日期和标签筛选、分页、批量下载、按日期批量删除（带进度条）、按自然日的定时清理规则 |
| 日志 | 调用日志和账号日志，按类型和日期筛选 |
| 设置 | 生图参数、代理与稳定代理运行时、AI 审核与敏感词、用户 API key、CPA / Sub2API 连接、图片存储（本地 / WebDAV）、R2 备份 |
| 调试 | 在页面上直接试调用对话、搜索、PPT、PSD 等接口 |

另有 `/health`（号池健康看板，`?format=json` 返回 JSON）。

### 生图

- 兼容 `POST /v1/images/generations`、`POST /v1/images/edits`，也可以通过 `/v1/chat/completions`、`/v1/responses` 生图；`n` 支持 1–4
- 模型：`gpt-image-2`、`gpt-image-2.5*`、`gpt-image-2.6*`（网页链路）和 `codex-gpt-image-*`（Codex 链路，仅 Plus / Team / Pro，
  与网页链路额度分开）；Codex 链路传 `output_format: "gif"` 可输出 GIF 动图
- `/v1/images/generations` 请求里带参考图时自动按图片编辑处理；参考图过大时自动压缩到上游可接受的大小
- 请求尺寸会换算成画幅比例发给上游；可选按请求尺寸精确输出、按目标长边放大（Lanczos 或 ONNX 模型）
- 后台生图任务：`/api/image-tasks/*` 提交后轮询结果，页面刷新不丢任务
- 可编辑 PPT / PSD 文件生成（`/v1/ppt/generations`、`/v1/psd/generations`，需要 Plus / Team / Pro 账号）

### 稳定性

- 按账号轮询并限制每个账号的并发；账号 token 失效时先尝试刷新，失败再换号或剔除
- 换号重试：403、TLS / 连接超时、HTTP/2 流被重置（curl 92）、模型不可用、上传达到上限等情况会换一个账号重试；403 的账号冷却 10 分钟
- 「换号生成」（`image_failover_enabled`）：单个账号出图超过设定时间仍没结果时换号重新生成，并归还卡住账号的并发名额
- Cloudflare 挑战页自动重试；已生成图片下载失败时退避重试；生图 SSE 流有硬超时
- Turnstile 挑战交给 `sentinel-solver` 求解，走与账号相同的出口
- 接口线程池默认 256（`api_threadpool_tokens`），大量长时间生图不会拖住其它接口

### 号池与账号来源

- 自动刷新账号的邮箱、类型、额度和恢复时间；定时检查限流账号，refresh token 定期保活
- 异常账号显示具体原因；可对选中账号逐个测试生图；支持密码重新登录（可配合 TOTP 两步验证）
- 导入方式：`access_token`、本地 CPA JSON 文件、远程 CPA、Sub2API、手动 OAuth、批量协议登录（每行 `邮箱----密码----TOTP密钥`）
- 导入时可把新账号平均分配到多个代理，跳过异常代理

### 文本接口

- `/v1/chat/completions`、`/v1/responses`、`/v1/messages`（Anthropic 格式，工具调用用 XML 约定模拟）、`/v1/search`
- 按模型自动选择能用该模型的账号类型；支持网页搜索工具；相同请求在短时间内命中缓存

### 其它

- 用户 API key：管理员可以发放普通用户 key，用户只能调用生成接口
- 请求内容审核：敏感词和可选的 AI 审核
- 图片存储：本地、WebDAV 或两者同时；定时清理和磁盘空间不足时自动清理最旧的图片
- 备份：定时把配置和数据打包上传到 Cloudflare R2，可加密、可轮换

## API

所有 AI 接口都需要请求头：

```http
Authorization: Bearer <auth-key>
```

<details>
<summary><code>GET /v1/models</code></summary>
<br>

返回当前可用的模型列表。

```bash
curl http://localhost:8000/v1/models \
  -H "Authorization: Bearer <auth-key>"
```

<details>
<summary>说明</summary>
<br>

| 字段   | 说明                                                                                                         |
|:-----|:-----------------------------------------------------------------------------------------------------------|
| 返回模型 | 号池里各类账号在官网可用的文本模型（按账号类型汇总，5 分钟缓存），加上图片模型 `gpt-image-2`、`gpt-image-2.5*`、`gpt-image-2.6*`；有 Plus / Team / Pro 的 Codex 账号时另有 `codex-gpt-image-*` 及 `plus-` / `team-` / `pro-` 前缀版本 |
| 接入场景 | 可接入 Cherry Studio、New API 等上游或客户端                                                                          |

<br>
</details>
</details>

<details>
<summary><code>POST /v1/images/generations</code></summary>
<br>

OpenAI 兼容图片生成接口，用于文生图。

```bash
curl http://localhost:8000/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "一只漂浮在太空里的猫",
    "n": 1,
    "response_format": "b64_json"
  }'
```

<details>
<summary>字段说明</summary>
<br>

| 字段                | 说明                                                 |
|:------------------|:---------------------------------------------------|
| `model`           | 图片模型，当前可用值以 `/v1/models` 返回结果为准；`gpt-image-2` 兼容旧链路，`gpt-image-2.5` / `gpt-image-2.5-flare` / `gpt-image-2.5-sunburst` 走 Images 2.5 |
| `prompt`          | 图片生成提示词                                            |
| `n`               | 生成数量，当前后端限制为 `1-4`                                 |
| `response_format` | 当前请求模型中包含该字段，默认值为 `b64_json`                       |

<br>
</details>
</details>

<details>
<summary><code>POST /v1/images/edits</code></summary>
<br>

OpenAI 兼容图片编辑接口，可上传图片文件，也可按官方 JSON 格式传入图片链接并生成编辑结果。

```bash
curl http://localhost:8000/v1/images/edits \
  -H "Authorization: Bearer <auth-key>" \
  -F "model=gpt-image-2" \
  -F "prompt=把这张图改成赛博朋克夜景风格" \
  -F "n=1" \
  -F "image=@./input.png"
```

也可以直接传图片 URL：

```bash
curl http://localhost:8000/v1/images/edits \
  -H "Authorization: Bearer <auth-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "把这张图改成赛博朋克夜景风格",
    "images": [
      {"image_url": "https://example.com/input.png"}
    ]
  }'
```

<details>
<summary>字段说明</summary>
<br>

| 字段          | 说明                                            |
|:------------|:----------------------------------------------|
| `model`     | 图片模型，默认 `gpt-image-2`；2.5 可用 `gpt-image-2.5` / `flare` / `sunburst` |
| `prompt`    | 图片编辑提示词                                       |
| `n`         | 生成数量，当前后端限制为 `1-4`                            |
| `image`     | 需要编辑的图片文件，使用 multipart/form-data 上传           |
| `images`    | JSON 图片引用数组，支持 `{"image_url": "https://..."}` |
| `image_url` | 表单模式下也可直接传图片链接，支持重复字段传多张图                     |

<br>
</details>
</details>

<details>
<summary><code>POST /v1/chat/completions</code></summary>
<br>

面向文本、网页搜索与图片场景的 Chat Completions 兼容接口，不是完整通用聊天代理。

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-image-2",
    "messages": [
      {
        "role": "user",
        "content": "生成一张雨夜东京街头的赛博朋克猫"
      }
    ],
    "n": 1
  }'
```

<details>
<summary>字段说明</summary>
<br>

| 字段                   | 说明                                                                           |
|:---------------------|:-----------------------------------------------------------------------------|
| `model`              | 文本、搜索或图片模型；搜索模型会触发网页搜索兼容逻辑                                                   |
| `messages`           | 消息数组，支持文本、搜索和图片请求内容                                                          |
| `n`                  | 图片生成数量，按当前实现解析为图片数量                                                          |
| `stream`             | 文本、搜索和图片场景均支持，仍在测试                                                           |
| `tools`              | 文本场景支持 `web_search` / `web_search_preview` / `web_search_preview_2025_03_11` |
| `web_search_options` | 传入时会触发网页搜索兼容逻辑                                                               |

<br>
</details>
</details>

<details>
<summary><code>POST /v1/responses</code></summary>
<br>

面向文本、网页搜索和图片生成工具调用的 Responses API 兼容接口，不是完整通用 Responses API 代理。

```bash
curl http://localhost:8000/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-5",
    "input": "生成一张未来感城市天际线图片",
    "tools": [
      {
        "type": "image_generation"
      }
    ]
  }'
```

<details>
<summary>字段说明</summary>
<br>

| 字段       | 说明                                                                                      |
|:---------|:----------------------------------------------------------------------------------------|
| `model`  | 响应中会回显该模型字段，搜索和图片生成会走对应兼容逻辑                                                             |
| `input`  | 输入内容；搜索使用最后一条用户文本，图片生成需能解析出提示词                                                          |
| `tools`  | 支持 `image_generation`、`web_search`、`web_search_preview`、`web_search_preview_2025_03_11` |
| `stream` | 已实现，但仍在测试                                                                               |

<br>
</details>
</details>

## 开发分支

- `main`：当前生产版本（Python）
- `go-v2`：迁移到 Go 的分支，接口与 `main` 保持一致，开发中，计划和进度见该分支的 `docs/go-migration.md`
