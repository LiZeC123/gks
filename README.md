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
- 观测：默认**静默运行**（控制台不输出任何内容），加 `-console` 才在控制台周期刷新统计表格；
  统计 HTTP 端点（`GET /metrics` JSON + `GET /healthz`，默认 `127.0.0.1:12081`）；
  结构化日志只写配置文件指定的文件（留空则丢弃任何日志）。
- 监控面板：独立工具 `gks-dashboard`（`cmd/dashboard`）周期拉取 `/metrics`，用网页展示累计量、
  窗口速率、四张折线图与派生指标；页面与静态资源全部内嵌进二进制，可单文件分发。
- 运行：panic 隔离、SIGTERM/SIGINT 退出。

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
go build -o bin/dashboard ./cmd/dashboard   # 可选：本地监控面板（单文件，页面已内嵌）

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

两端各起一个**本地统计端点**（示例配置里客户端 `127.0.0.1:12081`、服务端 `127.0.0.1:12082`；
同机联调必须错开，两机部署可以都用 `12081`）：

```bash
curl -s 127.0.0.1:12081/metrics | python3 -m json.tool      # 客户端统计（窗口 = metrics_interval，示例 1s）
curl -s '127.0.0.1:12082/metrics?window=1s'                  # 服务端统计（1s 窗口）
./bin/client -c gks.yaml -console &                          # 想在终端看统计表格就加 -console
```

两个程序的日志默认都**不写文件**（示例配置 `log.file: ""`）；需要留档时在配置里写
`log.file: "logs/client.log"`（父目录会自动创建）。

## 常见启动方式

两个程序都读同一份配置（各取自己的段落），下面的命令在 `gks/` 目录下执行；`-console` 可选，
不加就是静默运行。**默认组合（不加 `-console` + `log.file: ""`）不会有任何控制台输出**，
统计只从 HTTP 端点拿。

```bash
# 1) 静默常驻（生产推荐）：只提供 SOCKS5 代理 + 本地统计端点，控制台与日志都无输出
./bin/server -c gks.yaml &
./bin/client -c gks.yaml &

# 2) 终端里看统计表格（本地排查时用；Ctrl-C 退出）
./bin/server -c gks.yaml -console
./bin/client -c gks.yaml -console

# 3) 静默 + 日志落文件（配置里写 log.file，父目录自动创建）
#    client.log.file: "logs/client.log"   /   server.log.file: "logs/server.log"
./bin/client -c gks.yaml &

# 4) 拉取统计（无需 -console；外部程序也可以这样周期拉）
curl -s 127.0.0.1:12081/metrics | python3 -m json.tool          # 客户端
curl -s '127.0.0.1:12082/metrics?window=1s' | python3 -m json.tool  # 服务端（1s 窗口）

# 5) 启动监控面板（网页看累计量/速率/折线图；与 gks 完全解耦，可单文件分发）
go build -o bin/dashboard ./cmd/dashboard
./bin/dashboard                                                  # 拉客户端 12081，监听 0.0.0.0:12080
./bin/dashboard -pull 127.0.0.1:12082 -listen 127.0.0.1:12080     # 改看服务端 / 只监听本机
# 浏览器打开 http://127.0.0.1:12080/

# 6) 一键端到端验收（自建临时配置与随机 PSK，不会碰你手头的 gks.yaml）
./test/e2e.sh
```

后台运行建议交给 systemd（或 `nohup`），例如：

