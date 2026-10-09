# AGENTS.md — 协作与提交规范

本文件适用于本仓库（`gks/`，Go module `github.com/LiZeC123/gks`），描述在该仓库里工作时的固定约定。
上级目录的 `dev.md`（设计文档）与 `gks.yaml`（手动测试配置）不在本仓库内，默认不动。

## 1. 提交规范（重要）

- **每个任务完成后，先跑测试；全绿后立即 `git commit`，不必再询问用户。**
- **一个任务一个提交**，不要积攒多个不相关的改动；大改动按「可独立验收」的粒度拆分。
- **测试不通过就不提交**：修到全绿；确实无法通过时，停下来向用户说明阻塞点，不得硬提交。
- 消息格式沿用仓库历史风格：中文 conventional 标题 + 分节正文。

  ```text
  <type>(<scope>): <中文摘要>

  背景/动机：
  - ...

  变更：
  - ...

  实测（涉及运行时行为时必写，附真实数据）：
  - ...

  测试：
  - ...

  文档：
  - ...
  ```

  `type`：`feat` | `fix` | `docs` | `refactor` | `test` | `chore` | `perf`；
  `scope` 用包名或模块名（如 `config,metrics`、`client`、`observability`）。
- 提交前检查 `git status`：构建产物、日志、本地配置（`gks.yaml`、`*.local.yaml`，可能含真实 PSK）
  一律不得进入提交（`.gitignore` 已覆盖这些）。
- 不重写已推送的历史、不强推。未推送的上一次提交如需修订，用 `git commit --amend`，
  否则用新提交叠加。

## 2. 测试与验收

提交前必须全部通过：

```bash
gofmt -l ./cmd ./internal ./test   # 应无输出
go vet ./...                       # 应无输出
go test -race ./...                # 单元测试 + 端到端集成测试
```

运行时行为改动的端到端验收（脚本自建临时目录与随机 PSK，会顺带校验统计端点、错误码、并发）：

```bash
./test/e2e.sh
TARGETS="http://127.0.0.1:PORT/" ./test/e2e.sh   # 外网不可达时缩范围（--socks5 那一项仍会访问 baidu）
```

测试数据隔离：

- 单元测试一律用 `t.TempDir()` / `httptest`；`test/e2e.sh` 在 `mktemp -d` 里生成配置。
- **不要用上级目录的 `gks.yaml` 跑测试或开发验证**，那是给手动测试用的（含真实 PSK 与端口）。
- **浏览器/人工可视验证交给用户手工完成**：页面渲染、JS 局部刷新、深浅色、响应式这类无法被
  Go 单测覆盖的场景，agent 不安装/不调用无头浏览器、不做截图；只把结果写成可在浏览器里复核的
  说明（路由、参数、看什么），凡是能落到单测里的行为都补成单测。

受限环境（例如文件沙箱里 Go 缓存不可写）可用：

```bash
GOCACHE=/tmp/gks-gocache go test -buildvcs=false ./...
```

`go build` 报 `writing stat cache: ... operation not permitted` 只是该环境的缓存写入警告，
二进制仍会正常生成；不要为此改动仓库里的构建命令。

## 3. 代码与文档约定

- 注释、日志、错误信息、README 一律用中文；提交信息用中文。
- 配置走 `internal/config` 的三段式严格模式：
  - 新增字段必须同步四处：`internal/config/config.go` 的类型与校验、`test/gks.yaml.example`
    的带注释示例、`README.md` 的配置表、以及对应单测。
  - `common` 只放「两端必须一致」的字段；写进 `client`/`server` 会因严格模式启动失败。
- 观测有三个出口，职责不要混：
  - **控制台表格**（人看）：`internal/metrics/table.go` 只产出纯文本；TTY 原地刷新的控制序列在
    `internal/monitor`；默认**不输出**，加 `-console` 才在控制台周期刷新。
  - **`GET /metrics`、`GET /healthz`**（程序拉）：JSON 字段是**外部契约**，改动字段或语义必须
    同步 README 的 JSON 表并提升 `schema_version`。
  - **日志文件**（事后查）：日志只写 `*.log.file`，为空即丢弃；控制台不再出现日志。
    表格与空 `log.file` 同时生效即「完全静默」。
  - `cmd/dashboard`（`gks-dashboard`）是 `/metrics` 的**只读消费者**：改 `/metrics` 的字段或语义时
    必须同时检查 `internal/dashboard` 的解码与页面渲染；它自己的 `/api/state` 同样是对外契约
    （版本见 `internal/dashboard/server.go` 的 `stateSchemaVersion`），改动需同步 README。
  - 面板资源（`internal/dashboard/assets/`）必须保持自包含：不得引用任何外部 CDN/字体/图标，
    否则单文件分发会缺资源（有单测兜底）。
- 统计口径集中在 `internal/metrics`（`Collector` + 1s 粒度环形历史，保留 10 分钟）；
  `common.metrics_interval` = 控制台刷新周期 + `/metrics` 默认速率窗口（必须 > 0 且 ≤ 10m）。
  速率一律由历史窗口两端算出，不要在别处再实现一套采样。
- 统计端点默认 `127.0.0.1:12081`（未配置时）；同机跑两端必须在示例与测试里错开端口
  （示例用客户端 12081 / 服务端 12082）。端点绑定失败按 fail-fast 处理（启动失败）。

## 4. 完成的标准（Definition of Done）

1. 代码、配置、文档同步改完；
2. `gofmt` 干净、`go vet` 干净、`go test -race ./...` 全绿；
3. 涉及运行时行为的改动，做过一次真实进程验证（起 server + client 后 `curl /metrics`，或跑
   `./test/e2e.sh`），并把实测数据写进提交信息；
4. 已提交、工作区干净，回复中给出提交号与验收结论。
