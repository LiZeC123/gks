# 基于 KCP 的 SOCKS5 代理系统 gks —— 技术方案文档

> 目标读者：AI 实现者。
> 设计目标：**最强稳定可用**，支持多路复用与连接池，暂不考虑限流与用户层防御。
> 实现语言：Go。
> 文档版本：**v2（定稿版）**。初版中被讨论、修订、细化的内容已并入正文，逐条对照见 [§15 变更记录](#15-变更记录与初版对照)；尚未实现项与明确不做项见 [§16 待办与不做](#16-待办与不做)。

---





## TODO

1. 传输层启用加密, 可以保证包含kcp头部在内的所有信息加密, 应用层逻辑上来说无需加密
   1. 传输层本身包含序列号机制, 可以防止重放攻击, 重复包会直接丢弃, 无法解密的包也会直接丢弃, 隐蔽性更好
   2. 应用层只防止了认证阶段的重放攻击, 但是这并没有什么意义, 抓数据包重放会直接解析并处理, 这没有解决重放的问题. 还可能导致链路被干扰. 所以必须依赖运输层的序号机制
   3. 应用层的帧都分配的递增的nonce, 因此直接重放已有的包会因为nonce不一样而无法解密, 
2. 去掉应用层的验证环节可以减少1RTT, 由于当前网络的RTT是秒级别的, 因此对于启动速度有很大帮助
   1. 应用层的加密有握手环节, 相当于每个链接都随机的改变了密钥, 因此对于nonce冲突有更大的抵抗, 不太需要考虑冲突问题了.
   2. 相关代码已经嵌入到逻辑里面, 也很难再修改了
3. 理论上可以复用底层UDP链接, 也有一个叫做smux的库, 但是这个似乎没有太大收益
   1. 收益是看起来只有一个UDP链接, 所有请求都从这里走, 资源开销小, 但是这有可能隐蔽特征, 也有可能加强了特征
   2. 现在的Steam其实是无法有效实现半关闭逻辑的, 底层就不支持, 所以自己实现没有什么更多的收益
4. KCP-GO如果遇到底层发送问题, 并不能中断已经在缓冲区的数据, 会不断重置, 直到上层的心跳机制超时关闭了这个链接, 这大概需要1分钟的时间, 期间会按照RTO无限次重发数据, 可能会较大程度拉高CPU
5. 空载的时候CPU消耗太多了, 相比另外一个项目几乎没有CPU消耗, 这还是空转的东西太多,
6. KCP提供的是流模型, TCP和SOCKS5本来也是流模型, 现在手动拆包没有什么必要, 导致开销比较高.
   1. 但是为了发送控制信号, 似乎也不能直接无脑转发, 还是得封装成帧, 然后按帧处理









## 0. 文档约定

### 0.1 术语

- **Client**：本地客户端，对外提供 SOCKS5 服务。
- **Server**：远端服务端，负责实际拨号与转发。
- **Stream**：一条逻辑上的 TCP 连接，对应一次 SOCKS5 CONNECT。
- **Session**：一条 KCP 会话，可承载多个 Stream（1 个 KCP conv ↔ 1 个 Session）。
- **Pool**：KCP 会话池，复用 Session 以减少握手开销。
- **握手态 / 安全态**：Session 的两个编码阶段。AUTH 完成前为握手态（帧体明文），AUTH 成功后为安全态（帧体 AEAD 密文）。
- **proto v1**：本文档定义的帧协议版本（`Ver = 0x01`）。

### 0.2 范围

- 支持 SOCKS5 `CONNECT`（TCP）。
- `UDP ASSOCIATE` 与 `BIND` 不实现，仅在帧类型表中预留。
- 不做限流、黑名单、审计等生产级防御。仅保留一项与稳定性相关的保护：未认证 Session 数量上限（见 §5.2），不属于用户层防御。
- 本地 SOCKS5 认证：**只实现 `none`**；`userpass`（RFC1929）留 TODO，配置字段保留但校验时拒绝非空 users。
- 必须做认证与加密，避免裸奔；不做明文传输。

### 0.3 加密分层（分两步落地）

- **阶段 B（先做，默认形态）**：仅应用层 AEAD + AUTH 握手，KCP 层不加密（`common.crypt: none`）。
  - 代价：KCP 头（conv/cmd/sn/una/ts/wnd）在线上明文，理论上可被定向篡改（干扰窗口、RTT），也会留下 KCP 协议指纹。这是本阶段已知且接受的取舍。
- **阶段 A（后做）**：在阶段 B 基础上开启 kcp-go 自带 crypt（**推荐 `aes-128-gcm`**），隐藏 KCP/FEC 头与协议指纹。
  - 关键事实（已核对 kcp-go **v5.6.72** 源码）：kcp-go 提供两类传输层加密——① **AEAD 模式**（`NewAESGCMCrypt`/`NewAEADCrypt`）：每包 12B 随机 nonce + GCM tag，无 CRC32；② **传统 CFB 模式**（AES/Salsa20/SM4 等）：每包 16B 随机 nonce + CRC32。两者都加密**整包**，覆盖 FEC 头、KCP 头与载荷。
  - 真正的限制不在于用哪种模式，而在于 **kcp-go 没有密钥交换**：密钥直接来自静态 PSK，因此它**不提供身份认证，也不防重放**（README 明确要求上层自行认证，并说明「即使上层已加密也建议开启」）。
  - 因此两者职责不重叠、不可互相替代：crypt 保护传输层头，应用层 AEAD + AUTH 负责身份、密钥派生、帧级完整性与防重放。
- 帧格式本身**按安全态的形态设计一次到位**，阶段 A 只是多套一层传输层加密，不改变帧协议（见 §3.2）。

---

## 1. 实现目标

### 1.1 功能目标

1. Client 在本地监听 TCP，作为 SOCKS5 服务端，兼容 `curl --socks5-hostname`、浏览器、`golang.org/x/net/proxy` 等标准客户端。
2. Client 完成 SOCKS5 握手、认证、请求解析后，将请求封装为自定义帧，经 KCP 发送到 Server。
3. Server 解析帧，校验认证，拨号目标，双向转发。
4. 支持多路复用：单 KCP Session 承载多条 Stream。
5. 支持连接池：Client 维护多个 KCP Session，按负载分配 Stream。
6. 支持 TCP 半关闭、超时、错误码回传、心跳保活。
7. 支持认证与加密：预共享 Token + HMAC 证明完成身份认证，AEAD（默认 ChaCha20-Poly1305）保障帧级机密性与完整性，单调 nonce 计数器与时间窗 + nonce 去重共同防重放。
8. 降低可观测指纹：心跳间隔抖动、池启动错峰、安全态下帧类型与 StreamID 不外露（见 §3.2、§4.4）。

### 1.2 非功能目标

- **稳定性优先**：任何单点错误不得导致进程崩溃或 goroutine 泄漏。
- **可观测**：日志分级（结构化字段），关键指标（连接数、Session 数、Stream 数、字节数、错误数）。
- **可测试**：核心模块可单测；端到端可用脚本验证；丢包场景可用可控的 mock PacketConn 复现（见 §9.5）。
- **可配置**：全部参数通过配置文件或命令行传入，启动即校验。

### 1.3 非目标

- 不做用户级限流、QoS、黑名单。
- 不做 UDP ASSOCIATE、BIND。
- 不做多用户计费、审计；不支持多 token（单 PSK 单 token）。
- 不做复杂调度算法，连接池采用简单策略即可。
- 不做流量伪装层（伪装成 QUIC/TLS 等常见 UDP 形态）、不做帧长填充（见 §16）。
- 不做本地 SOCKS5 `userpass` 认证（本版）。

---

## 2. 总体架构

```text
┌─────────────┐   SOCKS5/TCP   ┌──────────────┐   KCP/UDP   ┌──────────────┐   TCP   ┌──────────┐
│ 本地应用    │ ─────────────► │  Client      │ ──────────► │  Server      │ ──────► │ 目标服务 │
│ (curl/浏览器)│ ◄───────────── │  (本地监听)  │ ◄────────── │  (远端监听)  │ ◄────── │          │
└─────────────┘                └──────────────┘             └──────────────┘         └──────────┘
```

### 2.1 模块划分

**Client**

- `socks5`：SOCKS5 握手、认证（仅 none）、请求解析、响应编码。
- `pool`：KCP Session 池，管理 Session 生命周期与调度。
- `session`：单条 KCP 会话，封装多路复用读写。
- `stream`：单条逻辑流，读写接口。
- `protocol`：帧编解码、认证、加密。
- `config`：配置加载与校验。

**Server**

- `listener`：KCP Listener，接受 Session。
- `session`：单条 KCP 会话，解复用。
- `stream`：单条逻辑流，负责拨号与转发。
- `protocol`：帧编解码、认证、解密。
- `acl`：可选，目标地址过滤（预留，本版不实现）。
- `config`：配置加载。

**公共**

- `protocol/frame`：帧格式定义与编解码（握手态/安全态）。
- `protocol/crypto`：AEAD 封装、HKDF 密钥派生、nonce 计数器。
- `protocol/auth`：AUTH 请求/应答构造与校验、HMAC 证明、时间窗、重放缓存。
- `mux`：`Mux`/`Stream` 接口定义 + 自研实现（接口先行，便于日后替换 `xtaci/smux`）。
- `transport`：KCP 拨号/监听封装，含 `common.crypt` 开关与测试用 mock PacketConn。
- `log`：统一日志（`log/slog`）。

---

## 3. 协议设计

### 3.1 分层

```text
┌──────────────────────────────┐
│ SOCKS5 (仅 Client 侧)         │
├──────────────────────────────┤
│ 自定义帧协议（Frame）          │
├──────────────────────────────┤
│ 多路复用（Stream over Session）│
├──────────────────────────────┤
│ KCP（可靠 UDP）                │
├──────────────────────────────┤
│ [阶段 A] kcp-go crypt（可选）  │
├──────────────────────────────┤
│ UDP                            │
└──────────────────────────────┘
```

### 3.2 帧格式（proto v1，定稿）

每个帧 = 明文头（8B，同时作为 AEAD 的 AAD）+ 帧体（Body，Length 字节）。

```text
+--------+--------+--------+----------------+---------------------------+
| Magic  | Ver    | Flags  | Length         | Body                      |
| 2B     | 1B     | 1B     | 4B (BE)        | Length 字节                |
+--------+--------+--------+----------------+---------------------------+
   └──────────── 明文 8B，作为 AAD ────────┘

Body 逻辑结构:  Type(1B) | StreamID(4B, BE) | Payload(N)
```

- **Magic**：`0x4B 0x43`（"KC"），用于快速丢弃错误帧。保留明文（首帧必然明文，无法隐藏）。
- **Ver**：`0x01`。非本版本直接丢帧并断开。
- **Flags**：v1 **必须为 0**；收到非 0 值直接丢帧并断开（避免 AAD 语义歧义，为将来协商留空间）。
- **Length**：Body 长度，大端。`Length > common.limits.max_frame_body`（默认 65536）时**立即断开，不等待字节**（防伪造长度导致的挂起）。
- **StreamID**：大端，`0` 表示 Session 级控制帧；**Client 主动分配的流使用奇数**，偶数留给将来 Server 主动。

**Body 的两种编码态**

| 状态   | 适用帧              | Body 编码                                                      | Length       |
| ------ | ------------------- | -------------------------------------------------------------- | ------------ |
| 握手态 | AUTH_REQ、AUTH_RESP | 明文 `Type\|StreamID\|Payload`                                 | `5 + N`      |
| 安全态 | 其余全部帧          | `AEAD.Seal(key, nonce, Type\|StreamID\|Payload, aad=8B明文头)` | `5 + N + 16` |

- 安全态密文体长度为 `5 + N + tagLen`（ChaCha20-Poly1305 与 AES-256-GCM 的 tag 均为 16B）。
- **不降级**：安全态下解密失败（含 tag 校验失败）一律丢弃该帧并**断开 Session**，绝不回退为明文解析；握手态下收到非 AUTH_REQ / 非 AUTH_RESP 的帧同样直接断开。
- 明文头参与 AAD，使攻击者无法篡改 Magic / Ver / Flags / Length 而不被发现（解密时用收到的明文头原样重建 AAD）。
- 安全态下 **Type 与 StreamID 不再外露**（满足指纹收敛目标）；`Magic` 与 `Length` 的可见性无法避免。

**长度上限与分片**

- `common.limits.max_frame_body` 默认 65536（4B 长度字段的实际上限由实现收紧）。
- DATA 帧单帧 Payload 上限 `common.limits.max_data_payload` 默认 16384；上层一次写入超过该值时拆为多个帧，避免单帧过大引发行级队头阻塞与内存峰值。
- 帧头固定 8B + Body 前缀 5B + tag 16B = 每帧 29B 开销（安全态）。

### 3.3 帧类型

| Type | 名称         | 方向 | 编码态 | 说明                                          |
| ---- | ------------ | ---- | ------ | --------------------------------------------- |
| 0x01 | AUTH_REQ     | C→S  | 握手态 | 携带 token_id、nonce_c、timestamp、HMAC proof |
| 0x02 | AUTH_RESP    | S→C  | 握手态 | 认证结果 + nonce_s                            |
| 0x03 | CONNECT_REQ  | C→S  | 安全态 | 目标地址、端口                                |
| 0x04 | CONNECT_RESP | S→C  | 安全态 | 成功/失败、错误码、BND                        |
| 0x05 | DATA         | 双向 | 安全态 | 流数据                                        |
| 0x06 | FIN          | 双向 | 安全态 | 半关闭（本方向不再发 DATA）                   |
| 0x07 | RST          | 双向 | 安全态 | 强制关闭双向，丢弃缓冲                        |
| 0x08 | PING         | 双向 | 安全态 | 心跳                                          |
| 0x09 | PONG         | 双向 | 安全态 | 心跳响应                                      |
| 0x0A | GOAWAY       | S→C  | 安全态 | Session 即将关闭，不再接受新 Stream           |
| 0x0B | ERROR        | 双向 | 安全态 | 通用错误（带错误码与简短描述）                |

- Session 级控制帧（AUTH_*、PING、PONG、GOAWAY）StreamID 一律为 0。
- PONG 必须回显 PING 的 Payload（8B 随机数），防止乱序环境下误判。

### 3.4 认证（定稿）

预共享密钥 **PSK：32 字节**（配置中为 base64）。PSK **永不在线路上传输**。

**握手流程**

```text
1) Client 生成 nonce_c(16B 随机)、取当前 timestamp(8B BE, Unix 秒)
   proof = HMAC-SHA256(PSK, "gks-auth-v1" || token_id(2B BE) || nonce_c(16B) || timestamp(8B BE))
   → AUTH_REQ{ token_id, timestamp, nonce_c, proof }        （握手态，明文 Body）

2) Server 校验（顺序：token_id → 时间窗 → HMAC proof → 重放缓存）：
   a. token_id 必须等于已配置值（本版固定 0x0001）
   b. |now - timestamp| <= timestamp_window（默认 60s）
   c. hmac.Equal(HMAC 重算值, proof)（常量时间比较）
   d. nonce_c 未在重放缓存中出现过
   说明：把重放检查放在 HMAC 之后，是为了避免未通过 proof 的请求写入重放缓存（缓存投毒）。
   全部通过 → 生成 nonce_s(16B 随机)，AUTH_RESP{ code=0x00, nonce_s }
   任一失败 → AUTH_RESP{ code=错误码 }，随后关闭 Session

3) 双方派生会话密钥（HKDF-SHA256）：
   salt    = nonce_c || nonce_s
   key_c2s = HKDF-SHA256(PSK, salt, "gks-c2s-v1", 32B)
   key_s2c = HKDF-SHA256(PSK, salt, "gks-s2c-v1", 32B)
   之后全部帧进入安全态。
```

**AUTH_REQ Payload 布局（明文，共 58B）**

```text
token_id(2B BE) | timestamp(8B BE) | nonce_c(16B) | proof(32B)
```

**AUTH_RESP Payload 布局**

```text
code(1B) | nonce_s(16B，仅 code=0x00 时存在)
code: 0x00 成功 / 0x01 token 错误 / 0x02 时间窗超限 / 0x03 重放 / 0x04 其他
```

**防重放与时间**

- 重放缓存：键 `(token_id, nonce_c)`，TTL = `2 × timestamp_window`，容量 `replay_cache_size`（默认 65536），LRU 淘汰最旧。
- `timestamp` 只用于缩小重放窗口，**正确性由 nonce 缓存保证**；因此对两端时钟同步只有宽松要求（偏差需小于 `timestamp_window`）。
- **Session 密钥每会话唯一**：`nonce_s` 由 Server 随机提供，保证任何一方都无法独自决定会话密钥（即便 `nonce_c` 被预测）。

**认证超时与并发保护**

- Server 等待 AUTH_REQ 超时 `auth_timeout`（默认 5s）→ 关闭。
- Client 等待 AUTH_RESP 超时 `auth_timeout`（默认 5s）→ 关闭并重建 Session。
- 未认证 Session 并发上限 `max_pending_sessions`（默认 1024，属于稳定性保护而非用户层防御），超出时新连接直接丢弃并计数。

### 3.5 加密（定稿）

- 算法：**默认 ChaCha20-Poly1305**（纯软件实现快、无硬件依赖）；可配置 `aes-256-gcm`。通过配置项 `aead` 选择。
- 每帧独立封装：

```text
ciphertext = AEAD.Seal(key_dir, nonce_dir, plaintext=Type|StreamID|Payload, aad=8B明文头)
plaintext  = AEAD.Open(key_dir, nonce_dir, ciphertext, aad=8B明文头)   // 失败即断开
```

- **方向独立密钥**：`key_c2s` / `key_s2c` 各一把，这是消除 nonce 冲突的根本手段。
- **nonce 结构（12B）**：`0x00000000(4B) || counter(8B BE)`，每个方向各自从 0 开始单调递增。
- **nonce 复用防护**：同一密钥下计数器绝不重复；计数器达到 `2^64-1` 时记录 error 日志并**主动断开 Session**（禁止回绕）。
- **计数器必须与发送方同步前进**：即使某一帧解密/校验失败，接收方的计数器也要前进（发送方每发一帧就自增一次），否则后续帧 nonce 会整体错位。
- 帧头参与 AAD：`Magic|Ver|Flags|Length` 共 8B。
- **不降级**：见 §3.2。
- 握手态帧（AUTH_REQ/AUTH_RESP）不加密；其后的任何帧都必须解密成功才被处理。
- 阶段 A 的传输层加密：`common.crypt ∈ {none, aes-128-gcm, aes-256-gcm, aes-128, aes-256, salsa20}`（推荐 `aes-128-gcm`）；开启时密钥由 `HKDF-SHA256(PSK, "", "gks-kcp-<algo>-v1")` 派生到对应长度（**不得直接截断 PSK**）。kcp-go 的 crypt 没有密钥交换、不提供认证，故应用层 AEAD 无论如何都保留。

### 3.6 多路复用

- 单 KCP Session 内，每个 Stream 用 4 字节 StreamID 标识（0 = 控制帧）。
- **StreamID 分配规则（定稿）**：Client 侧在 Session 内**单调递增分配奇数**（1,3,5,…），**不回收、不复用**；Session 重建时从 1 重新开始。理由：彻底消除「旧帧迟到串入新流」的问题；4B 空间对单会话生命周期足够（配合 `common.limits.max_streams_per_session` 上限）。
- 达到 `common.limits.max_streams_per_session`（默认 256）或 StreamID 超过 `0x7FFFFFFF` → 该 Session 不再接受新流，由池新建 Session。
- 每个 Stream 维护：
  - 接收缓冲（带背压，见 §3.7）
  - 发送队列（容量上限 256KB）
  - 状态机（见 §3.8）
- 一个 Session 内有一个 `mux` 组件：
  - **读循环**：从 KCP 读帧（8B 头 → Length 字节体），解密，按 StreamID 分发；控制帧由 mux 直接处理。
  - **写循环**：从各 Stream 发送队列聚合，**串行写 KCP**，避免并发写（单写原则）。
  - 写循环采用「批量聚合 + 短等待」：优先一次性取空当前可用的发送队列，减少小包与 syscall 次数。
  - 写队列满时（超过 `write_queue_high_watermark`，默认 1024 帧）对新帧施加背压，而非无限堆积。

**接口先行（定稿）**：`mux` 包先定义接口，自研实现之，便于日后替换为 `xtaci/smux`：

```go
// Mux：一条 Session 上的多路复用器
type Mux interface {
    OpenStream(ctx context.Context) (Stream, error) // Client 主动开流；Server 侧返回错误
    AcceptStream(ctx context.Context) (Stream, error) // Server 侧接受流
    NumStreams() int
    Close() error
    CloseChan() <-chan struct{}
}

// Stream：一条逻辑流，语义对齐 net.Conn + 半关闭
type Stream interface {
    io.ReadWriteCloser
    StreamID() uint32
    CloseWrite() error   // 发送 FIN（半关闭本方向）
    CloseRead() error    // 丢弃接收方向（对端仍可发送，仅本地不再读）
    Reset() error        // 发送 RST，立即双向关闭
}
```

> 注意：`xtaci/smux` 的半关闭语义不完整，仅作为 Ctrl 语义降级时的备选（把 CloseWrite 降级为 RST 全关）。接口预留是为降低替换成本，不承诺等价语义。

### 3.7 流控与背压（定稿数值）

- 每 Stream 发送缓冲上限 `256KB`；**发送缓冲满时阻塞上层写入**（即阻塞从 SOCKS5 连接读取），不得无限堆积。
- 接收侧：Session 读循环不得被单个 Stream 阻塞。实现为「读循环解密分发到 Stream 的带缓冲 channel，缓冲满则该 Stream 背压到读循环」——为避免全局卡死，规定：
  - 每 Stream 接收缓冲上限 `256KB`；
  - 若某 Stream 接收缓冲满且下层长时间不消费，超过 `stream_idle_timeout` 或累计超出阈值时，向该 Stream 发送 RST 并清理（保护整条 Session 不被单流拖死）。
- 单帧 Payload 上限 16KB（见 §3.2），单次写入更大数据由上层拆帧。

### 3.8 半关闭与关闭语义（状态机定稿）

状态：`open` → `localClosed` / `remoteClosed` → `closed`

| 当前状态     | 事件                   | 动作                                  | 新状态       |
| ------------ | ---------------------- | ------------------------------------- | ------------ |
| open         | 本地读端 EOF           | 发送 FIN，禁止再发 DATA               | localClosed  |
| open         | 收到 FIN               | 关闭本地读方向（半关闭），仍可发 DATA | remoteClosed |
| localClosed  | 收到 FIN               | 关闭本地读方向，释放资源              | closed       |
| remoteClosed | 本地读端 EOF           | 发送 FIN，释放资源                    | closed       |
| 任意         | 收到 RST               | 立即丢弃缓冲、关闭双向、释放资源      | closed       |
| 任意         | Session 关闭           | 等同 RST（Client 关闭对应本地连接）   | closed       |
| open/…       | 收到 FIN 后又收到 DATA | 协议违规：丢弃该帧、计数、不断开      | 不变         |
| closed       | 任何帧                 | 丢弃（不回 RST，避免风暴）            | closed       |

- `FIN`：发送方不再发送 DATA，但可继续接收。
- `RST`：立即关闭双向，丢弃缓冲。
- Session 关闭：向所有 Stream 广播 `RST`，Client 关闭对应本地连接。
- 本地 SOCKS5 侧半关闭：Client 收到远端 FIN → 对本地 TCP 连接调用 `CloseWrite()`；本地读端 EOF → 发送 FIN。

### 3.9 会话级控制语义

- **PING/PONG**：任意一方可发起；收到 PING 立即回 PONG（回显 8B payload）。心跳间隔见 §4.4。
- **GOAWAY**：Server 决定关闭 Session 时先发 GOAWAY（可选携带原因码），Client 收到后不再在该 Session 上开新流，已存在的流继续跑完或由 Server 广播 RST。
- **ERROR**：通用错误，带 `code(1B) + 描述(变长 UTF-8, ≤255B)`；仅用于诊断，不替代 RST/FIN 的关闭语义。

---

## 4. Client 设计

### 4.1 启动流程

1. 加载并校验配置（同一 YAML 文件，取 `client:` 段）。
2. 初始化 KCP Session 池（错峰建立，见 §4.3）。
3. 监听本地 TCP 端口（SOCKS5）。
4. 每个本地连接启动一个 goroutine（带 recover 隔离）。
5. 监听 SIGTERM/SIGINT：停止接受新连接，等待活跃流结束或超时。

### 4.2 单连接处理流程

1. SOCKS5 握手，选择认证方式：仅支持 `0x00 无认证`；客户端提供 `0x02` 时回复「无可接受方法」并关闭。
2. 解析请求，仅接受 `CMD=CONNECT`；`UDP ASSOCIATE`/`BIND` 回复 `0x07 Command not supported`。
3. 从池中获取一个可用 Session（见 §4.3），分配 StreamID。
4. 发送 `CONNECT_REQ`。
5. 等待 `CONNECT_RESP`（超时 `client.socks5.connect_timeout`，默认 10s）：
   - 成功：回复 SOCKS5 成功，进入转发。
   - 失败：映射错误码（§5.3），回复 SOCKS5 失败，关闭。
6. 转发循环：
   - 本地读 → 封装 DATA → 写 Stream（超过 16KB 拆帧）。
   - Stream 读 → 写本地。
   - 任一端 EOF → 发送 FIN，等待对端 FIN，再关闭（状态机见 §3.8）。
7. 清理：Stream 关闭；Session 归还池；StreamID 不复用（见 §3.6）。

### 4.3 Session 池（定稿细节）

> **实现状态（阶段 5 的保守形态）**：当前池采用「每条 Session 同一时刻最多 1 条活跃流」的**串行复用**——
> 不需要流间公平调度，也不会出现「一条慢流拖死整条会话」；并发压力通过扩充 Session 数（至 `max_sessions`）承担。
> 因此本节里「按 Stream 数最少选择」在此形态下退化成「取一条空闲 Session」；§3.6 的单 Session 多流
> 仍未启用，等启用真正的 mux 后，选择策略与流控按原设计生效。


- 池维护 `size` 个活跃 Session（默认 2，范围 1..16）；`max_sessions` 为硬上限（范围 1..128，`max_sessions ≥ size`）。在保守形态下它就是**并发连接上限**，实测浏览型负载峰值并发可达 20+，因此示例配置取 64。
- **启动错峰**：池初始化时，相邻 Session 的建立间隔随机 `startup_jitter`（默认 100ms~500ms），避免启动瞬间 N 个握手同时发出（指纹与瞬时负载考虑）。
- 新 Stream 分配：优先选择 **Stream 数最少**的可用 Session；并列时随机选择。
- 池的 Session 全部达到 `common.limits.max_streams_per_session` 或均已失效：
  - 若活跃 Session 数 < `max_sessions` → 新建；
  - 否则**排队等待**可用 Session，等待上限 `pool.connect_timeout`（默认 5s）；超时则该 SOCKS5 请求以 `0x01 general failure` 失败。
- Session 断开时：
  - 从池移除；
  - 其上所有 Stream 收到 RST（Client 侧关闭对应本地连接）；
  - 触发重建，**指数退避 0.5s → 最大 30s，带 ±20% 抖动**；重建失败只记日志，不影响进程。
- Session 空闲（无 Stream）超过 `idle_timeout`（默认 300s）→ 关闭回收，但保证池内活跃 Session 数不低于 `size`。
- 池为空时，新建 Session（同步等待，受 `pool.connect_timeout` 约束）。

### 4.4 心跳（定稿）

- 每个 Session 发送 `PING`，间隔在 `[0.75×, 1.25×]` 区间内抖动，默认 `heartbeat_interval = 20s` → 实际 15s~25s 随机，避免形成周期性小包指纹。
- 连续 `heartbeat_miss`（默认 3）次未收到 `PONG` → 判定 Session 失效，按 §4.3 的断线流程处理。
- 收到 PING 立即回 PONG，不参与本地方的心跳计数。

### 4.5 优雅关闭

- 收到 SIGTERM/SIGINT：关闭监听（不再接受新 SOCKS5 连接），停止池内新建 Session。
- 等待活跃 Stream 结束，上限 `shutdown_grace`（默认 30s）；超时则对剩余 Stream 发送 RST 并退出。
- 退出前对每个 Session 发送 GOAWAY，并关闭 KCP Session。

---

## 5. Server 设计

### 5.1 启动流程

1. 加载并校验配置（同一 YAML 文件，取 `server:` 段）。
2. 启动 KCP Listener（UDP）。
3. 每个新 Session 启动一个 goroutine 处理（带 recover 隔离）。
4. 监听 SIGTERM/SIGINT：停止 Accept，向所有 Session 发 GOAWAY，等待活跃流结束或 `shutdown_grace` 超时，**最后**才关闭监听。
   - **注意（kcp-go 结构限制）**：`kcp.Listener.Close()` 关闭的是被所有会话共用的 UDP socket，一旦关闭，所有活跃会话会立刻读到错误。因此排空期间必须保持监听打开；对新到会话采用「接受后立刻回 GOAWAY 并关闭」的方式拒绝，排空或超时后再关闭监听。

### 5.2 Session 处理流程

1. 等待 `AUTH_REQ`，超时 `auth_timeout`（默认 5s）则关闭；未认证 Session 并发超过 `max_pending_sessions` 时直接丢弃新 Session。
2. 校验 token、时间戳、nonce、HMAC proof（§3.4）。
3. 返回 `AUTH_RESP`，派生会话密钥，进入安全态。
4. 进入 mux 循环：
   - 读帧、解密、按 StreamID 分发。
   - `CONNECT_REQ`：新建 Stream，拨号目标。
     - 成功：回复 `CONNECT_RESP{ok}`；若 Stream 数已达上限，回复失败码并 RST。
     - 失败：映射错误码（§5.3），回复失败。
   - `DATA`：写入目标连接（带超时保护）。
   - `FIN`：关闭目标写方向。
   - `RST`：关闭目标连接，清理 Stream。
   - `PING`：回 `PONG`。
5. Session 关闭：关闭所有 Stream 与目标连接，清理 Session 级状态。

### 5.3 拨号

- 使用 `net.Dialer`，设置 `Timeout`（默认 10s）、`KeepAlive`（默认 30s）；拨号必须携带 context（受 Session 生命周期约束）。
- 目标地址来自 `CONNECT_REQ`，支持 IPv4/IPv6/域名（域名在 Server 侧解析，避免本地 DNS 泄漏）。
- 拨号失败映射到 SOCKS5 错误码：

| 情况                   | SOCKS5 回复码 |
| ---------------------- | ------------- |
| 成功                   | `0x00`        |
| 一般失败               | `0x01`        |
| 规则不允许（预留 acl） | `0x02`        |
| 网络不可达             | `0x03`        |
| 主机不可达             | `0x04`        |
| 连接被拒               | `0x05`        |
| TTL 过期               | `0x06`        |
| 不支持的命令           | `0x07`        |
| 不支持的地址类型       | `0x08`        |

- 映射依据：`context.DeadlineExceeded`/`net.Error.Timeout()` → `0x06`；`ECONNREFUSED` → `0x05`；`ENETUNREACH` → `0x03`；`EHOSTUNREACH`/DNS 失败 → `0x04`；其余 → `0x01`。

### 5.4 目标转发

- 目标读 → 封装 DATA → 写 Session（拆帧 ≤16KB）。
- Session DATA → 写目标（写超时保护）。
- 目标 EOF → 发送 FIN。
- 目标错误 → 发送 RST。
- 任何转发 goroutine panic → recover、记录堆栈、发 RST、清理该 Stream，不影响进程。

---

## 6. KCP 参数与调优

### 6.1 基础参数

```go
kcp.NewKCP(conv, output)
kcp.NoDelay(1, 10, 2, 1)   // nodelay, interval, resend, nc
kcp.WndSize(256, 256)      // sndwnd, rcvwnd
kcp.SetMtu(1350)
kcp.SetStreamMode(true)
```

阶段 A 追加（传输层加密，密钥由 PSK 经 HKDF 派生，**禁止直接截断 PSK**）：

```go
// 推荐：AEAD 模式（kcp-go v5.6.72 起提供）
block, _ := kcp.NewAESGCMCrypt(hkdfKey(psk, "gks-kcp-aes-128-gcm-v1", 16))
sess, _ := kcp.DialWithOptions(raddr, block, dataShards, parityShards)
```

### 6.2 建议初始值

| 参数            | 建议值           | 说明                                                                                                                                                                                                                                                                                                                      |
| --------------- | ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| nodelay         | 1                | 启用快速重传                                                                                                                                                                                                                                                                                                              |
| interval        | 10ms             | 刷新间隔（范围 5ms~100ms）                                                                                                                                                                                                                                                                                                |
| resend          | 2                | 快速重传阈值                                                                                                                                                                                                                                                                                                              |
| nc              | 1                | 关闭拥塞控制，激进                                                                                                                                                                                                                                                                                                        |
| sndwnd          | 256              | 发送窗口（范围 32~4096）                                                                                                                                                                                                                                                                                                  |
| rcvwnd          | 256              | 接收窗口（范围 32~4096）                                                                                                                                                                                                                                                                                                  |
| mtu             | 1350             | 避免 IP 分片（范围 576~1400，需为 header 预留空间）                                                                                                                                                                                                                                                                       |
| FEC             | 10/3（丢包链路） | 走 `data_shards`/`parity_shards`。实测（loopback 无丢包）：开 10/3 后线/载荷开销从 **2.1% 升到 34.6%**、payload 吞吐从 214 MB/s 降到 **161 MB/s**（-20%）；这是纯成本。在真实丢包链路上它用 +30% 带宽换掉重传（每次丢包约 1 RTT + 重复数据），通常净赚，判据是 `fec_recovered` 是否 > 0、`retrans`/`repeat_segs` 是否下降 |
| crypt（阶段 A） | aes-128-gcm      | AEAD 模式（每包随机 nonce + GCM tag）；AES-NI 加速；仅作传输层保护                                                                                                                                                                                                                                                        |

### 6.3 保活

- KCP 自身有 dead-link 检测（`IKCP_DEADLINK`），但语义与撤销时机不透明，**不作为唯一依赖**。
- 应用层 PING/PONG 为第一手段，间隔抖动 15s~25s（§4.4）。
- 两条手段同时生效，任一先触发即按 Session 失效处理。

---

## 7. 配置项

### 7.1 形态（定稿）

- **单个 YAML 文件**，顶层分**三段**：
  - `common:` 两端**必须一致**的参数——只写一次；
  - `client:` 仅客户端使用的参数；
  - `server:` 仅服务端使用的参数。
- 两个程序各自 `-c <同一文件>`，各取所需段落（客户端读 `common` + `client`，服务端读 `common` + `server`）。
- **为什么要抽出 `common`**：PSK、传输层/应用层加密算法、KCP 调参（interval/MTU/窗口/FEC）、流空闲口径、流数与帧体上限，这些字段**一旦两端不一致就会出问题**——轻则行为诡异（窗口/MTU 不匹配），重则直接不可用（psk/crypt/aead 不一致导致握手或解密失败）。集中到一段后，"两处各写一份、改漏一处"在结构上不可能发生。
- 因此这些字段**不允许**出现在 `client:`/`server:` 里；写错位置会因严格模式直接报错（有单测守着这条规则）。
- 加载使用 yaml.v3 的**严格模式**（`KnownFields(true)`）：出现未知字段直接报错退出。
- 校验入口：客户端的 `ValidateClient()` 与服务端的 `ValidateServer()`，各自校验 `common` + 自身段落，用 `errors.Join` **一次性报出所有问题**（不必改一个跑一次）。
- 全部时长字段使用 Go duration 字符串（如 `10s`、`300ms`）。
- 已知取舍：单文件意味着 Client 机器上也会带着 `server:` 段（仅含未使用字段，无害）。若日后需要分开分发，可增加「整文件即单段」的自动识别，不破坏现有格式。

### 7.2 完整示例

```yaml
common:                              # 两端必须一致，只写一次
  psk: "base64-encoded-32B"
  crypt: "none"                      # none | aes-128-gcm（阶段 A 推荐）| aes-256-gcm | aes-128 | aes-256 | salsa20
  aead: "chacha20-poly1305"          # chacha20-poly1305 | aes-256-gcm
  kcp:
    interval: 10ms
    mtu: 1350
    sndwnd: 256
    rcvwnd: 256
    data_shards: 10                  # FEC：10 数据 + 3 校验（0 = 关闭）
    parity_shards: 3                 # 丢包链路建议开启（线开销约 +30%，换来免重传的低抖动）
  stream:
    idle_timeout: 600s               # 流长时间无数据则 RST
  limits:
    max_streams_per_session: 256     # 客户端开流上限 = 服务端接受上限
    max_frame_body: 65536            # 超过即断开（防伪造长度挂起）
    max_data_payload: 16384          # 发送方据此拆帧
  metrics_interval: 10s              # 传输统计采样间隔（0 = 关闭）

client:                              # 仅客户端
  listen: "127.0.0.1:2080"           # 本地 SOCKS5 TCP 监听（不用 1080 这个常见端口）
  auth:
    mode: "none"                     # none | userpass(TODO，当前未实现)
    users: []
  socks5:
    handshake_timeout: 10s          # 方法协商 + 请求解析
    connect_timeout: 10s            # 等待服务端 CONNECT_RESP
  pool:
    size: 2
    max_sessions: 64                 # 既是 Session 硬上限，也是并发连接上限
    idle_timeout: 300s
    startup_jitter: 300ms
    connect_timeout: 5s
    backoff_min: 500ms
    backoff_max: 30s
  kcp:
    server: "127.0.0.1:4000"         # 服务端 KCP/UDP 地址
    heartbeat_interval: 20s          # 实际在 15s~25s 间抖动
    heartbeat_miss: 3
    auth_timeout: 5s                 # 等待 AUTH_RESP
  shutdown_grace: 30s
  log:
    level: "info"                    # debug | info | warn | error

server:                              # 仅服务端
  listen: "127.0.0.1:4000"           # KCP/UDP 监听
  auth:
    timestamp_window: 60s
    auth_timeout: 5s                 # 等待 AUTH_REQ
    replay_cache_size: 65536
    max_pending_sessions: 1024
  dial:
    timeout: 10s
    keepalive: 30s
  shutdown_grace: 30s
  log:
    level: "info"
```

字段归属速查：

| 段       | 字段                                                                                          | 说明                            |
| -------- | --------------------------------------------------------------------------------------------- | ------------------------------- |
| common   | `psk` / `crypt` / `aead`                                                                      | 密钥与两端加密算法，必须一致    |
| common   | `kcp.interval` / `mtu` / `sndwnd` / `rcvwnd` / `data_shards` / `parity_shards`                | KCP 调参，必须一致              |
| common   | `stream.idle_timeout`                                                                         | 两端对「流空闲多久算死」的口径  |
| common   | `limits.max_streams_per_session`                                                              | 客户端开流上限 = 服务端接受上限 |
| common   | `limits.max_frame_body` / `max_data_payload`                                                  | 接收方校验上限 / 发送方拆帧上限 |
| client   | `listen` / `auth` / `socks5` / `pool` / `kcp.server` / `kcp.heartbeat_*` / `kcp.auth_timeout` | 仅客户端                        |
| server   | `listen` / `auth.*` / `dial.*`                                                                | 仅服务端                        |
| 两端各自 | `shutdown_grace` / `log.level`                                                                | 本地行为，允许不同              |

### 7.3 启动校验规则（必须实现）

| 项                                                       | 规则                                                            | 失败处理 |
| -------------------------------------------------------- | --------------------------------------------------------------- | -------- |
| common.psk                                               | base64 解码后必须为 32B，且不能全 0                             | 报错退出 |
| common.crypt                                             | ∈ {none, aes-128-gcm, aes-256-gcm, aes-128, aes-256, salsa20}   | 报错退出 |
| common.aead                                              | ∈ {chacha20-poly1305, aes-256-gcm}（空值即 chacha20-poly1305）  | 报错退出 |
| common.kcp.mtu                                           | 576..1400                                                       | 报错退出 |
| common.kcp.interval                                      | 5ms..100ms                                                      | 报错退出 |
| common.kcp.sndwnd / rcvwnd                               | 32..4096                                                        | 报错退出 |
| common.kcp.parity_shards                                 | 启用 FEC（data_shards>0）时必须 > 0                             | 报错退出 |
| common.kcp.data_shards + parity_shards                   | ≤ 256（reedsolomon 硬约束）                                     | 报错退出 |
| common.stream.idle_timeout                               | > 0                                                             | 报错退出 |
| common.limits.max_streams_per_session                    | 1..32768                                                        | 报错退出 |
| common.limits.max_frame_body                             | (0, 65536]，且 > 5+16                                           | 报错退出 |
| common.limits.max_data_payload                           | ≥ 1，且 ≤ max_frame_body − 5 − 16                               | 报错退出 |
| common.metrics_interval                                  | ≥ 0（0 表示关闭采样）                                           | 报错退出 |
| client.listen / client.kcp.server / server.listen        | 合法 `host:port`，端口 1..65535；client.kcp.server 必须指定主机 | 报错退出 |
| client.auth.mode                                         | 仅 `none`；`userpass` 报「尚未实现」                            | 报错退出 |
| client.auth.users                                        | 当前必须为空                                                    | 报错退出 |
| client.socks5.handshake_timeout                          | > 0                                                             | 报错退出 |
| client.socks5.connect_timeout                            | > 0                                                             | 报错退出 |
| client.pool.size / max_sessions                          | size 1..16；max_sessions 1..128，且 ≥ size                      | 报错退出 |
| client.pool.idle_timeout / connect_timeout / backoff_min | > 0，且 backoff_max ≥ backoff_min                               | 报错退出 |
| client.kcp.heartbeat_interval / auth_timeout             | > 0                                                             | 报错退出 |
| client.kcp.heartbeat_miss                                | ≥ 1                                                             | 报错退出 |
| server.auth.timestamp_window / auth_timeout              | > 0                                                             | 报错退出 |
| server.auth.replay_cache_size / max_pending_sessions     | > 0                                                             | 报错退出 |
| server.dial.timeout / keepalive                          | > 0                                                             | 报错退出 |
| client.shutdown_grace / server.shutdown_grace            | > 0                                                             | 报错退出 |
| client.log.level / server.log.level                      | ∈ {debug, info, warn, error}                                    | 报错退出 |
| 未知字段                                                 | 严格模式下视为错误（含把 common 字段写进 client/server）        | 报错退出 |

---

## 8. 开发顺序

按阶段推进，每阶段必须可运行、可验证。

### 阶段 1：基础骨架

- 项目结构（`gks/` module 根，见 §10）、配置加载与校验、日志（`slog`）。
- `mux` 的 `Mux`/`Stream` 接口定义（接口先行，先不给实现）。
- 帧格式定义与编解码单测（握手态 / 安全态）。
- AEAD 封装、HKDF 派生、nonce 计数器单测。
- AUTH_REQ/AUTH_RESP 的构造、proof 计算、时间窗与重放缓存单测。

**验证**：`go test ./...` 通过；帧 round-trip 正确；AAD 篡改全部被检出；nonce 双向独立递增。

### 阶段 2：KCP 最小链路

- `transport` 封装 Client/Server 建立单条 KCP Session（`common.crypt` 开关预留，默认 `none`）。
- 实现 `AUTH_REQ/AUTH_RESP`，握手态 → 安全态切换。
- 实现 `PING/PONG`（含间隔抖动与 PONG 回显校验）。
- **不做多路复用**，仅 Session 级 echo。
- 集成测试：同进程起 Server + Client（随机端口），完成握手、echo、心跳。

**验证**：Client 发送字符串，Server 回显；日志可见 AUTH 结果与心跳；`go test -race ./...` 通过。

### 阶段 3：单 Stream CONNECT

- Client 侧 SOCKS5 握手 + CONNECT 解析。
- `CONNECT_REQ/RESP`、`DATA`、`FIN`。
- Server 拨号目标并转发。
- 每条 SOCKS5 连接使用独立 KCP Session（暂不进池）。

**验证**：
```bash
curl --socks5-hostname 127.0.0.1:2080 http://127.0.0.1:8080/
curl --socks5-hostname 127.0.0.1:2080 https://127.0.0.1:8443/ -k
```

### 阶段 4：多路复用

- 实现 `Mux`/`Stream` 接口的自研实现：StreamID 递增分配、分发、写聚合、单写原则。
- 先用内存 pipe 模拟 KCP 单测 mux，再接入真实 KCP。
- 单 Session 承载多 Stream。
- 半关闭语义完整实现（§3.8 状态机逐条单测）。

**验证**：
- 单 Session 上并发 10 条 curl 全部成功。
- 单条流半关闭不影响其他流。
- 状态机单测覆盖表中每一行。

### 阶段 5：连接池

- Session 池管理与调度（最少 Stream 优先）、启动错峰。
- Session 断开重连（指数退避 + 抖动）。
- 空闲回收与保底数量。

**验证**：
- 并发 100 条连接，Session 数符合预期。
- 杀掉 Server，Client 自动重连，新连接恢复（记录恢复耗时）。

### 阶段 6：稳定性加固

- 超时：握手、认证、连接、空闲、心跳（§7.2 全部生效）。
- goroutine 泄漏检查、panic recover、优雅关闭（SIGTERM）。
- nonce 溢出检查、安全态解密失败即断开（不降级）。
- 阶段 A：开启 `common.crypt` 并回归全部测试。

**验证**：
- `go test -race ./...`。
- 本机 10 分钟稳定性采样（§9.4）；1 小时长跑待 Linux 环境。
- `pprof` 检查 goroutine 数稳定。

### 阶段 7：错误处理与观测

- SOCKS5 错误码映射（§5.3 全表）。
- 结构化日志（§11.9 字段规范）。
- 指标：连接数、Session 数、Stream 数、字节数、错误计数、重连次数、KCP SRTT。

**验证**：
- 目标不可达、拒绝、超时分别返回正确 SOCKS5 错误码。
- 日志与指标可读。

### 阶段 8：端到端测试与压测

- 自动化集成测试脚本。
- **丢包模拟：优先用 mock PacketConn（可复现、可进 CI）**，其次 Linux 上 `tc netem`。
- 并发压测。

**验证**：
- 丢包 5%（mock PacketConn）下功能可用。
- 并发 100 条无错误（本机基线）；500 条待评估（见 §9.5）。

---

## 9. 检验环节

### 9.1 单元测试

- 帧编解码 round-trip（握手态 / 安全态各一组，含 Payload 空、1B、16KB 边界）。
- 帧异常：坏 Magic、坏 Ver、Flags≠0、Length 超限、截断、长度与实际不符。
- AAD 篡改检测：分别篡改 Magic / Ver / Flags / Length / 密文体，均必须解密失败。
- AEAD 加解密、HKDF 派生向量、nonce 计数器（双向独立、递增、溢出拒绝）。
- AUTH：proof 正确/错误、时间窗边界（±window、±window+1s）、重放拒绝、缓存 TTL 与容量淘汰。
- StreamID 分配严格递增且为奇数；到达上限后的拒绝行为。
- 错误码映射（拨号错误 → SOCKS5 码）。
- 半关闭状态机（§3.8 表中每行）。
- 配置加载与校验（§7.3 每行：合法值通过、非法值报错）。
- 不降级：安全态收到明文帧必须断开；握手态收到 DATA 必须断开。

### 9.2 集成测试

- 同进程启动 Server 与 Client（随机端口，避免占用冲突）。
- 本地起 HTTP 服务与自签 HTTPS 服务作为目标。
- 通过 SOCKS5 访问，校验响应体与状态码。
- 多并发（10/100 条），断言 Session/Stream 计数符合预期。
- Server 重启后 Client 恢复（断言恢复耗时 ≤ 30s）。
- 心跳：注入静默（暂停 PONG）验证 `heartbeat_miss` 后判定失效。

### 9.3 端到端脚本

仓库内已提供一键脚本 `test/e2e.sh`（自动挑端口、生成随机 PSK、起本地目标、测外部目标与错误码）。等价的裸命令如下：

```bash
# 启动（两个程序读同一个配置文件）
./bin/server -c gks.yaml &
./bin/client -c gks.yaml &

# 本地目标服务（示例）
python3 -m http.server 8080 &

# 测试
curl --socks5-hostname 127.0.0.1:2080 http://127.0.0.1:8080/
curl --socks5-hostname 127.0.0.1:2080 https://example.com

# 并发
for i in $(seq 1 100); do
  curl --socks5-hostname 127.0.0.1:2080 http://127.0.0.1:8080/ &
done
wait
```

### 9.4 稳定性测试

- `go test -race ./...`（每阶段门槛）。
- **本机可执行**：10 分钟运行，每 10s 采样 `runtime.NumGoroutine` 与 `HeapAlloc`，断言 goroutine 数不随连接数线性增长、空闲后回落到基线附近。
- **待 Linux 环境/人工**：1 小时长跑；内存长期曲线；断网重连；Server 重启恢复（可与集成测试合并）。
- 每条连接 goroutine 必须有 recover 与超时，静态检查 + 单测覆盖。

### 9.5 性能测试（含丢包）

- **丢包模拟（本机首选）**：测试用 mock `net.PacketConn` 包装真实 UDP，按配置概率与固定随机种子丢包，可实现可复现的 5% 丢包集成测试；这是本机替代 `tc netem` 的正式方案。
- 备选：macOS 上 `dnctl` + `pf` 做系统级丢包（需 sudo，属审批操作）；Linux 上 `tc qdisc add dev eth0 root netem loss 5%` 做最终复验。
- 吞吐：`curl --socks5-hostname ... -w '%{speed_download}'` 拉大文件即可得到**端到端载荷速率**；更细的链路数据用下面的内置统计。
  - **注意**：iperf3 原生不支持 SOCKS5（需 proxychains 之类包装），因此不把 iperf3 作为本项目的标准手段；需要独立压测工具时用 Go 编写（见 §9.5 末尾）。
- 延迟：`curl -w` 记录 `time_connect`/`time_total`。
- 并发上限：先确定本机 `ulimit -n`（macOS 默认常为 256），据此设定基线（100 条），500 条待提高限额或换环境后评估。

### 9.5.1 内置速率统计（`common.metrics_interval`）

两端各自每 `metrics_interval` 输出一个 `event=metrics` 统计块：**首行是事件头**（`event`、采样间隔），
其后**每个分组一行**，组内字段按同一列宽对齐（`internal/metrics.FormatBlock`）。分组顺序固定为
会话/流 → 载荷速率 → 线速率 → 累计量 → 链路质量 → 会话池 → 错误；服务端不维护会话池，故不输出 `[会话池]` 行。
指标触发关注条件时，末尾追加一行 `[告警]`（`pool_waiters > 0`、`retrans > 1%` 且发送段 ≥ 20、`fec_errs > 0`、`kcp_in_errors > 0`）。

```text
time=... level=INFO msg=传输统计 event=metrics interval=10s
  [会话/流]  sessions=2        streams=1         sessions_total=12 streams_total=418
  [载荷速率] payload_sent=426.0 KB/s payload_recv=2.3 KB/s
  [线速率]   wire_sent=2.7 MB/s  wire_recv=38.0 KB/s
  [累计量]   payload_sent_total=27.0 MB  payload_recv_total=142.0 KB wire_sent_total=57.0 MB     wire_recv_total=1007.0 KB
  [链路质量] retrans=52.54%   lost_segs=4032   repeat_segs=7    kcp_in_errors=0  fec_recovered=35 fec_errs=0
  [会话池]   pool_in_use=1   pool_idle=1     pool_creating=0 pool_waiters=0  pool_rebuilds=0
  [错误]     auth=0    session=0 socks5=0  dial=0
```

字段含义（不是结构化日志的独立属性，而是块内的 `key=value`）：

| 字段                                                                             | 含义                                                                                                          |
| -------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `payload_sent` / `payload_recv`                                                  | 隧道**载荷**速率（B/s，实时累加，与线速率同窗口口径）                                                         |
| `wire_sent` / `wire_recv`                                                        | **UDP 线速率**（来自 kcp-go `DefaultSnmp.OutBytes/InBytes`）                                                  |
| `payload_*_total` / `wire_*_total`                                               | 进程启动至今的累计量                                                                                          |
| `retrans`                                                                        | 本区间「重传段 / 发送段」；发送段 < 20 时显示 `n/a`（小样本百分比会误导）                                     |
| `lost_segs` / `repeat_segs` / `kcp_in_errors`                                    | 推断丢包、对端重传来的重复段、KCP 输入错误                                                                    |
| `fec_recovered` / `fec_errs`                                                     | FEC 恢复出的包数 / 恢复失败数（未启用 FEC 时恒为 0；`fec_recovered > 0` 说明 FEC 正在起作用）                 |
| `pool_in_use` / `pool_idle` / `pool_creating` / `pool_waiters` / `pool_rebuilds` | 会话池使用中/空闲/正在建立/排队等待的连接数，以及重建次数（`pool_waiters > 0` 表示并发已顶到 `max_sessions`） |
| `sessions` / `streams`                                                           | 当前活跃会话数 / 流数；`*_total` 为累计                                                                       |
| `auth` / `session` / `socks5` / `dial`                                           | 认证失败 / 建会话失败 / 本地握手失败 / 目标拨号失败（累计值）                                                 |

判读要点：

- 客户端 `payload_recv_total` 与服务端 `payload_sent_total` 应基本相等，否则说明有流被中途重置；
- `wire / payload` 的比值即链路开销：`crypt: none` + 16KB 帧时实测约 **1.02（2%）**；
- `retrans` 持续 > 1% 说明 UDP 链路有丢包或限速，可结合 `lost_segs` 与 §12 的风险表处理；
- 服务端 `sessions` 长期不降说明会话未回收；`[错误]` 行里 `dial` 增长说明目标侧不可达。

### 9.6 兼容性测试

- `curl --socks5` 与 `--socks5-hostname`。
- `golang.org/x/net/proxy`（写进集成测试）。
- `ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:2080 %h %p' user@host`（本机可验证）。
- 浏览器（Chrome/Firefox）代理配置：**待人工**，本机无 GUI 不覆盖。

---

## 10. 项目结构

模块名：`github.com/LiZeC123/gks`。**项目根为 `gks/` 子目录**，本技术方案文档位于其上级目录。

```text
DeepseekSpace/
├── dev.md                        # 本文档
└── gks/                          # Go module: github.com/LiZeC123/gks
    ├── cmd/
    │   ├── client/
    │   │   └── main.go
    │   └── server/
    │       └── main.go
    ├── internal/
    │   ├── protocol/
    │   │   ├── frame.go            # 帧定义与编解码（握手态/安全态）
    │   │   ├── frame_test.go
    │   │   ├── crypto.go           # AEAD 封装、HKDF、nonce 计数器
    │   │   ├── crypto_test.go
    │   │   ├── auth.go             # AUTH_REQ/RESP、HMAC proof、时间窗、重放缓存
    │   │   └── auth_test.go
    │   ├── mux/
    │   │   ├── iface.go            # Mux/Stream 接口（便于换 smux）
    │   │   ├── mux.go
    │   │   ├── session.go
    │   │   ├── stream.go
    │   │   └── mux_test.go
    │   ├── transport/
    │   │   ├── dialer.go           # Client 侧 KCP 建连（含 crypt 开关）
    │   │   ├── listener.go         # Server 侧 KCP Listener
    │   │   └── lossy.go            # mock PacketConn（丢包注入，测试用）
    │   ├── client/
    │   │   ├── socks5.go
    │   │   ├── handler.go
    │   │   ├── pool.go
    │   │   └── pool_test.go
    │   ├── server/
    │   │   ├── listener.go
    │   │   ├── session.go
    │   │   ├── dialer.go
    │   │   └── session_test.go
    │   ├── config/
    │   │   ├── config.go
    │   │   └── config_test.go
    │   └── log/
    │       └── log.go
    ├── test/
    │   ├── gks.yaml.example
    │   ├── e2e.sh
    │   └── concurrent.sh
    ├── go.mod
    └── README.md
```

依赖版本（已锁定在 `go.mod`）：`github.com/xtaci/kcp-go/v5 v5.6.72`、`gopkg.in/yaml.v3 v3.0.1`、`golang.org/x/crypto v0.45.0`。

---

## 11. 关键实现约束

1. **单写原则**：每条 KCP Session 只能有一个 goroutine 写 KCP，其他通过 channel 投递。
2. **无阻塞分发**：Session 读循环不得因单个 Stream 阻塞；单流缓冲满到阈值时按 §3.7 处理（RST 该流，保护 Session）。
3. **背压必做**：Stream 发送缓冲满时阻塞上层，不得无限堆积（上限 256KB）。
4. **半关闭必做**：`FIN` 语义按 §3.8 状态机完整实现，否则会出现连接悬挂。
5. **超时必做**：握手、认证、连接、空闲、心跳全部设置超时，且全部可配置、启动时校验。
6. **panic 隔离**：每个连接/会话 goroutine 加 recover，记录堆栈，不影响进程。
7. **优雅关闭**：收到 SIGTERM 后停止接受新连接，发 GOAWAY，等待活跃流结束或 `shutdown_grace` 超时。
8. **配置校验**：按 §7.3 全表校验（PSK 长度、端口、窗口、MTU、interval、crypt/aead 一致性等）。
9. **日志规范**：结构化字段，至少含 `session_id`(conv)、`stream_id`、`remote`(对端地址)、`target`(目标地址)、`event`、`err`；日志级别可配。
10. **测试优先**：帧、加密、认证、mux 必须先单测，再集成。
11. **不降级**：安全态解密失败一律断开，禁止回退明文解析。
12. **nonce 不重用**：方向独立密钥 + 独立单调计数器；溢出即断开，禁止回绕。
13. **明文头权衡**：`Magic` 与 `Length` 必然明文；`Type`/`StreamID` 在安全态必须不可见。任何新增头部字段都必须明确「明文还是密文」以及是否参与 AAD。
14. **帧长度防御**：`Length` 超上限立即断开，不得无限等待字节。

---

## 12. 风险与应对

| 风险                           | 影响                           | 应对                                                                                              |
| ------------------------------ | ------------------------------ | ------------------------------------------------------------------------------------------------- |
| UDP 被 QoS 限速                | 连接不稳                       | 心跳保活（抖动间隔）、可切换端口、记录日志                                                        |
| KCP 队头阻塞                   | 多流互相影响                   | 连接池多 Session 分摊；单帧 ≤16KB                                                                 |
| goroutine 泄漏                 | 内存增长                       | 超时、context、recover、10 分钟采样 + pprof 检查                                                  |
| 半关闭处理错误                 | 连接悬挂                       | §3.8 状态机单测逐行覆盖 + 集成测试                                                                |
| AEAD nonce 重用                | 安全问题                       | 方向独立密钥 + 独立单调计数器 + 溢出断开 + 单测断言递增                                           |
| **KCP 头明文被篡改（阶段 B）** | 干扰窗口/RTT，造成连接质量下降 | 阶段 A 开启 `common.crypt`（推荐 `aes-128-gcm`）覆盖传输层头；阶段 B 期间仅记日志、无有效对抗手段 |
| **协议指纹可识别（阶段 B）**   | 被 DPI 识别为 KCP 隧道         | 阶段 A 隐藏 KCP/FEC 头；`Magic` 与首帧 AUTH_REQ 无法隐藏（已知残余）                              |
| Session 断连未通知             | 连接假死                       | GOAWAY + RST 广播 + 心跳判死                                                                      |
| 拨号阻塞                       | Stream 卡死                    | Dialer 超时 + context                                                                             |
| 写 KCP 阻塞                    | 全局卡死                       | 单写 goroutine + 有界队列 + 背压                                                                  |
| 未认证 Session 被大量占用      | 资源耗尽                       | `max_pending_sessions` 上限 + 认证超时                                                            |
| 时钟偏差过大                   | 认证全部失败                   | `timestamp_window` 可调；正确性由 nonce 缓存保证，仅需宽松同步                                    |

---

## 13. 交付标准

满足以下条件视为完成（标注「本机可验」/「待环境」）：

1. `curl --socks5-hostname` 可正常访问 HTTP/HTTPS（**本机可验**，用本地目标服务）。
2. 单 Session 支持至少 100 条并发 Stream（**本机可验**）。
3. 连接池自动重连，Server 重启后 30 秒内恢复（**本机可验**，集成测试断言恢复耗时）。
4. `go test -race ./...` 全绿（**本机可验**，每阶段门槛）。
5. 1 小时稳定性运行无 goroutine 泄漏、无内存持续增长（**待环境**；本机先做 10 分钟采样）。
6. 5% 丢包下功能可用（**本机可验**，用 mock PacketConn；Linux 上 `tc netem` 复验）。
7. 日志、指标可读，错误码映射正确（**本机可验**）。
8. 配置文件完整，启动参数文档化（**本机可验**，含 `test/gks.yaml.example`）。
9. 端到端脚本可一键运行（**本机可验**）。
10. 并发 500 连接无错误（**待环境/待评估**：受本机 `ulimit -n` 限制）。
11. 浏览器代理配置可用（**待人工**）。

---

## 14. 建议给 AI 的实现顺序提示

1. 先定接口：`mux.Mux`/`mux.Stream`、`protocol` 的编解码与加解密签名，再写实现。
2. 先写 `protocol/frame.go` 与单测（握手态/安全态、边界、异常、AAD 篡改）。
3. 再写 `protocol/crypto.go`（HKDF、AEAD、nonce 计数器）与单测。
4. 再写 `protocol/auth.go`（proof、时间窗、重放缓存）与单测。
5. 再写 `config` 与 `log`，跑通两个 `cmd` 的空壳启动与校验失败路径。
6. `transport` 接入真实 KCP，跑通 AUTH + PING/PONG + echo（阶段 2）。
7. 接入 SOCKS5，跑通单 Stream（阶段 3）。
8. 打开多路复用（先用内存 pipe 单测 mux），跑并发（阶段 4）。
9. 加连接池与启动错峰、重连退避（阶段 5）。
10. 加超时、心跳判死、优雅关闭、nonce 溢出与不降级检查（阶段 6）。
11. 加日志字段、指标、错误码映射（阶段 7）；开启 `common.crypt` 回归（阶段 A）。
12. 最后压测与稳定性验证（阶段 8，丢包用 mock PacketConn）。

每完成一步必须提交可运行、可测试的代码，禁止一次性堆砌。

---

## 15. 变更记录与初版对照

| #   | 条目                 | 初版                                                                                                             | 定稿                                                                                                                                                                                                                                                                                                      | 原因                                                                                             |
| --- | -------------------- | ---------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| 1   | 项目名/模块          | 目录 `socks5-kcp/`                                                                                               | 目录 `gks/`，module `github.com/LiZeC123/gks`                                                                                                                                                                                                                                                             | 名称过于具体，改用 gks                                                                           |
| 2   | 帧头                 | 13B 全明文（Magic/Ver/Type/Flags/StreamID/Length）                                                               | 8B 明文头（Magic/Ver/Flags/Length）+ 加密体（Type/StreamID/Payload）                                                                                                                                                                                                                                      | 隐藏 Type/StreamID，削弱帧级元数据外露                                                           |
| 3   | 编码态               | 未定义                                                                                                           | 握手态明文 / 安全态密文，且不降级                                                                                                                                                                                                                                                                         | 明确 AUTH 前后差异，防降级攻击                                                                   |
| 4   | 认证                 | 两套方案并列（HMAC token 派生 / 整 Session 单密钥），未定                                                        | AUTH_REQ 明文 + HMAC proof，HKDF 派生 `key_c2s`/`key_s2c`                                                                                                                                                                                                                                                 | 收敛到单一可实现方案，PSK 不上线                                                                 |
| 5   | nonce                | 只提"单调递增计数器"                                                                                             | 方向独立密钥 + 每方向独立 12B nonce（4B 0 + 8B 计数器），溢出断开                                                                                                                                                                                                                                         | 初版会导致双向 nonce 冲突，AEAD 失效                                                             |
| 6   | 加密算法             | ChaCha20-Poly1305 或 AES-256-GCM（未定）                                                                         | 默认 ChaCha20-Poly1305，`aead` 可配置，两端一致性校验                                                                                                                                                                                                                                                     | 消除歧义                                                                                         |
| 7   | 传输层加密           | §3.4"简化方案"含糊提及                                                                                           | 明确分两步：先仅应用层 AEAD，后加 `common.crypt`（推荐 `aes-128-gcm`）；并记录 kcp-go crypt 无密钥交换、不认证、不防重放                                                                                                                                                                                  | 分层职责清晰，避免误以为可替代                                                                   |
| 8   | 配置形态             | 顶层 `client:`/`server:`，但 §9.3 用两个文件                                                                     | 单文件，两程序 `-c` 同一文件，各取一段；严格模式                                                                                                                                                                                                                                                          | 与 §7 一致；新增字段（crypt/aead/超时/抖动/限额）并补校验表                                      |
| 9   | 本地 SOCKS5 认证     | 支持 none 与 userpass                                                                                            | 仅 none，userpass 留 TODO 且配置非空时报错                                                                                                                                                                                                                                                                | 缩小本版范围                                                                                     |
| 10  | mux                  | 自研                                                                                                             | 自研 + 先定义 `Mux`/`Stream` 接口                                                                                                                                                                                                                                                                         | 降低日后替换 smux 的成本（并注明 smux 半关闭语义不完整）                                         |
| 11  | StreamID 回收        | "释放 StreamID，归还 Session"                                                                                    | Session 内单调递增奇数，不回收不复用                                                                                                                                                                                                                                                                      | 消除旧帧串入新流的风险                                                                           |
| 12  | 心跳                 | 固定 20s                                                                                                         | 15s~25s 抖动，`heartbeat_miss=3` 判死；PONG 回显 payload                                                                                                                                                                                                                                                  | 削弱周期性小包指纹，避免误判                                                                     |
| 13  | 启动                 | 未规定                                                                                                           | 池启动错峰（`startup_jitter`，默认 300ms 内随机）                                                                                                                                                                                                                                                         | 削弱启动瞬间 N 个同时握手的特征                                                                  |
| 14  | 断线重连             | "触发重建"                                                                                                       | 指数退避 0.5s→30s + ±20% 抖动                                                                                                                                                                                                                                                                             | 避免雪崩式重连                                                                                   |
| 15  | 长度防御             | 未规定                                                                                                           | `max_frame_body=65536` 超限即断开；DATA 单帧 ≤16KB                                                                                                                                                                                                                                                        | 防伪造长度挂起与单帧过大                                                                         |
| 16  | 重放防护             | 提"nonce 未重放"                                                                                                 | 明确 `(token_id, nonce_c)` 缓存，TTL=2×窗口，LRU 容量上限                                                                                                                                                                                                                                                 | 可单测、可配置                                                                                   |
| 17  | 未认证 Session       | 未规定                                                                                                           | `max_pending_sessions` 上限 + `auth_timeout`                                                                                                                                                                                                                                                              | 稳定性保护（非用户层防御）                                                                       |
| 18  | 验证手段             | `tc netem`、1 小时长跑、500 并发、iperf3、浏览器                                                                 | 丢包改用 mock PacketConn（可复现）+ Linux 复验；10 分钟采样替代 1 小时；Go 吞吐客户端替代 iperf3；并发先做 100；浏览器待人工                                                                                                                                                                              | 本机（macOS）无法原样执行，且 mock 更可复现                                                      |
| 19  | Flags 语义           | "保留，默认 0"                                                                                                   | v1 必须为 0，非 0 丢帧并断开                                                                                                                                                                                                                                                                              | 避免 AAD 语义歧义                                                                                |
| 20  | 帧类型表             | 无编码态列                                                                                                       | 增加编码态列，AUTH_* 为握手态，其余为安全态                                                                                                                                                                                                                                                               | 与 §3.2 呼应                                                                                     |
| 21  | kcp-go 加密能力      | 初版结论：kcp-go 只有 CFB+CRC32，「不提供 AEAD」                                                                 | 修正：v5.6.72 提供 `NewAESGCMCrypt`（AEAD，每包随机 nonce + GCM tag）；阶段 A 推荐 `aes-128-gcm`                                                                                                                                                                                                          | 定版依赖 v5.6.72 后复核源码发现，原结论基于旧版本（v5.4.20 / v5.6.12 文档）                      |
| 22  | 优雅关闭排空         | 「收到 SIGTERM 后等待活跃流结束或超时」                                                                          | 补充实现约束：kcp-go 的 `Listener.Close()` 关闭的是所有会话共用的 UDP socket，因此排空期间不能关监听，必须先「逻辑停 Accept + 拒绝新会话」；阶段 2 暂为立即断开，阶段 6 修正                                                                                                                              | 实现阶段 2 时实测发现，属 kcp-go 结构限制                                                        |
| 23  | 配置结构             | 顶层 `client:`/`server:` 两段；psk/crypt/aead/KCP 调参/流与帧上限在两段各写一份                                  | 改为三段：**`common`（两端必须一致，只写一次）** + `client` + `server`；把这些字段写进 client/server 会因严格模式报错；校验入口拆为 `ValidateClient()`/`ValidateServer()` 并用 `errors.Join` 一次性报错                                                                                                   | 消除两处各写一份导致的不一致（用户要求）                                                         |
| 24  | 本地 SOCKS5 端口     | 示例与文档用 `127.0.0.1:1080`（经典端口，易与他方冲突）                                                          | 默认改为 `127.0.0.1:2080`；同时新增 `client.socks5.connect_timeout`（等待 CONNECT_RESP 的超时，默认 10s）                                                                                                                                                                                                 | 用户要求避开 1080；CONNECT_RESP 需要独立的等待上限                                               |
| 25  | 传输速率观测         | 只有 `frames_in/out`，无法回答「速率是多少」                                                                     | 新增 `common.metrics_interval`：端到端载荷速率 + UDP 线速率 + 重传率 + 累计量 + 错误计数，两端各打一行 `event=metrics`                                                                                                                                                                                    | 用户需要量化客户端↔服务端之间的传输速率                                                          |
| 26  | 连接池实现形态       | 阶段 4 做 mux、阶段 5 做池（两阶段连续）                                                                         | 阶段 5 先以「串行复用」落地：每条会话同时 1 条流，选择退化为取空闲会话，绕开流间公平调度与读路径非阻塞化两项高风险改造；阶段 4（单会话多流）仍待实现                                                                                                                                                      | 用户选择低风险路线，先拿到 AUTH/建链被摊薄的延迟收益                                             |
| 27  | FEC 与池指标的默认值 | FEC 默认关闭；指标里没有池状态与 FEC 恢复量                                                                      | 示例配置默认开 FEC 10/3 并注明实测开销；`event=metrics` 增加 `fec_recovered`/`fec_errs` 与 `pool_in_use`/`pool_idle`/`pool_creating`/`pool_waiters`/`pool_rebuilds`；`max_sessions` 上限由 16 放宽到 128（保守形态下它就是并发上限，实测峰值并发 20+）                                                    | 实测链路易丢包，需要 FEC；调 `max_sessions` 时需要能直接看到排队                                 |
| 28  | 统计输出形态         | `event=metrics` 把 20+ 个字段挤在一行，还内嵌 `errors="auth=0 session=..."` 字符串，终端里换行折行、无法快速定位 | 改为「1 行事件头 + 每组一行」的多行块（`internal/metrics.FormatBlock`），分组固定为会话/流→载荷→线速率→累计→链路质量→会话池→错误；`errors` 拆成四个独立字段；指标异常时追加 `[告警]` 行；服务端不输出 `[会话池]`。为此在 `internal/log` 增加支持多行块的 `slog.Handler`（整块一次写出，不与并发日志交错） | 用户反馈单行统计不可读，要求「类似表格、相关参数放一起」；控制台分级着色（颜色）用户要求后续再做 |

---

## 16. 待办与不做

### 16.1 待办（后续版本）

- 本地 SOCKS5 `userpass`（RFC1929）认证：配置字段已保留，实现后放开校验。
- 阶段 A 完成后，将 `common.crypt` 默认值由 `none` 改为 `aes-128-gcm`。
- FEC 调参：已支持并在示例中默认 10/3；后续可根据 `fec_recovered`/`retrans` 的实测比例给出 10/2、20/3、10/5 的推荐区间。
- `acl` 目标地址过滤（Server 侧，错误码 `0x02` 已预留）。
- 集成测试补充：断网重连、Server 重启恢复耗时断言。
- 优雅关闭排空：改为「保持监听打开但停止 Accept → 新会话一律 GOAWAY 后立即关闭 → 等活跃流结束或 `shutdown_grace` 超时 → 最后关闭监听」。阶段 2 当前是直接关闭监听，`shutdown_grace` 实际未生效（见 §15 第 22 条）。

### 16.2 明确不做（本版）

- UDP ASSOCIATE、BIND（帧类型与 SOCKS5 `0x07` 已对应）。
- 用户级限流、QoS、黑名单、审计、多用户计费、多 token。
- 流量伪装层（伪装为 QUIC/TLS 等常见 UDP 形态）。
- 帧长填充（曾评估，需牺牲带宽，暂不做）。
- 隐藏 `Magic` 与首帧 AUTH_REQ 类型：握手态必然明文，无法隐藏，已知残余特征。