```ini
[Unit]
Description=gks client
After=network-online.target

[Service]
WorkingDirectory=/opt/gks
ExecStart=/opt/gks/bin/client -c /opt/gks/gks.yaml
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

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

日志只写 `*.log.file` 指定的文件（留空即丢弃），常用事件：`client_start`、`server_start`、
`shutdown`、`session_up`、`session_down`、`dial_failed`、`auth_failed`、`pool_session_new`、
`pool_session_dead`、`accept_error`、`pending_limit`、`client_stop`、`server_stop`、`pool_stats`。
运行状态改由控制台表格与 `GET /metrics` 呈现（见「观测与调优」）。

## 配置

单个 YAML 文件分三段，两个程序 `-c` **同一份**文件：

| 段 | 用途 |
| --- | --- |
| `common` | 两端必须一致的参数（PSK、加密算法、KCP 调参、FEC、上限、统计刷新周期）——只写一次 |
| `client` | 本地 SOCKS5 监听、连接池、服务端地址、心跳、统计端点、日志 |
| `server` | KCP 监听、认证窗口、拨号超时、统计端点、日志 |

要点：

- **严格模式**：未知字段直接启动失败；把 `common` 的字段写进 `client`/`server` 同样报错——这样两端不可能写岔。
- 启动前会校验所有字段，问题会**一次性全部列出**（缺少必填、越界、地址不合法等）。
- `*.metrics.listen`：**不写**时默认 `127.0.0.1:12081`；显式写 `""` 表示关闭该端点。
  两个程序读同一份配置时，同机运行必须错开端口（示例里客户端 `12081`、服务端 `12082`）。
- `*.log.file`：日志只写这个文件；留空表示**不写任何日志文件**。父目录不存在会自动创建，
  打开失败即启动失败。
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
| `common.metrics_interval` | `1s` | 控制台表格刷新周期 + `/metrics` 默认速率窗口；必须 > 0 且 ≤ `10m` |
| `client.listen` | `127.0.0.1:2080` | 本地 SOCKS5 监听 |
| `client.socks5.handshake_timeout` | `10s` | 本地协商 + 请求解析 |
| `client.socks5.connect_timeout` | `10s` | 等待服务端 `CONNECT_RESP` |
| `client.pool.size` | `12` | 保底（热）会话数，1~16 |
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
| `client.metrics.listen` / `server.metrics.listen` | `127.0.0.1:12081` | 统计 HTTP 端点；不写用默认地址，写 `""` 关闭（同机需错开端口） |
| `*.log.file` | `""` | 日志文件；空 = 不写日志文件（父目录自动创建，打开失败即启动失败） |
| `*.shutdown_grace` | `30s` | 退出时等待在途连接收尾 |

## 观测与调优

观测有三个出口，职责分明：**控制台表格**（人看，默认关闭，`-console` 打开）、
**HTTP 端点**（程序拉，始终可用）、**日志文件**（事后查，`log.file` 为空即丢弃）。
默认状态下进程不产生任何输出；与统计无关的事件日志（启动/停止/会话/错误）只写文件，
不会和控制台表格抢屏。

### 控制台表格（`-console` 显式开启）

**默认不输出**：不加参数时进程在控制台完全静默（日志也只写文件）。加上 `-console` 后，
每 `common.metrics_interval` 刷新一次（示例配置为 1s）：

```text
gks client · 运行 1h02m03s · 窗口 1s · 2025-10-09 13:02:01
┌────────────┬────────────────────┬──────────────────┐
│ 会话/流    │ sessions           │ 2                │
│            │ streams            │ 1                │
│            │ sessions_total     │ 12               │
│            │ streams_total      │ 418              │
├────────────┼────────────────────┼──────────────────┤
│ 载荷速率   │ payload_sent       │ 426.0 KB/s       │
│            │ payload_recv       │ 2.3 KB/s         │
├────────────┼────────────────────┼──────────────────┤
│ 线速率     │ wire_sent          │ 2.7 MB/s         │
...
│ 链路质量   │ retrans            │ 52.54%           │
│ 会话池     │ pool_waiters       │ 0                │
│ 错误       │ dial               │ 0                │
└────────────┴────────────────────┴──────────────────┘
[告警] pool_waiters=1；retrans=52.54%(≥1%)
```

- 终端（TTY）里**原地刷新**；输出被重定向到文件/管道时，每个周期**追加**一份同样的纯文本表格
  （不含 ANSI 转义，可直接留档）。
- 表格写 stdout，只有 `-console`（client / server 都支持）才会开启；不加就是完全静默
  （配合默认的 `log.file: ""`，进程不产生任何输出）。
- 启动初期历史不足一个窗口时，速率与增量列显示 `n/a`，此时会多一行提示；攒够窗口后自动消失。
- 刷新周期就是 `metrics_interval`。**输出被重定向到文件时每个周期会追加一份表格**，周期太短会把
  日志撑大（1s 约每天 340 万行）：往文件里留档时建议调到 `10s` 以上，或者不加 `-console`
  只保留 `/metrics` 端点（默认就是这样）。
- 分组顺序固定：会话/流 → 载荷速率 → 线速率 → 累计量 → 链路质量 → 会话池（仅客户端）→ 错误；
  末行固定为 `[告警]`（无异常显示 `[告警] 无`）。告警判据：`pool_waiters > 0`、
  `retrans > 1%`（发送段 ≥ 20 时）、`fec_errs > 0`、`kcp_in_errors > 0`。

### 统计 HTTP 端点

两端各有一个本地端点（`client.metrics.listen` / `server.metrics.listen`），默认关闭日志之外的
一切鉴权，因此**默认只监听 127.0.0.1**；要对外暴露请自行加反向代理/防火墙。

| 请求 | 说明 |
| --- | --- |
| `GET /metrics` | JSON 统计，速率窗口 = `metrics_interval` |
| `GET /metrics?window=1s` | 指定速率窗口，取值需在 `1s`~`10m`，否则 `400` |
| `GET /healthz` | 存活探测：`{"status":"ok","role":...,"uptime_seconds":...}` |

其他路径 `404`，非 GET/HEAD `405`。累计计数器是**请求时刻**的新鲜值，速率与增量是该窗口两端
算出的值——因此外部程序**按 1s 拉取时用 `?window=1s` 就能拿到真正的 1s 速率**，多个消费者
各拉各的窗口互不干扰（历史是只读的）。历史以 1s 粒度保留 10 分钟。

```bash
curl -s '127.0.0.1:12081/metrics?window=1s' | python3 -m json.tool
```

```json
{
  "schema_version": 1,
  "role": "client",
  "started_at": "2025-10-09T12:00:00+08:00",
  "uptime_seconds": 3721.5,
  "now": "2025-10-09T13:02:01.123+08:00",
  "rate_window_seconds": 1,
  "rate_available": true,
  "sessions": {"active": 2, "total": 12},
  "streams": {"active": 1, "total": 418},
  "payload": {"sent_total": 28311552, "recv_total": 145408, "sent_bps": 446545.9, "recv_bps": 2354.1},
  "wire": {"sent_total": 60489728, "recv_total": 1031168, "sent_bps": 2831155.2, "recv_bps": 38912.0},
  "link": {
    "out_segs_total": 12345, "in_segs_total": 12000,
    "retrans_segs_total": 2000, "fast_retrans_segs_total": 10,
    "window": {"seconds": 1, "out_segs": 320, "retrans_ratio": 0.5254, "retrans_ratio_available": true,
               "lost_segs": 402, "repeat_segs": 7, "kcp_in_errors": 0, "fec_recovered": 35, "fec_errs": 0}
  },
  "pool": {"available": true, "in_use": 1, "idle": 1, "creating": 0, "waiters": 0, "rebuilds": 0},
  "errors": {"auth": 0, "session": 0, "socks5": 0, "dial": 0},
  "alarms": ["pool_waiters=1", "retrans=52.54%"]
}
```

- `*_total` 是进程启动至今的单调累计（`payload_*` 是隧道载荷，`wire_*` 是 UDP 线字节）；
  消费方也可以直接对这些计数器做差分，自己算任意间隔的速率。
- `payload.*_bps` / `wire.*_bps` / `link.window.*` 是**窗口内**的速率与增量；历史不足一个完整
  窗口时 `rate_available` 为 `false`，这些字段为 `null`。`retrans_ratio_available` 在窗口内发送段
  < 20 时为 `false`（小样本百分比会误导）。
- `pool.available`：服务端没有会话池，固定为 `false`；客户端为 `true`。
- `alarms` 与表格末行同口径，无异常时是空数组。

| 字段 | 含义 |
| --- | --- |
| `payload.*` | 隧道**载荷**（应用数据）速率与累计 |
| `wire.*` | **UDP 线速率**与累计；与 payload 之比即链路开销 |
| `retrans_ratio` | 窗口内「重传段 / 发出的段」；持续 > 1% 说明 UDP 链路有丢包或限速 |
| `lost_segs` | 本端 RTO 超时事件的窗口增量（同一段反复超时会重复计数，不是"丢失的段数"） |
| `repeat_segs` | **对端重传过来的、本端已经收到的段数**（对端方向的丢包信号） |
| `fec_recovered` / `fec_errs` | FEC 成功恢复 / 恢复失败的包数；`fec_recovered > 0` 才说明 FEC 在起作用 |
| `pool_*` | 池状态；**`waiters > 0` 说明并发已顶到 `max_sessions`** |
| `errors.*` | 认证失败 / 建会话失败 / 本地 SOCKS5 失败 / 目标拨号失败（累计） |

### 参考值与调参

| 场景 | 实测（loopback, MTU 1350 / window 256 / interval 10ms） |
| --- | --- |
| 100MB 下载，FEC 关 | 载荷 ≈ 214 MB/s，线/载荷 ≈ 1.02 |
| 100MB 下载，FEC 10/3 | 载荷 ≈ 161 MB/s，线/载荷 ≈ 1.35 |
| 直连 loopback 对照 | ≈ 1.78 GB/s |
| 每次新建会话 | 客户端 1 个 UDP socket + 两端各约 3.5 个 goroutine |

- **链路干净 → 关掉 FEC**（`data_shards: 0` / `parity_shards: 0`）：省掉约 30% 流量。
- **链路丢包 → 看 `fec_recovered` 与 `repeat_segs`**：恢复量远小于超时事件数说明冗余不足（10/3 → 10/6 或更重），此时也可把 `interval` 提到 20ms 减少包数。
- **首屏慢**：先把 `pool.size` 提到接近日常并发（示例给 12，日常并发更高可提到 16~24），让首屏连接都能命中热会话，避免在关键路径上做 AUTH。
- **并发被挡**：`pool_waiters > 0` 就调大 `max_sessions`；它同时是客户端 UDP socket 数量，注意 `ulimit -n`。
- 需要精确的线速率、包长分布、丢包比例时，用抓包：

```bash
sudo tcpdump -i en0 -n udp port 4000 -w kcp.pcap   # 交给 Wireshark IO Graph 或 capinfos
```

## 监控面板（gks-dashboard）

`cmd/dashboard` 是一个**与 gks 完全解耦**的只读面板：它不读 gks 的配置、不共享进程，只周期拉取
`/metrics`；gks 起停随意，拉不到数据时面板继续重试并在页面上标红。

```bash
go build -o bin/dashboard ./cmd/dashboard
./bin/dashboard                                   # 拉 127.0.0.1:12081，监听 0.0.0.0:12080
./bin/dashboard -pull 127.0.0.1:12082 -listen 127.0.0.1:12080 -interval 2s
# 浏览器打开 http://127.0.0.1:12080/
```

页面与静态资源（HTML/CSS/JS）全部用 `go:embed` 打进二进制，**单文件即可分发**：把它拷到任何机器上
直接运行即可，不依赖工作目录里的任何文件。

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-listen` | `0.0.0.0:12080` | 面板 HTTP 监听地址（TCP）。与 gks 服务端的 UDP `12080` 不冲突 |
| `-pull` | `127.0.0.1:12081` | 上游 gks 统计端点；可写 `host:port`，也可写完整 URL（缺路径自动补 `/metrics`） |
| `-interval` | `1s` | 拉取间隔 |
| `-window` | = `-interval` | 传给上游的速率窗口 `?window=` |
| `-timeout` | `3s` | 单次拉取超时 |
| `-refresh` | `2s` | 页面局部刷新间隔（`0` = 只渲染首屏，不注入刷新脚本） |
| `-history` | `300` | 保留的采样点数，也是趋势图最多画出的点数 |
| `-log-level` | `info` | 日志级别（写 stderr，只在「上游恢复/断开」等状态迁移时输出） |

