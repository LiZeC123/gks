gks(Go-KCP-Socks5)
======================


```text
local ──SOCKS5/TCP──► gks client ──KCP/UDP（AUTH + AEAD）──► gks server ──TCP──► target
   ▲                        │                                     │
   └────────────────────────┴─────────────────────────────────────┘
```



快速开始
-------------

```bash
# 1) 构建
go build -o bin/server ./cmd/server
go build -o bin/client ./cmd/client
go build -o bin/dashboard ./cmd/dashboard   

# 2) 准备配置（示例里的 PSK 是占位值, 必须换成真实密钥）
cp test/gks.yaml.example gks.yaml
PSK="$(openssl rand -base64 32)"
sed -i.bak "s|^  psk: .*|  psk: \"$PSK\"|" gks.yaml && rm -f gks.yaml.bak

# 3) 改地址：客户端填服务端公网地址, 服务端监听所有网卡
#    client.kcp.server: "你的服务器IP:4000"
#    server.listen:     "0.0.0.0:4000"

# 4) 起服务（两个程序读同一份配置, 各取自己的段落）
./bin/server -c gks.yaml &
./bin/client -c gks.yaml &
```

> 服务端只监听UDP, 注意放行对应端口

如需在后台运行, 可搭配[lsm](https://github.com/LiZeC123/lsm)使用.


功能
---------

### 已实现

- SOCKS5 `CONNECT`（TCP）, 支持服务端域名解析
- 传输层加密与应用层加密
- 连接池, 支持链接复用, 空闲回收
- 心跳保活与优雅告别（GOAWAY）
- 独立监控面板工具


### 暂不考虑

- `UDP ASSOCIATE` 与 `BIND`
- 本地 SOCKS5 `userpass` 认证






测试验证
---------

一键验收（自动挑空闲端口、生成随机 PSK、起本地目标、测外部站点与错误码）：

```bash
./test/e2e.sh                                     # 自动挑空闲端口 + 随机 PSK + 本地目标 + 外部站点
SOCKS_PORT=2150 KCP_PORT=4070 ./test/e2e.sh       # 默认端口被占用时换端口
TARGETS="https://www.baidu.com" ./test/e2e.sh     # 只测指定目标
./test/concurrent.sh 100 http://127.0.0.1:8081/   # 并发压测（注意 ulimit -n）
```

日志只写 `*.log.file` 指定的文件（留空即丢弃）, 常用事件：`client_start`、`server_start`、
`shutdown`、`session_up`、`session_down`、`dial_failed`、`auth_failed`、`pool_session_new`、
`pool_session_dead`、`accept_error`、`pending_limit`、`client_stop`、`server_stop`、`pool_stats`。
运行状态改由控制台表格与 `GET /metrics` 呈现（见「观测与调优」）。

## 配置

单个 YAML 文件分三段, 两个程序 `-c` **同一份**文件：

| 段       | 用途                                                                                       |
| -------- | ------------------------------------------------------------------------------------------ |
| `common` | 两端必须一致的参数（PSK、加密算法、KCP 调参、FEC、心跳口径、上限、统计刷新周期）——只写一次 |
| `client` | 本地 SOCKS5 监听、连接池、服务端地址、统计端点、日志                                       |
| `server` | KCP 监听、认证窗口、拨号超时、统计端点、日志                                               |

详细配置参考[test/gks.yaml.example](test/gks.yaml.example)中的说明.



观测与调优
----------

观测有三个出口, 职责分明：**控制台表格**（人看, 默认关闭, `-console` 打开）、
**HTTP 端点**（程序拉, 始终可用）、**日志文件**（事后查, `log.file` 为空即丢弃）。
默认状态下进程不产生任何输出；与统计无关的事件日志（启动/停止/会话/错误）只写文件, 
不会和控制台表格抢屏。

### 控制台表格（`-console` 显式开启）

**默认不输出**：不加参数时进程在控制台完全静默（日志也只写文件）。加上 `-console` 后, 
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

- 终端（TTY）里**原地刷新**；输出被重定向到文件/管道时, 每个周期**追加**一份同样的纯文本表格
  （不含 ANSI 转义, 可直接留档）。
- 表格写 stdout, 只有 `-console`（client / server 都支持）才会开启；不加就是完全静默
  （配合默认的 `log.file: ""`, 进程不产生任何输出）。
- 启动初期历史不足一个窗口时, 速率与增量列显示 `n/a`, 此时会多一行提示；攒够窗口后自动消失。
- 刷新周期就是 `metrics_interval`。**输出被重定向到文件时每个周期会追加一份表格**, 周期太短会把
  日志撑大（1s 约每天 340 万行）：往文件里留档时建议调到 `10s` 以上, 或者不加 `-console`
  只保留 `/metrics` 端点（默认就是这样）。
- 分组顺序固定：会话/流 → 载荷速率 → 线速率 → 累计量 → 链路质量 → 会话池（仅客户端）→ 错误；
  末行固定为 `[告警]`（无异常显示 `[告警] 无`）。告警判据：`pool_waiters > 0`、
  `retrans > 1%`（发送段 ≥ 20 时）、`fec_errs > 0`、`kcp_in_errors > 0`。

### 统计 HTTP 端点

两端各有一个本地端点（`client.metrics.listen` / `server.metrics.listen`）, 默认关闭日志之外的
一切鉴权, 因此**默认只监听 127.0.0.1**；要对外暴露请自行加反向代理/防火墙。

| 请求                     | 说明                                                        |
| ------------------------ | ----------------------------------------------------------- |
| `GET /metrics`           | JSON 统计, 速率窗口 = `metrics_interval`                    |
| `GET /metrics?window=1s` | 指定速率窗口, 取值需在 `1s`~`10m`, 否则 `400`               |
| `GET /healthz`           | 存活探测：`{"status":"ok","role":...,"uptime_seconds":...}` |

其他路径 `404`, 非 GET/HEAD `405`。累计计数器是**请求时刻**的新鲜值, 速率与增量是该窗口两端
算出的值——因此外部程序**按 1s 拉取时用 `?window=1s` 就能拿到真正的 1s 速率**, 多个消费者
各拉各的窗口互不干扰（历史是只读的）。历史以 1s 粒度保留 10 分钟。

```bash
curl -s '127.0.0.1:12081/metrics?window=1s' | python3 -m json.tool
```

```json
{
  "schema_version": 2,
  "role": "client",
  "started_at": "2025-10-09T12:00:00+08:00",
  "uptime_seconds": 3721.5,
  "now": "2025-10-09T13:02:01.123+08:00",
  "rate_window_seconds": 1,
  "rate_available": true,
  "sessions": {"active": 2, "total": 12, "reaped_silence_total": 0, "goaway_received_total": 4},
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

- `*_total` 是进程启动至今的单调累计（`payload_*` 是隧道载荷, `wire_*` 是 UDP 线字节）；
  消费方也可以直接对这些计数器做差分, 自己算任意间隔的速率。
- `payload.*_bps` / `wire.*_bps` / `link.window.*` 是**窗口内**的速率与增量；历史不足一个完整
  窗口时 `rate_available` 为 `false`, 这些字段为 `null`。`retrans_ratio_available` 在窗口内发送段
  < 20 时为 `false`（小样本百分比会误导）。
- `pool.available`：服务端没有会话池, 固定为 `false`；客户端为 `true`。
- `alarms` 与表格末行同口径, 无异常时是空数组。

| 字段                             | 含义                                                                                                                                                                                            |
| -------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `payload.*`                      | 隧道**载荷**（应用数据）速率与累计                                                                                                                                                              |
| `wire.*`                         | **UDP 线速率**与累计；与 payload 之比即链路开销                                                                                                                                                 |
| `retrans_ratio`                  | 窗口内「重传段 / 发出的段」；持续 > 1% 说明 UDP 链路有丢包或限速                                                                                                                                |
| `lost_segs`                      | 本端 RTO 超时事件的窗口增量（同一段反复超时会重复计数, 不是"丢失的段数"）                                                                                                                       |
| `repeat_segs`                    | **对端重传过来的、本端已经收到的段数**（对端方向的丢包信号）                                                                                                                                    |
| `fec_recovered` / `fec_errs`     | FEC 成功恢复 / 恢复失败的包数；`fec_recovered > 0` 才说明 FEC 在起作用                                                                                                                          |
| `pool_*`                         | 池状态；**`waiters > 0` 说明并发已顶到 `max_sessions`**                                                                                                                                         |
| `sessions.reaped_silence_total`  | 因对端长时间沉默被**本端回收**的会话数（累计）。服务端被动判活的触发次数；持续增长说明有客户端「不告而别」（被杀、断电、分区）                                                                  |
| `sessions.goaway_received_total` | 收到的 **GOAWAY 帧数**（累计）：对端主动放弃会话（客户端回收空闲会话、服务端退出）。它是「优雅告别」生效的直接证据；服务端侧长期为 0 而 `reaped_silence_total` 增长, 说明客户端退出路径没走告别 |
| `errors.*`                       | 认证失败 / 建会话失败 / 本地 SOCKS5 失败 / 目标拨号失败（累计）                                                                                                                                 |


## 监控面板（gks-dashboard）

`cmd/dashboard` 是一个**与 gks 完全解耦**的只读面板：它不读 gks 的配置、不共享进程, 只周期拉取
`/metrics`；gks 起停随意, 拉不到数据时面板继续重试并在页面上标红。

```bash
go build -o bin/dashboard ./cmd/dashboard
./bin/dashboard                                   # 拉 127.0.0.1:12081, 监听 0.0.0.0:12080
./bin/dashboard -pull 127.0.0.1:12082 -listen 127.0.0.1:12080 -interval 2s
# 浏览器打开 http://127.0.0.1:12080/
```

页面与静态资源（HTML/CSS/JS）全部用 `go:embed` 打进二进制, **单文件即可分发**：把它拷到任何机器上
直接运行即可, 不依赖工作目录里的任何文件。

| 参数         | 默认              | 说明                                                                           |
| ------------ | ----------------- | ------------------------------------------------------------------------------ |
| `-listen`    | `0.0.0.0:12080`   | 面板 HTTP 监听地址（TCP）。与 gks 服务端的 UDP `12080` 不冲突                  |
| `-pull`      | `127.0.0.1:12081` | 上游 gks 统计端点；可写 `host:port`, 也可写完整 URL（缺路径自动补 `/metrics`） |
| `-interval`  | `1s`              | 拉取间隔                                                                       |
| `-window`    | = `-interval`     | 传给上游的速率窗口 `?window=`                                                  |
| `-timeout`   | `3s`              | 单次拉取超时                                                                   |
| `-refresh`   | `2s`              | 页面局部刷新间隔（`0` = 只渲染首屏, 不注入刷新脚本）                           |
| `-history`   | `300`             | 保留的采样点数, 也是趋势图最多画出的点数                                       |
| `-log-level` | `info`            | 日志级别（写 stderr, 只在「上游恢复/断开」等状态迁移时输出）                   |

页面内容：

- **状态条**：上游角色、运行时长、最近成功时间与陈旧度、可达徽标（不可达标红）、schema 版本。
- **累计数据**：载荷上/下行、线上/下行（UDP）、会话与流的活跃/累计、沉默判活回收与收到 GOAWAY 的
  累计次数、错误累计。
- **窗口速率**：载荷与线的实时上下行、重传率、FEC 恢复/失败。
- **派生指标**：链路开销比（线/载荷）、平均载荷速率、每会话平均流数、错误密度（每千条流）、
  沉默回收占比（沉默回收 / 累计会话, 越接近 0 越好）、FEC 恢复成功率、上游陈旧度。
- **五张折线图**（**一行两张**, 窄屏 < 1100px 自动降为单列）：① 上行（载荷 vs 线, 两条线同图看开销）
  ② 下行（同）③ 重传率 ④ 错误率（每千条流, 由面板对累计量做窗口差分）⑤ 会话回收（沉默判活 vs
  收到 GOAWAY, 同样是窗口增量；两条都贴着 0 说明会话都走了优雅路径）。SVG 由 Go 生成, 首次渲染
  即可见（无 JS 也能看数据）。
- **明细表**：链路窗口/累计段数、会话池（仅客户端）、四类错误累计；并转发上游 `alarms`。

HTTP 路由：

| 路由             | 说明                                                                                                                                                                                                       |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /`          | 完整页面（首屏服务端渲染）                                                                                                                                                                                 |
| `GET /partial`   | 只返回内容片段；页面内 JS 每 `-refresh` 替换一次, 避免整页刷新闪烁                                                                                                                                         |
| `GET /api/state` | 面板状态 + 上游最近 payload + 历史序列（JSON, `schema_version` 面板独立维护, 当前 `2`：`latest.sessions` 含沉默回收 / GOAWAY 计数, `derived.silence_reap_share`, `history[].reaped_delta`/`goaway_delta`） |
| `GET /healthz`   | 面板存活；body 里带上游可达性与陈旧度（上游挂了这里仍是 200）                                                                                                                                              |
| `GET /static/*`  | 内嵌的 css/js                                                                                                                                                                                              |

注意：面板**无鉴权**, 默认监听 `0.0.0.0`；对外暴露前请自行加防火墙或反向代理。

## 常见问题

| 现象                                                          | 排查方向                                                                                                      |
| ------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `curl` 报 `Can't complete SOCKS5 connection ... (5)`          | 目标端口拒绝连接, 属正常错误码回传                                                                            |
| `curl` 报 `(4)`                                               | 目标域名无法解析或服务端不可达                                                                                |
| 连不上代理                                                    | 客户端没起来（看日志 `client_start`）、`client.listen` 被占用或端口不匹配                                     |
| 页面很慢 / 打不开                                             | 看两端 `retrans` 与 `repeat_segs`；`pool_waiters>0` 说明并发被上限挡住；确认 UDP 端口没被限速                 |
| 服务端 `retrans` 很高（如 50%）                               | 表示“发出的段里一半是重传”, 不等于丢包率；此时链路已在承压, 先降负载（关 FEC、加大 `interval`、减少并发）再看 |
| 改了 `common` 却不生效                                        | 两端必须用同一份配置并**都重启**；`common` 与 `client`/`server` 的字段不能互换（严格模式会报错）              |
| 会话反复重建（`pool_rebuilds` 增长）                          | 链路质量差或服务端不可达；看 `session_down` 的原因                                                            |
| 启动报 `统计端点监听 127.0.0.1:12081: address already in use` | 同机跑了两个 gks（或端口被占）：把 `server.metrics.listen` 换成 `127.0.0.1:12082`, 或写 `""` 关闭端点         |
| 终端里什么都看不到                                            | 这是默认行为：控制台不输出统计（要加 `-console`）, 日志只写 `log.file`（留空即丢弃）                          |
| 启动报 `flag provided but not defined: -no-console`           | 已改为默认静默：去掉 `-no-console`, 想输出统计表格改加 `-console`                                             |
| `curl 127.0.0.1:12081/metrics` 连不上                         | 端点被写成空串关了、端口写错、或程序没起来；看日志里的 `metrics_listen` 字段                                  |
| `rate_available: false`                                       | 启动时间不足一个 `metrics_interval`, 历史还没攒够一个完整窗口；等一个周期再拉, 或把 `?window=` 调小           |
| 面板页面显示「上游不可达」                                    | gks 没在跑、`-pull` 指错端口、或端点被 `metrics.listen: ""` 关了；面板会一直重试, gks 起来后自动恢复          |
| 面板折线图只有「暂无数据」                                    | 面板刚启动（不足两个采样点）, 或重传率因窗口发送段 < 20 被上游标为 n/a；等几个周期即可                        |
| 面板端口被占                                                  | 改 `-listen`（默认 TCP 12080, 与 gks 服务端的 UDP 12080 不冲突, 但可能被别的程序占用）                        |
