// gks-dashboard 的局部刷新：每个刷新周期取一次 /partial（服务端渲染好的内容片段），
// 替换 #content 即可——所有数值与 SVG 折线图都由 Go 渲染，前端不含任何绘图逻辑。
(function () {
  'use strict';

  var script = document.currentScript;
  var ms = script ? parseInt(script.getAttribute('data-refresh-ms'), 10) : 0;
  if (!ms || ms <= 0) {
    return;
  }
  var content = document.getElementById('content');
  var warn = document.getElementById('conn-warn');

  function tick() {
    fetch('/partial', { cache: 'no-store' })
      .then(function (resp) {
        if (!resp.ok) {
          throw new Error('HTTP ' + resp.status);
        }
        return resp.text();
      })
      .then(function (html) {
        if (content) {
          content.innerHTML = html;
        }
        if (warn) {
          warn.hidden = true;
        }
      })
      .catch(function () {
        // 刷新失败不清空已有内容，只提示；面板自身仍在运行，下一周期会重试。
        if (warn) {
          warn.hidden = false;
        }
      });
  }

  setInterval(tick, ms);
})();
