# gks

KCP 上的 SOCKS5 代理。客户端在本地提供标准 SOCKS5 服务，把 TCP 连接经 KCP/UDP 隧道送到服务端，
由服务端拨号真实目标并双向转发。隧道自带认证、AEAD 加密、会话复用池与速率观测。

需要 Go 1.25 或更高版本。

## 能力范围

**已实现**

- SOCKS5 `CONNECT`（TCP），兼容 `curl --socks5` / `--socks5-hostname`、浏览器、`golang.org/x/net/proxy`。
- 目标地址支持 IPv4、IPv6 与域名；用 `--socks5-hostname` 时**域名由服务端解析**，本地不产生 DNS 泄漏。
- 认证：预共享密钥 + `timestamp` + `nonce` 的 HMAC 握手，带时间窗与重放缓存，密钥由 HKDF 派生。
- 加密：`AUTH` 完成后所有帧走 AEAD（默认 ChaCha20-Poly1305），帧类型与 StreamID 不外露；可选再叠加传输层加密隐藏 KCP 头。
- 连接池：保底 `pool.size` 条已认证会话，空闲复用、上限排队、空闲回收、失效摘除、指数退避重建。
- 流语义：半关闭（FIN）、RST、未知 StreamID 丢弃、会话关闭广播 RST。
- 错误码：服务端拨号失败按 SOCKS5 REP 码回传（`5` 拒绝 / `4` 主机不可达 / `3` 网络不可达 / `6` 超时 / `1` 其他）。
- 链路：心跳保活与判死、可选 KCP 参数与 FEC、传输速率/重传/FEC/池状态指标。
- 运行：结构化日志、panic 隔离、SIGTERM/SIGINT 退出。

**未实现**

- `UDP ASSOCIATE` 与 `BIND`（回 `0x07`）。
- 本地 SOCKS5 `userpass` 认证（配置写 `userpass` 会启动失败）。
- 单条会话承载多条并发流：当前**每条会话同一时刻只跑 1 条流**，并发靠扩充会话数承担。
- 优雅排空：收到 SIGTERM 会立刻断开会话，`shutdown_grace` 只用于等待正在收尾的连接。
- 目标地址过滤（ACL）、限流、审计。

## 数据流

```text
本地应用 ──SOCKS5/TCP──► gks client ──KCP/UDP（AUTH + AEAD）──► gks server ──TCP──► 目标
   ▲                        │                                     │
   └────────────────────────┴─────────────────────────────────────┘
                    每条本地连接 = 1 条流；流承载 CONNECT_REQ/RESP + DATA + FIN/RST
```

一次 `CONNECT` 的关键路径：本地 SOCKS5 握手 → 从池取一条已认证会话 → 开流 → `CONNECT_REQ`
→ 服务端拨号 → `CONNECT_RESP` → 回本地 SOCKS5 成功 → 双向转发。

## 快速开始

```bash
# 1) 构建
go build -o bin/server ./cmd/server
go build -o bin/client ./cmd/client

# 2) 准备配置（示例里的 PSK 是占位值，必须换成真实密钥）
cp test/gks.yaml.example gks.yaml
PSK="$(openssl rand -base64 32)"
sed -i.bak "s|^  psk: .*|  psk: \"$PSK\"|" gks.yaml && rm -f gks.yaml.bak

# 3) 改地址：客户端填服务端公网地址，服务端监听所有网卡
#    client.kcp.server: "你的服务器IP:4000"
#    server.listen:     "0.0.0.0:4000"

# 4) 起服务（两个程序读同一份配置，各取自己的段落）
./bin/server -c gks.yaml &
./bin/client -c gks.yaml &
```

服务端只监听 **UDP**，记得放行该端口。默认本地代理地址是 `127.0.0.1:2080`。

## 使用方式

命令行：

```bash
curl --socks5-hostname 127.0.0.1:2080 https://www.baidu.com   # 域名交给服务端解析（推荐）
curl --socks5 127.0.0.1:2080 http://www.baidu.com             # 本地先解析
curl -x socks5h://127.0.0.1:2080 https://example.com          # 等价写法
ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:2080 %h %p' user@host
```

浏览器：代理类型选 SOCKS5，地址 `127.0.0.1`、端口 `2080`，并勾选“代理 DNS / 远程解析”。

git 之类的工具：`git config --global http.proxy socks5h://127.0.0.1:2080`。

一键验收（自动挑空闲端口、生成随机 PSK、起本地目标、测外部站点与错误码）：

```bash
./test/e2e.sh                                     # 自动挑空闲端口 + 随机 PSK + 本地目标 + 外部站点
SOCKS_PORT=2150 KCP_PORT=4070 ./test/e2e.sh       # 默认端口被占用时换端口
TARGETS="https://www.baidu.com" ./test/e2e.sh     # 只测指定目标
./test/concurrent.sh 100 http://127.0.0.1:8081/   # 并发压测（注意 ulimit -n）
```

日志是结构化的，常用事件：`client_start`、`server_start`、`proxy_up`、`proxy_down`、
`session_up`、`session_down`、`dial_failed`、`auth_failed`、`pool_session_new`、`pool_session_dead`、`metrics`。

