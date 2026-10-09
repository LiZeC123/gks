#!/usr/bin/env bash
#
# gks 端到端脚本（dev.md §9.3）
#
# 用法：
#   ./test/e2e.sh                  # 测本地目标 + baidu（HTTP/HTTPS）
#   TARGETS="http://www.baidu.com" ./test/e2e.sh
#
# 可用环境变量覆盖：
#   SOCKS_PORT          本地 SOCKS5 端口（默认 2080）
#   KCP_PORT            服务端 KCP 端口（默认 4000）
#   METRICS_PORT_CLIENT 客户端统计端点端口（默认 12081）
#   METRICS_PORT_SERVER 服务端统计端点端口（默认 12082，同机必须与客户端错开）
#   TARGET_PORT         本地目标服务端口（默认自动挑一个空闲端口）
#   TARGETS             额外测试的目标 URL 列表（默认 http://www.baidu.com https://www.baidu.com）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

SOCKS_PORT="${SOCKS_PORT:-2080}"
KCP_PORT="${KCP_PORT:-4000}"
METRICS_PORT_CLIENT="${METRICS_PORT_CLIENT:-12081}"
METRICS_PORT_SERVER="${METRICS_PORT_SERVER:-12082}"
TARGETS="${TARGETS:-http://www.baidu.com https://www.baidu.com}"

TMP="$(mktemp -d)"
PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do
    kill "${pid}" 2>/dev/null || true
  done
  rm -rf "${TMP}"
}
trap cleanup EXIT

log() { printf '\033[1m==> %s\033[0m\n' "$*"; }

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

wait_port() {
  python3 - "$1" "$2" <<'PY'
import socket, sys, time
host, port = sys.argv[1], int(sys.argv[2])
deadline = time.time() + 15
while time.time() < deadline:
    try:
        s = socket.create_connection((host, port), 0.3)
        s.close()
        sys.exit(0)
    except OSError:
        time.sleep(0.1)
sys.exit(1)
PY
}

TARGET_PORT="${TARGET_PORT:-$(free_port)}"

log "构建"
go build -o "${TMP}/server" ./cmd/server
go build -o "${TMP}/client" ./cmd/client

log "生成临时配置（随机 PSK，端口 ${SOCKS_PORT} / ${KCP_PORT} / 统计 ${METRICS_PORT_CLIENT},${METRICS_PORT_SERVER}）"
PSK="$(openssl rand -base64 32)"
sed -e "s|^  psk: .*|  psk: \"${PSK}\"|" \
    -e "s|127.0.0.1:2080|127.0.0.1:${SOCKS_PORT}|" \
    -e "s|127.0.0.1:4000|127.0.0.1:${KCP_PORT}|g" \
    -e "s|127.0.0.1:12081|127.0.0.1:${METRICS_PORT_CLIENT}|g" \
    -e "s|127.0.0.1:12082|127.0.0.1:${METRICS_PORT_SERVER}|g" \
    test/gks.yaml.example > "${TMP}/gks.yaml"

log "启动本地目标服务（127.0.0.1:${TARGET_PORT}）"
python3 -m http.server "${TARGET_PORT}" --directory "${TMP}" >"${TMP}/target.log" 2>&1 &
PIDS+=("$!")

log "启动 gks 服务端与客户端"
"${TMP}/server" -c "${TMP}/gks.yaml" -no-console >"${TMP}/server.log" 2>&1 &
PIDS+=($!)
"${TMP}/client" -c "${TMP}/gks.yaml" -no-console >"${TMP}/client.log" 2>&1 &
PIDS+=($!)

wait_port 127.0.0.1 "${SOCKS_PORT}" || { echo "SOCKS5 端口未就绪"; cat "${TMP}/client.log"; exit 1; }
wait_port 127.0.0.1 "${TARGET_PORT}" || { echo "本地目标未就绪"; exit 1; }

fail=0
check() {
  local url="$1" expect="${2:-200}" label="$3"
  local code
  code="$(curl -sS -m 20 --socks5-hostname "127.0.0.1:${SOCKS_PORT}" -o /dev/null -w '%{http_code}' "${url}" 2>&1)" || true
  if [[ "${code}" == "${expect}" ]]; then
    printf '  \033[32mPASS\033[0m %-40s %s\n' "${label}" "${url}"
  else
    printf '  \033[31mFAIL\033[0m %-40s %s (got %s)\n' "${label}" "${url}" "${code}"
    fail=1
  fi
}

