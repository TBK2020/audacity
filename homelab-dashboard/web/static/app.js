// labdeck dashboard frontend: renders the summary feed, keeps a live
// WebSocket connection (poll fallback), and lazily loads uptime strips.
"use strict";

const STATUS_WORD = { up: "正常", degraded: "降级", down: "宕机", pending: "待测" };
const URL_LABEL = { internal: "内网", external: "外网", tailscale: "Tailscale" };

const board = document.getElementById("board");
const emptyEl = document.getElementById("empty");
const tooltip = document.getElementById("tooltip");
const uptimeCache = new Map(); // service id -> [{date, up_ratio, samples}]

function render(summary) {
  const services = summary.services || [];
  renderHealth(services);
  if (!services.length) return;
  emptyEl.hidden = true;

  const groups = new Map();
  for (const svc of services) {
    if (!groups.has(svc.group)) groups.set(svc.group, []);
    groups.get(svc.group).push(svc);
  }
  // Groups with an outage float to the top of the wall.
  const ordered = [...groups.entries()].sort((a, b) => badness(b[1]) - badness(a[1]));

  board.querySelectorAll(".group").forEach((el) => el.remove());
  for (const [name, svcs] of ordered) {
    const section = document.createElement("section");
    section.className = "group";
    const h = document.createElement("h2");
    h.textContent = name;
    const grid = document.createElement("div");
    grid.className = "grid";
    svcs.sort((a, b) => rank(b.status) - rank(a.status));
    for (const svc of svcs) grid.appendChild(card(svc));
    section.append(h, grid);
    board.appendChild(section);
  }
}

const rank = (s) => ({ down: 3, degraded: 2, pending: 1, up: 0 }[s] ?? 0);
const badness = (svcs) => Math.max(...svcs.map((s) => rank(s.status)));

function card(svc) {
  const el = document.createElement("article");
  el.className = "card" + (svc.status === "down" ? " down" : "");

  const head = document.createElement("div");
  head.className = "card-head";
  const dot = document.createElement("span");
  dot.className = "status-dot " + svc.status;
  const name = document.createElement("span");
  name.className = "card-name";
  name.textContent = svc.name;
  const word = document.createElement("span");
  word.className = "status-word";
  word.textContent = STATUS_WORD[svc.status] || svc.status;
  head.append(dot, name, word);

  const meta = document.createElement("div");
  meta.className = "card-meta";
  if (svc.latency_ms > 0) {
    const lat = document.createElement("span");
    lat.className = "latency";
    lat.textContent = svc.latency_ms + " ms";
    meta.appendChild(lat);
  }
  const failing = (svc.checks || []).find((c) => c.status === "down" || c.status === "degraded");
  if (failing && failing.detail) {
    const why = document.createElement("span");
    why.textContent = failing.detail;
    meta.appendChild(why);
  }

  const strip = document.createElement("div");
  strip.className = "strip";
  strip.dataset.service = svc.id;
  strip.setAttribute("aria-label", svc.name + " 最近 30 天可用性");
  drawStrip(strip, uptimeCache.get(svc.id));
  hookStripTooltip(strip, svc);
  if (!uptimeCache.has(svc.id)) loadUptime(svc.id);

  el.append(head, meta, strip);

  const urls = Object.entries(svc.urls || {});
  if (urls.length) {
    const links = document.createElement("div");
    links.className = "card-links";
    for (const [kind, href] of urls) {
      const a = document.createElement("a");
      a.href = href;
      a.target = "_blank";
      a.rel = "noreferrer";
      a.textContent = URL_LABEL[kind] || kind;
      links.appendChild(a);
    }
    el.appendChild(links);
  }
  return el;
}

// drawStrip renders 30 day-cells, oldest→newest; missing days stay neutral.
function drawStrip(strip, buckets) {
  strip.textContent = "";
  const byDate = new Map((buckets || []).map((b) => [b.date, b]));
  for (let i = 29; i >= 0; i--) {
    const d = new Date(Date.now() - i * 864e5);
    const key = d.toISOString().slice(0, 10);
    const cell = document.createElement("span");
    cell.className = "cell";
    const b = byDate.get(key);
    if (b) {
      cell.className += b.up_ratio >= 0.999 ? " ok" : b.up_ratio >= 0.5 ? " part" : " bad";
      cell.dataset.tip = `${key} · 可用 ${(b.up_ratio * 100).toFixed(1)}%（${b.samples} 次探测）`;
    } else {
      cell.dataset.tip = `${key} · 无数据`;
    }
    strip.appendChild(cell);
  }
}

