# gks

基于 KCP 的 SOCKS5 代理系统。完整技术方案见上级目录的 [`dev.md`](../dev.md)。

**当前进度：阶段 1 + 阶段 2 + 阶段 3 完成**（见 dev.md §8）。

- 阶段 1：项目骨架、配置加载与校验、结构化日志、帧编解码（握手态/安全态）、AEAD/HKDF/nonce、AUTH 握手与防重放 —— 均有单测。
- 阶段 2：KCP 上跑通 `AUTH_REQ/AUTH_RESP` → 安全态、PING/PONG 心跳（抖动间隔）、握手/心跳超时、拒绝明文降级。
- 阶段 3：**可用的 SOCKS5 代理** —— 本地 SOCKS5 握手与 CONNECT 解析、`CONNECT_REQ/RESP`、`DATA/FIN/RST`、半关闭、目标拨号与错误码映射（服务端拨号失败 → SOCKS5 REP 码）。
- 尚未实现（后续阶段）：多路复用（单会话多流）、连接池、真正的优雅排空、指标。

## 目录结构

```text
gks/
├── cmd/
│   ├── client/         # 客户端入口：本地 SOCKS5 监听 → KCP 隧道
│   └── server/         # 服务端入口：接受 KCP 会话 → 拨号目标并转发
├── internal/
│   ├── protocol/       # 帧编解码、地址/CONNECT 编解码、AEAD/HKDF/nonce、AUTH 与重放缓存
│   ├── mux/            # Mux/Stream 接口 + Session（读循环/单写循环/心跳/流表）+ Bridge
│   ├── client/         # SOCKS5 服务端逻辑与单连接处理
│   ├── server/         # 会话处理、CONNECT_REQ 拨号与转发
│   ├── transport/      # KCP 拨号与监听（传输层加密开关）
│   ├── config/         # 三段式配置（common/client/server）+ 严格校验
│   └── log/            # slog 封装与字段规范
└── test/
    ├── gks.yaml.example
    ├── integration_test.go   # 端到端集成测试（进程内起 server+client+目标服务）
    ├── e2e.sh                # 一键端到端脚本（curl 走代理访问本地/外部目标）
    └── concurrent.sh         # 并发压测脚本
```

## 快速开始

```bash
# 构建
go build -o bin/server ./cmd/server
go build -o bin/client ./cmd/client

# 准备配置：示例里的 PSK 是占位值，换成真实密钥
cp test/gks.yaml.example gks.yaml
PSK="$(openssl rand -base64 32)"
sed -i.bak "s|^  psk: .*|  psk: \"$PSK\"|" gks.yaml && rm -f gks.yaml.bak

# 两个程序读同一份配置，各取自己的段落
./bin/server -c gks.yaml &
./bin/client -c gks.yaml &
```

代理就绪后（默认监听 `127.0.0.1:2080`）：

```bash
# 域名由服务端解析（推荐，本地无 DNS 泄漏）
curl --socks5-hostname 127.0.0.1:2080 http://www.baidu.com
curl --socks5-hostname 127.0.0.1:2080 https://www.baidu.com

# 本地解析模式
curl --socks5 127.0.0.1:2080 http://www.baidu.com
```

也可以一键跑完整验收（自动挑端口、生成随机 PSK、起本地目标、测 baidu 与错误码）：

```bash
./test/e2e.sh
```

客户端与服务端会输出结构化日志，关键事件：`client_start`、`proxy_up`、`proxy_down`、`server_start`、`session_up`、`dial_failed`、`session_end`。

## 配置

配置是**单个 YAML 文件，分三段**；两个程序 `-c` 同一文件，各取所需段落：

| 段 | 内容 | 为什么这样分 |
| --- | --- | --- |
| `common` | `psk`、`crypt`、`aead`、`kcp`（interval/mtu/window/FEC）、`stream.idle_timeout`、`limits`（流数上限、帧体上限、单帧 Payload 上限） | **两端必须一致**，只写一次就不存在写岔的可能 |
| `client` | `listen`（默认 `127.0.0.1:2080`）、`socks5`（握手/连接超时）、`auth`、连接池、服务端地址、心跳、日志级别 | 仅客户端使用 |
| `server` | `listen`、认证窗口与重放缓存、拨号超时、日志级别 | 仅服务端使用 |

- **严格模式**：出现未知字段直接启动失败；把 `common` 里的字段写进 `client`/`server` 同样会报错——这正是防止两处不一致的手段（有单测守着）。
- `common.psk` 必须是 base64 编码的 32 字节，且不能是全 0 占位值。
- `common.aead`：`chacha20-poly1305`（默认）或 `aes-256-gcm`。
- `common.crypt`：`none`（默认，阶段 B）或 `aes-128-gcm` 等（阶段 A，见 dev.md §0.3）。
- `client.auth.mode` 目前只支持 `none`；`userpass` 尚未实现（配置中出现会直接报错）。
- 校验用 `errors.Join`，会**一次性报出** `common` 与该端段落里的所有问题。

## 测试

```bash
go test ./...            # 全部单测与集成测试
go test -race ./...      # 含竞态检测（推荐）
go vet ./...
```

覆盖要点：

- 协议：帧 round-trip 与边界/异常、AAD 篡改检测、nonce 双向独立递增与溢出拒绝、HKDF 域分离、地址与 CONNECT 编解码、拨号错误 → REP 码映射。
- 认证：时间窗边界、重放拒绝、错误 PSK（不污染重放缓存）、proof 域分离。
- 会话与流：真实 KCP 上的握手 + 心跳判死、安全态明文帧必须断开（不降级）、双向数据、1MB 级大块分帧重组、半关闭、RST 传播、未知 StreamID 丢弃、接收缓冲长时间满 → 重置该流、会话关闭广播 RST。
- 端到端：进程内起 server + client + HTTP 目标，经 `golang.org/x/net/proxy` 走 SOCKS5 访问；IPv4 与域名（远程 DNS）目标、512KB 大body、8 并发、连接被拒与 DNS 失败的错误码。

## 已知限制

1. **每条 SOCKS5 连接一条独立 KCP Session**（阶段 3 的设计），因此还没有多路复用与连接池；阶段 4/5 会引入。
2. **服务端不发心跳**，只响应 PING（server 段配置中没有心跳字段）。保活由客户端的 `client.kcp.heartbeat_interval` 负责。
3. **优雅关闭尚不完整**：kcp-go 的 `Listener.Close()` 会关闭被所有会话共用的 UDP socket，因此收到 SIGTERM 时会立刻断开所有会话，`shutdown_grace` 实际未起到「等活跃流结束」的作用。阶段 6 会改为「先停止 Accept，期间对新会话直接拒绝，排空后再关闭监听」。
4. `userpass` 本地认证、UDP ASSOCIATE / BIND 未实现（协议预留）。
5. 接收缓冲长时间满时，当前实现是**重置该流**（保护整条会话）；更细的按流窗口与提前 RST 留待阶段 4/6 细化。