# 统计端点：拉取 JSON 并校验关键字段（角色、schema、分组、池可用性）。
check_metrics() {
  local port="$1" role="$2"
  if curl -sS -m 5 "http://127.0.0.1:${port}/metrics?window=1s" 2>/dev/null | python3 -c '
import json, sys
p = json.load(sys.stdin)
role = sys.argv[1]
assert p["schema_version"] == 1, p.get("schema_version")
assert p["role"] == role, p.get("role")
for key in ("sessions", "streams", "payload", "wire", "link", "errors", "alarms"):
    assert key in p, key
assert p["pool"]["available"] is (role == "client"), p["pool"]
assert isinstance(p["payload"]["sent_total"], int), p["payload"]
' "${role}" 2>/dev/null; then
    printf '  \033[32mPASS\033[0m %-40s http://127.0.0.1:%s/metrics\n' "统计端点 ${role}" "${port}"
  else
    printf '  \033[31mFAIL\033[0m %-40s http://127.0.0.1:%s/metrics\n' "统计端点 ${role}" "${port}"
    fail=1
  fi
  local hz
  hz="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/healthz" 2>/dev/null || true)"
  if [[ "${hz}" == "200" ]]; then
    printf '  \033[32mPASS\033[0m %-40s http://127.0.0.1:%s/healthz\n' "存活探测 ${role}" "${port}"
  else
    printf '  \033[31mFAIL\033[0m %-40s http://127.0.0.1:%s/healthz (got %s)\n' "存活探测 ${role}" "${port}" "${hz}"
    fail=1
  fi
}

log "统计端点（-no-console 下仍然提供 JSON）"
check_metrics "${METRICS_PORT_CLIENT}" client
check_metrics "${METRICS_PORT_SERVER}" server

log "功能测试（--socks5-hostname，域名由服务端解析）"
check "http://127.0.0.1:${TARGET_PORT}/" 200 "本地 HTTP 目标"
for url in ${TARGETS}; do
  check "${url}" 200 "外部目标"
done

log "本地 DNS 模式（--socks5）"
check "http://www.baidu.com" 200 "--socks5 模式"

log "错误码测试（期望 curl 报 SOCKS5 错误 5 / 4）"
refused="$(curl -sS -m 10 --socks5-hostname "127.0.0.1:${SOCKS_PORT}" "http://127.0.0.1:9/" 2>&1 || true)"
if [[ "${refused}" == *"(5)"* ]]; then
  printf '  \033[32mPASS\033[0m 端口未监听 → connection refused(5)\n'
else
  printf '  \033[31mFAIL\033[0m 端口未监听: %s\n' "${refused}"; fail=1
fi
nodns="$(curl -sS -m 10 --socks5-hostname "127.0.0.1:${SOCKS_PORT}" "http://no-such-host-xyz.invalid/" 2>&1 || true)"
if [[ "${nodns}" == *"(4)"* ]]; then
  printf '  \033[32mPASS\033[0m 域名不存在 → host unreachable(4)\n'
else
  printf '  \033[31mFAIL\033[0m 域名不存在: %s\n' "${nodns}"; fail=1
fi

log "并发 10 条"
codes="$(for _ in $(seq 1 10); do
  curl -sS -m 20 --socks5-hostname "127.0.0.1:${SOCKS_PORT}" -o /dev/null -w '%{http_code}\n' "http://127.0.0.1:${TARGET_PORT}/" &
done; wait)"
ok_count="$(grep -c '^200$' <<<"${codes}" || true)"
if [[ "${ok_count}" == "10" ]]; then
  printf '  \033[32mPASS\033[0m 10/10 成功\n'
else
  printf '  \033[31mFAIL\033[0m 仅 %s/10 成功\n' "${ok_count}"; fail=1
fi

if [[ "${fail}" == "0" ]]; then
  log "全部通过"
else
  log "存在失败项"
  echo '--- client.log ---'; tail -20 "${TMP}/client.log"
  echo '--- server.log ---'; tail -20 "${TMP}/server.log"
fi
exit "${fail}"