页面内容：

- **状态条**：上游角色、运行时长、最近成功时间与陈旧度、可达徽标（不可达标红）、schema 版本。
- **累计数据**：载荷上/下行、线上/下行（UDP）、会话与流的活跃/累计、错误累计。
- **窗口速率**：载荷与线的实时上下行、重传率、FEC 恢复/失败。
- **派生指标**：链路开销比（线/载荷）、平均载荷速率、每会话平均流数、错误密度（每千条流）、
  FEC 恢复成功率、上游陈旧度。
- **四张折线图**：① 上行（载荷 vs 线，两条线同图看开销）② 下行（同）③ 重传率 ④ 错误率（每千条流，
  由面板对累计量做窗口差分）。SVG 由 Go 生成，首次渲染即可见（无 JS 也能看数据）。
- **明细表**：链路窗口/累计段数、会话池（仅客户端）、四类错误累计；并转发上游 `alarms`。

HTTP 路由：

| 路由 | 说明 |
| --- | --- |
| `GET /` | 完整页面（首屏服务端渲染） |
| `GET /partial` | 只返回内容片段；页面内 JS 每 `-refresh` 替换一次，避免整页刷新闪烁 |
| `GET /api/state` | 面板状态 + 上游最近 payload + 历史序列（JSON，`schema_version` 面板独立维护） |
| `GET /healthz` | 面板存活；body 里带上游可达性与陈旧度（上游挂了这里仍是 200） |
| `GET /static/*` | 内嵌的 css/js |

