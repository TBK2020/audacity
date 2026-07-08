// labdeck WebSSH terminal page: xterm.js ↔ WebSocket ↔ Go SSH gateway.
"use strict";

const params = new URLSearchParams(location.search);
const hostID = params.get("host");
const targetEl = document.getElementById("target");
const stateEl = document.getElementById("state");
const stateText = document.getElementById("state-text");

function setState(cls, text) {
  stateEl.className = cls;
  stateText.textContent = text;
}

const term = new Terminal({
  fontFamily: 'ui-monospace, "SF Mono", Menlo, Consolas, monospace',
  fontSize: 14,
  cursorBlink: true,
  theme: {
    background: "#0d0d0d",
    foreground: "#e8e8e3",
    cursor: "#e8e8e3",
    selectionBackground: "rgba(255,255,255,0.25)",
  },
});
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("term"));
fit.fit();
term.focus();

if (!hostID) {
  setState("off", "缺少 host 参数");
  term.writeln("\x1b[31m用法: terminal.html?host=<host-id>\x1b[0m");
} else {
  targetEl.textContent = hostID;
  document.title = `labdeck · ${hostID}`;
  connect();
}

function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const base = location.pathname.replace(/\/[^/]*$/, "");
  const ws = new WebSocket(
    `${proto}//${location.host}${base}/api/ssh/${encodeURIComponent(hostID)}/ws`
  );
  ws.binaryType = "arraybuffer";

  ws.onopen = () => {
    // Sync the real terminal size before the shell draws its prompt.
    ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
  };

  ws.onmessage = (e) => {
    if (typeof e.data === "string") {
      const msg = JSON.parse(e.data);
      if (msg.type === "connected") {
        setState("on", msg.msg);
        targetEl.textContent = msg.msg;
      } else if (msg.type === "error") {
        setState("off", "出错");
        term.writeln(`\r\n\x1b[31m${msg.msg}\x1b[0m`);
      } else if (msg.type === "exit") {
        setState("off", "已断开");
        term.writeln(`\r\n\x1b[33m${msg.msg}\x1b[0m`);
      }
      return;
    }
    term.write(new Uint8Array(e.data));
  };

  ws.onclose = () => {
    if (stateEl.className !== "off") setState("off", "已断开");
    term.writeln("\r\n\x1b[90m连接已关闭。刷新页面可重连。\x1b[0m");
  };

  term.onData((data) => {
    if (ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: "input", data }));
    }
  });

  term.onResize(({ cols, rows }) => {
    if (ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: "resize", cols, rows }));
    }
  });
}

let resizeRaf = 0;
window.addEventListener("resize", () => {
  cancelAnimationFrame(resizeRaf);
  resizeRaf = requestAnimationFrame(() => fit.fit());
});