## 配置

单个 YAML 文件分三段，两个程序 `-c` **同一份**文件：

| 段 | 用途 |
| --- | --- |
| `common` | 两端必须一致的参数（PSK、加密算法、KCP 调参、FEC、上限、采样间隔）——只写一次 |
| `client` | 本地 SOCKS5 监听、连接池、服务端地址、心跳、日志级别 |
| `server` | KCP 监听、认证窗口、拨号超时、日志级别 |

要点：

- **严格模式**：未知字段直接启动失败；把 `common` 的字段写进 `client`/`server` 同样报错——这样两端不可能写岔。
- 启动前会校验所有字段，问题会**一次性全部列出**（缺少必填、越界、地址不合法等）。
- `common.psk`：base64 编码的 32 字节，且不能是全 0 占位值。
- `common.aead`：`chacha20-poly1305`（默认）或 `aes-256-gcm`。
- `common.crypt`：`none`（默认）或 `aes-128-gcm` / `aes-256-gcm` / `aes-128` / `aes-256` / `salsa20`；开启后连 KCP 头也被加密（隐藏协议指纹），代价是每包多 28B 与一点 CPU。
- `client.auth.mode`：当前只支持 `none`。

主要字段与默认值（完整带注释的模板见 [`test/gks.yaml.example`](test/gks.yaml.example)）：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `common.kcp.interval` | `10ms` | KCP 刷新间隔（5ms~100ms）。越小延迟越低、包越多 |
| `common.kcp.mtu` | `1350` | 576~1400，避免 IP 分片 |
| `common.kcp.sndwnd` / `rcvwnd` | `256` | 32~4096 |
| `common.kcp.data_shards` / `parity_shards` | `10` / `3` | FEC；置 `0`/`0` 关闭。丢包链路才值得开 |
| `common.stream.idle_timeout` | `600s` | 流长时间无数据则 RST |
| `common.limits.max_streams_per_session` | `256` | 单会话开流上限 |
| `common.limits.max_frame_body` / `max_data_payload` | `65536` / `16384` | 帧体上限 / 单帧载荷上限（发送方据此拆帧） |
| `common.metrics_interval` | `10s` | 指标采样间隔，`0` 关闭 |
| `client.listen` | `127.0.0.1:2080` | 本地 SOCKS5 监听 |
| `client.socks5.handshake_timeout` | `10s` | 本地协商 + 请求解析 |
| `client.socks5.connect_timeout` | `10s` | 等待服务端 `CONNECT_RESP` |
| `client.pool.size` | `2` | 保底（热）会话数，1~16 |
| `client.pool.max_sessions` | `64` | **并发连接上限**，1~128；超出排队 |
| `client.pool.idle_timeout` | `300s` | 空闲会话回收（只回收超出 `size` 的部分） |
| `client.pool.startup_jitter` | `300ms` | 启动错峰 |
| `client.pool.connect_timeout` | `5s` | 排队等待可用会话的上限，超时回 SOCKS5 `0x01` |
| `client.pool.backoff_min` / `backoff_max` | `500ms` / `30s` | 会话重建退避 |
| `client.kcp.heartbeat_interval` / `heartbeat_miss` | `20s` / `3` | 心跳间隔（±25% 抖动）与判死次数 |
| `client.kcp.auth_timeout` | `5s` | 等待 `AUTH_RESP` |
| `server.auth.timestamp_window` | `60s` | 允许的时钟偏差/重放窗口 |
| `server.auth.max_pending_sessions` | `1024` | 未认证会话并发上限 |
| `server.dial.timeout` / `keepalive` | `10s` / `30s` | 拨号目标 |
| `*.shutdown_grace` | `30s` | 退出时等待在途连接收尾 |

## 观测与调优

把 `common.metrics_interval` 设为非 0（默认 10s），两端会各自周期输出一行 `event=metrics`：

```text
level=INFO msg=传输统计 event=metrics interval=10s sessions=2 streams=1 \
  payload_sent="426 KB/s" payload_recv="2.3 KB/s" wire_sent="2.7 MB/s" wire_recv="38 KB/s" \
  payload_sent_total="27.0 MB" payload_recv_total="142 KB" wire_sent_total="57.7 MB" wire_recv_total="1007 KB" \
  retrans=52.54% lost_segs=4032 repeat_segs=7 fec_recovered=35 fec_errs=0 \
  pool_in_use=1 pool_idle=1 pool_creating=0 pool_waiters=0 pool_rebuilds=0 \
  errors="auth=0 session=0 socks5=0 dial=0"
```