注意：面板**无鉴权**，默认监听 `0.0.0.0`；对外暴露前请自行加防火墙或反向代理。

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
| 启动报 `统计端点监听 127.0.0.1:12081: address already in use` | 同机跑了两个 gks（或端口被占）：把 `server.metrics.listen` 换成 `127.0.0.1:12082`，或写 `""` 关闭端点 |
| 终端里什么都看不到 | 这是默认行为：控制台不输出统计（要加 `-console`），日志只写 `log.file`（留空即丢弃） |
| 启动报 `flag provided but not defined: -no-console` | 已改为默认静默：去掉 `-no-console`，想输出统计表格改加 `-console` |
| `curl 127.0.0.1:12081/metrics` 连不上 | 端点被写成空串关了、端口写错、或程序没起来；看日志里的 `metrics_listen` 字段 |
| `rate_available: false` | 启动时间不足一个 `metrics_interval`，历史还没攒够一个完整窗口；等一个周期再拉，或把 `?window=` 调小 |
| 面板页面显示「上游不可达」 | gks 没在跑、`-pull` 指错端口、或端点被 `metrics.listen: ""` 关了；面板会一直重试，gks 起来后自动恢复 |
| 面板折线图只有「暂无数据」 | 面板刚启动（不足两个采样点），或重传率因窗口发送段 < 20 被上游标为 n/a；等几个周期即可 |
| 面板端口被占 | 改 `-listen`（默认 TCP 12080，与 gks 服务端的 UDP 12080 不冲突，但可能被别的程序占用） |