async function loadUptime(id) {
  uptimeCache.set(id, []); // claim slot so concurrent renders don't refetch
  try {
    const res = await fetch(`api/services/${encodeURIComponent(id)}/uptime?days=30`);
    if (!res.ok) return;
    const buckets = await res.json();
    uptimeCache.set(id, buckets);
    document
      .querySelectorAll(`.strip[data-service="${CSS.escape(id)}"]`)
      .forEach((el) => drawStrip(el, buckets));
  } catch {
    uptimeCache.delete(id);
  }
}

function hookStripTooltip(strip, svc) {
  strip.addEventListener("mousemove", (e) => {
    const tip = e.target.dataset && e.target.dataset.tip;
    if (!tip) return hideTip();
    tooltip.innerHTML = "";
    const title = document.createElement("div");
    title.textContent = svc.name;
    const sub = document.createElement("div");
    sub.className = "tip-sub";
    sub.textContent = tip;
    tooltip.append(title, sub);
    tooltip.hidden = false;
    const pad = 12;
    let x = e.clientX + pad, y = e.clientY + pad;
    const r = tooltip.getBoundingClientRect();
    if (x + r.width > innerWidth - 8) x = e.clientX - r.width - pad;
    if (y + r.height > innerHeight - 8) y = e.clientY - r.height - pad;
    tooltip.style.left = x + "px";
    tooltip.style.top = y + "px";
  });
  strip.addEventListener("mouseleave", hideTip);
}
const hideTip = () => (tooltip.hidden = true);

function renderHealth(services) {
  const up = services.filter((s) => s.status === "up" || s.status === "degraded").length;
  document.getElementById("health-count").textContent = `${up}/${services.length}`;
  document.getElementById("health").classList.toggle(
    "has-down",
    services.some((s) => s.status === "down")
  );
}

async function loadEvents() {
  try {
    const res = await fetch("api/events?limit=20");
    if (!res.ok) return;
    const events = await res.json();
    const body = document.getElementById("events-body");
    body.textContent = "";
    if (!events.length) {
      const tr = document.createElement("tr");
      tr.innerHTML = '<td colspan="4" class="muted">暂无事件</td>';
      body.appendChild(tr);
      return;
    }
    for (const ev of events) {
      const tr = document.createElement("tr");
      const cells = [
        new Date(ev.time).toLocaleString("zh-CN", { hour12: false }),
        ev.service_id,
        `${STATUS_WORD[ev.from_status] || ev.from_status} → ${STATUS_WORD[ev.to_status] || ev.to_status}`,
        ev.reason || "",
      ];
      cells.forEach((text, i) => {
        const td = document.createElement("td");
        td.textContent = text;
        if (i === 2) td.className = ev.to_status === "down" ? "to-down" : "to-up";
        tr.appendChild(td);
      });
      body.appendChild(tr);
    }
  } catch {
    /* keep last table on transient errors */
  }
}

// ── Live feed: WebSocket with poll fallback ────────────
const conn = document.getElementById("conn");
const connText = document.getElementById("conn-text");
let pollTimer = null;

function setConn(state, text) {
  conn.className = "conn " + state;
  connText.textContent = text;
}

function connectWS() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const base = location.pathname.replace(/\/$/, "");
  const ws = new WebSocket(`${proto}//${location.host}${base}/api/ws`);
  ws.onopen = () => {
    setConn("live", "实时");
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  };
  ws.onmessage = (e) => {
    const msg = JSON.parse(e.data);
    if (msg.type === "summary") {
      render(msg.data);
      loadEvents();
    }
  };
  ws.onclose = () => {
    setConn("lost", "已断开，轮询中");
    if (!pollTimer) pollTimer = setInterval(poll, 15000);
    setTimeout(connectWS, 5000);
  };
}

async function poll() {
  try {
    const res = await fetch("api/summary");
    if (res.ok) render(await res.json());
  } catch { /* next tick */ }
}

poll();
loadEvents();
connectWS();