| 字段 | 含义 |
| --- | --- |
| `payload_sent` / `payload_recv` | 隧道**载荷**速率（应用数据），实时累加 |
| `wire_sent` / `wire_recv` | **UDP 线速率**；与 payload 之比即链路开销 |
| `payload_*_total` / `wire_*_total` | 进程启动至今的累计量；客户端 `payload_recv_total` 应约等于服务端 `payload_sent_total` |
| `retrans` | 本区间「重传段 / 发出的段」；发送段 < 20 时显示 `n/a` |
| `lost_segs` | 本端 RTO 超时事件的增量（同一段反复超时会重复计数，不是“丢失的段数”） |
| `repeat_segs` | **对端重传过来的、本端已经收到的段数**（对端方向的丢包信号） |
| `fec_recovered` / `fec_errs` | FEC 成功恢复 / 恢复失败的包数；`fec_recovered > 0` 才说明 FEC 在起作用 |
| `pool_in_use` / `pool_idle` / `pool_creating` / `pool_waiters` / `pool_rebuilds` | 池状态；**`pool_waiters > 0` 说明并发已顶到 `max_sessions`** |
| `errors` | 认证失败 / 建会话失败 / 本地 SOCKS5 失败 / 目标拨号失败 |

参考值与调参：

| 场景 | 实测（loopback, MTU 1350 / window 256 / interval 10ms） |
| --- | --- |
| 100MB 下载，FEC 关 | 载荷 ≈ 214 MB/s，线/载荷 ≈ 1.02 |
| 100MB 下载，FEC 10/3 | 载荷 ≈ 161 MB/s，线/载荷 ≈ 1.35 |
| 直连 loopback 对照 | ≈ 1.78 GB/s |
| 每次新建会话 | 客户端 1 个 UDP socket + 两端各约 3.5 个 goroutine |

- **链路干净 → 关掉 FEC**（`data_shards: 0` / `parity_shards: 0`）：省掉约 30% 流量。
- **链路丢包 → 看 `fec_recovered` 与 `repeat_segs`**：恢复量远小于超时事件数说明冗余不足（10/3 → 10/6 或更重），此时也可把 `interval` 提到 20ms 减少包数。
- **首屏慢**：先把 `pool.size` 提到接近日常并发（如 16~24），让首屏连接都能命中热会话，避免在关键路径上做 AUTH。
- **并发被挡**：`pool_waiters > 0` 就调大 `max_sessions`；它同时是客户端 UDP socket 数量，注意 `ulimit -n`。
- 需要精确的线速率、包长分布、丢包比例时，用抓包：

```bash
sudo tcpdump -i en0 -n udp port 4000 -w kcp.pcap   # 交给 Wireshark IO Graph 或 capinfos
```

## 常见问题

| 现象 | 排查方向 |
| --- | --- |
| `curl` 报 `Can't complete SOCKS5 connection ... (5)` | 目标端口拒绝连接，属正常错误码回传 |
| `curl` 报 `(4)` | 目标域名无法解析或服务端不可达 |
| 连不上代理 | 客户端没起来（看日志 `client_start`）、`client.listen` 被占用或端口不匹配 |
| 页面很慢 / 打不开 | 看两端 `retrans` 与 `repeat_segs`；`pool_waiters>0` 说明并发被上限挡住；确认 UDP 端口没被限速 |
| 服务端 `retrans` 很高（如 50%） | 表示“发出的段里一半是重传”，不等于丢包率；此时链路已在承压，先降负载（关 FEC、加大 `interval`、减少并发）再看 |
| 改了 `common` 却不生效 | 两端必须用同一份配置并**都重启**；`common` 与 `client`/`server` 的字段不能互换（严格模式会报错） |
| 会话反复重建（`pool_rebuilds` 增长） | 链路质量差或服务端不可达；看 `session_down` 的原因 |

## 测试

```bash
go test ./...            # 单元测试 + 端到端集成测试
go test -race ./...      # 含竞态检测（推荐）
go vet ./...
```

覆盖：帧编解码与边界/异常、AAD 篡改检测、nonce 溢出拒绝、HKDF 域分离、时间窗与重放、
地址/CONNECT 编解码与错误码映射、真实 KCP 上的握手与心跳判死、安全态明文帧必须断开（不降级）、
大块分帧重组与半关闭、RST 传播、接收缓冲满时重置该流、会话池（预热/复用/排队/回收/重建/失效摘除/
服务端重启恢复）、5 次串行请求只建立 1 条会话、指标计算与日志字段。

## 目录结构

```text
gks/
├── cmd/
│   ├── client/         # 本地 SOCKS5 监听 → KCP 隧道
│   └── server/         # 接受 KCP 会话 → 拨号目标并转发
├── internal/
│   ├── protocol/       # 帧编解码、地址/CONNECT 编解码、AEAD/HKDF/nonce、AUTH 与重放缓存
│   ├── mux/            # Session（读循环/单写循环/心跳/流表）、Stream、半关闭转发
│   ├── client/         # SOCKS5 服务端逻辑、单连接处理、会话池、CONNECT_RESP 路由
│   ├── server/         # 会话处理、CONNECT_REQ 拨号与转发
│   ├── transport/      # KCP 拨号/监听、调参、传输层加密开关、Snmp 采集
│   ├── config/         # 三段式配置与严格校验
│   ├── metrics/        # 进程级计数器与周期采样
│   └── log/            # slog 封装与字段规范
└── test/
    ├── gks.yaml.example
    ├── integration_test.go   # 进程内起 server + client + 目标服务
    ├── e2e.sh                # 一键端到端验收
    └── concurrent.sh         # 并发压测
```