## 测试

```bash
go test ./...            # 单元测试 + 端到端集成测试
go test -race ./...      # 含竞态检测（推荐）
go vet ./...
```

覆盖：帧编解码与边界/异常、AAD 篡改检测、nonce 溢出拒绝、HKDF 域分离、时间窗与重放、
地址/CONNECT 编解码与错误码映射、真实 KCP 上的握手与心跳判死、安全态明文帧必须断开（不降级）、
大块分帧重组与半关闭、RST 传播、接收缓冲满时重置该流、会话池（预热/复用/排队/回收/重建/失效摘除/
服务端重启恢复）、5 次串行请求只建立 1 条会话、速率窗口与环形历史（1s~10m 边界、历史不足）、
表格渲染（列宽对齐、TTY/非 TTY、告警判据）、`/metrics`·`/healthz` 的 JSON 契约与参数校验、
日志文件（自动建目录、追加、级别过滤、空值丢弃）、面板（拉取成功/失败/上游恢复/schema 不匹配、
样本环裁剪与窗口差分、派生指标除零保护、SVG 生成含空数据与断点、页面与 `/api/state` 契约）。

一键验收（自动挑空闲端口、生成随机 PSK、起本地目标、验证统计端点与错误码）：

```bash
./test/e2e.sh                                     # 独立临时配置，不会碰你手头的 gks.yaml
```

## 目录结构

```text
gks/
├── cmd/
│   ├── client/         # 本地 SOCKS5 监听 → KCP 隧道
│   ├── server/         # 接受 KCP 会话 → 拨号目标并转发
│   └── dashboard/      # 独立监控面板：拉取 /metrics 并展示网页
├── internal/
│   ├── protocol/       # 帧编解码、地址/CONNECT 编解码、AEAD/HKDF/nonce、AUTH 与重放缓存
│   ├── mux/            # Session（读循环/单写循环/心跳/流表）、Stream、半关闭转发
│   ├── client/         # SOCKS5 服务端逻辑、单连接处理、会话池、CONNECT_RESP 路由
│   ├── server/         # 会话处理、CONNECT_REQ 拨号与转发
│   ├── transport/      # KCP 拨号/监听、调参、传输层加密开关、Snmp 采集
│   ├── config/         # 三段式配置与严格校验
│   ├── metrics/        # 计数器、1s 粒度速率历史、控制台表格与 HTTP JSON 呈现
│   ├── monitor/        # 采样/表格/HTTP 端点的生命周期接线
│   ├── dashboard/      # 面板：拉取器、样本环、SVG 折线图、内嵌页面
│   └── log/            # slog 封装、字段规范与日志文件管理
└── test/
    ├── gks.yaml.example
    ├── integration_test.go   # 进程内起 server + client + 目标服务
    ├── e2e.sh                # 一键端到端验收
    └── concurrent.sh         # 并发压测
```
