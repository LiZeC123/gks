#!/usr/bin/env bash
#
# gks 并发压测脚本（dev.md §9.3）
#
# 用法：
#   ./test/concurrent.sh [并发数] [URL]
# 默认：100 条并发，目标为本地 127.0.0.1:8081
#
# 注意：并发数受本机文件描述符限制（macOS 默认 ulimit -n 常为 256）。
# 需要更大并发时：ulimit -n 4096 && ./test/concurrent.sh 500
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

N="${1:-100}"
URL="${2:-http://127.0.0.1:8081/}"
SOCKS_PORT="${SOCKS_PORT:-2080}"

echo "并发 ${N} 条，目标 ${URL}，代理 127.0.0.1:${SOCKS_PORT}"
echo "当前 ulimit -n = $(ulimit -n)"

tmp="$(mktemp)"
start=$(date +%s)

for _ in $(seq 1 "${N}"); do
  curl -sS -m 30 --socks5-hostname "127.0.0.1:${SOCKS_PORT}" -o /dev/null -w '%{http_code}\n' "${URL}" >>"${tmp}" 2>&1 &
done
wait

end=$(date +%s)
total="$(wc -l <"${tmp}" | tr -d ' ')"
ok="$(grep -c '^200$' "${tmp}" || true)"
echo "完成 ${total} 条，200 = ${ok}，耗时 $((end - start))s"
if [[ "${ok}" != "${N}" ]]; then
  echo "非 200 的响应（前 10 条）："
  grep -v '^200$' "${tmp}" | head -10
  rm -f "${tmp}"
  exit 1
fi
rm -f "${tmp}"
echo "全部成功"
