# gks

基于 KCP 的 SOCKS5 代理系统。完整技术方案见上级目录的 [`dev.md`](../dev.md)。

**当前进度：阶段 1 + 阶段 2 完成**（见 dev.md §8）。

- 阶段 1：项目骨架、配置加载与校验、结构化日志、帧编解码（握手态/安全态）、AEAD/HKDF/nonce、AUTH 握手与防重放 —— 均有单测。
- 阶段 2：KCP 上跑通 `AUTH_REQ/AUTH_RESP` → 安全态、PING/PONG 心跳（抖动间隔）、Session 级 echo、握手/心跳超时、拒绝明文降级。
- 尚未实现（后续阶段）：SOCKS5 监听与 CONNECT 转发、多路复用 `Mux` 实现、连接池、优雅排空。

## 目录结构

```text
gks/
├── cmd/
│   ├── client/         # 客户端入口（阶段二：连上服务端做 echo 联调）
│   └── server/         # 服务端入口（阶段二：认证 + echo 回显）
├── internal/
│   ├── protocol/       # 帧编解码、AEAD/HKDF/nonce、AUTH 握手与重放缓存
│   ├── mux/            # Mux/Stream 接口 + Session（读循环/单写循环/心跳）
│   ├── transport/      # KCP 拨号与监听（传输层加密开关）
│   ├── config/         # 单文件分段配置 + 严格校验
│   └── log/            # slog 封装与字段规范
└── test/
    └── gks.yaml.example
```

## 快速开始

```bash
# 构建
go build -o bin/server ./cmd/server
go build -o bin/client ./cmd/client

# 准备配置（示例里的 PSK 是占位值，仅用于本地联调）
cp test/gks.yaml.example /tmp/gks.yaml
# 生成并替换真实密钥：openssl rand -base64 32

# 两个程序读同一个配置文件，各取自己的段落
./bin/server -c /tmp/gks.yaml &
./bin/client -c /tmp/gks.yaml -n 3 -msg "hello"
```

预期输出（客户端）：

```text
发送: hello #1
回显: hello #1
...
会话统计: conv=1157069344 role=client secure=true frames_in=4 frames_out=4 pings_sent=1 pongs_recv=1
```

服务端日志（stderr）会显示 `server_start`、`session_up`、`session_down`、`server_stop` 等结构化事件。

## 配置

- **单个 YAML 文件**，顶层 `client:` / `server:` 两段；两个程序 `-c` 同一文件。
- **严格模式**：出现未知字段直接启动失败。
- `psk` 必须是 base64 编码的 32 字节；两端一致，且不能是全 0 占位值。
- `crypt`（传输层）与 `aead`（应用层）两端必须一致：
  - `aead`：`chacha20-poly1305`（默认）或 `aes-256-gcm`。
  - `crypt`：`none`（默认，阶段 B）或 `aes-128-gcm` 等（阶段 A，见 dev.md §0.3）。
- `client.auth.mode` 目前只支持 `none`；`userpass` 尚未实现（配置中出现会直接报错）。

## 测试

```bash
go test ./...            # 全部单测与集成测试
go test -race ./...      # 含竞态检测（推荐）
go vet ./...
```

覆盖要点：帧 round-trip 与边界/异常、AAD 篡改检测、nonce 双向独立递增与溢出拒绝、HKDF 域分离、AUTH 的时间窗/重放/错误 PSK、半关闭之外的安全性质（不降级：安全态明文帧必须断开）、心跳判死、配置逐项校验、真实 KCP 上的握手 + echo + 心跳。

## 阶段二的已知限制

1. 客户端**还不监听本地 1080 端口**，`curl --socks5-hostname` 尚不可用（阶段 3）；`client.auth.mode` 与 `listen` 目前只做校验。
2. 数据面只有 Session 级 echo（`TypeTestEcho = 0xF0`，仅用于联调，阶段 3 会移除）。
3. **服务端不发心跳**，只响应 PING（server 段配置中没有心跳字段）。客户端按 `client.kcp.heartbeat_interval` 保活。
4. **优雅关闭尚不完整**：kcp-go 的 `Listener.Close()` 会关闭被所有会话共用的 UDP socket，因此当前收到 SIGTERM 时会立刻断开所有会话，`shutdown_grace` 实际未起到「等活跃流结束」的作用。这需要在阶段 6 改为「先停止 Accept，期间对新会话直接拒绝，等排空后再关闭监听」。
