// labdeck resource top view: node → VM/LXC → container tree with live
// CPU/memory bars, collapsible branches, and a flattened Top-N mode.
"use strict";

const KIND_WORD = { node: "节点", vm: "VM", lxc: "LXC", container: "容器" };
const root = document.getElementById("top-root");
const emptyEl = document.getElementById("top-empty");
const refreshedText = document.getElementById("refreshed-text");

let mode = "tree"; // tree | flat
let latest = null;
const collapsed = new Set();

document.getElementById("mode-tree").addEventListener("click", () => setMode("tree"));
document.getElementById("mode-flat").addEventListener("click", () => setMode("flat"));

function setMode(m) {
  mode = m;
  document.getElementById("mode-tree").classList.toggle("on", m === "tree");
  document.getElementById("mode-flat").classList.toggle("on", m === "flat");
  document.getElementById("mode-tree").setAttribute("aria-selected", m === "tree");
  document.getElementById("mode-flat").setAttribute("aria-selected", m === "flat");
  if (latest) render(latest);
}

async function refresh() {
  try {
    const res = await fetch("api/top");
    if (!res.ok) return;
    latest = await res.json();
    render(latest);
    refreshedText.textContent =
      "更新于 " + new Date(latest.generated_at).toLocaleTimeString("zh-CN", { hour12: false });
  } catch {
    refreshedText.textContent = "刷新失败，重试中…";
  }
}

function render(data) {
  const integrations = data.integrations || [];
  emptyEl.hidden = integrations.length > 0;
  root.querySelectorAll(".integration").forEach((el) => el.remove());

  for (const integ of integrations) {
    const section = document.createElement("section");
    section.className = "integration group";
    const h = document.createElement("h2");
    h.textContent = `${integ.name || integ.id} · ${integ.type}`;
    section.appendChild(h);

    if (integ.error) {
      const err = document.createElement("p");
      err.className = "integ-error";
      err.textContent = "采集失败：" + integ.error + (integ.resources.length ? "（显示最后一次成功数据）" : "");
      section.appendChild(err);
    }

    if (integ.resources.length) {
      section.appendChild(mode === "tree" ? treeTable(integ) : flatTable(integ));
    } else if (!integ.error) {
      const p = document.createElement("p");
      p.className = "muted";
      p.textContent = integ.fetched_at ? "无资源" : "等待首次采集…";
      section.appendChild(p);
    }
    root.appendChild(section);
  }
}

function tableSkeleton() {
  const table = document.createElement("table");
  table.className = "top-table";
  table.innerHTML = `<thead><tr>
    <th class="col-name">名称</th><th>类型</th><th>状态</th>
    <th class="col-bar">CPU</th><th class="col-bar">内存</th><th class="col-num">磁盘</th>
  </tr></thead>`;
  return table;
}

function treeTable(integ) {
  const byParent = new Map();
  for (const r of integ.resources) {
    const key = r.parent_id || "";
    if (!byParent.has(key)) byParent.set(key, []);
    byParent.get(key).push(r);
  }
  const table = tableSkeleton();
  const tbody = document.createElement("tbody");
  const walk = (parentKey, depth) => {
    const children = (byParent.get(parentKey) || []).slice().sort(byCPUDesc);
    for (const r of children) {
      const hasKids = byParent.has(r.id);
      tbody.appendChild(row(integ.id, r, depth, hasKids));
      if (hasKids && !collapsed.has(integ.id + "/" + r.id)) walk(r.id, depth + 1);
    }
  };
  walk("", 0);
  table.appendChild(tbody);
  return table;
}

function flatTable(integ) {
  // Top-N leaderboard: guests and containers only, nodes excluded.
  const rows = integ.resources
    .filter((r) => r.kind !== "node" && r.status === "running")
    .sort(byCPUDesc)
    .slice(0, 20);
  const table = tableSkeleton();
  const tbody = document.createElement("tbody");
  for (const r of rows) tbody.appendChild(row(integ.id, r, 0, false));
  table.appendChild(tbody);
  return table;
}

// Sort key: absolute load (share of allocation × allocated cores) so a busy
// 8-core VM outranks a saturated 1-core LXC.
const absLoad = (r) => (r.cpu_pct || 0) * (r.cpu_cores || 1);
const byCPUDesc = (a, b) => absLoad(b) - absLoad(a);

function row(integID, r, depth, hasKids) {
  const tr = document.createElement("tr");
  if (r.status !== "running") tr.className = "dim";

  const name = document.createElement("td");
  name.className = "col-name";
  name.style.paddingLeft = 8 + depth * 22 + "px";
  if (hasKids) {
    const key = integID + "/" + r.id;
    const btn = document.createElement("button");
    btn.className = "fold";
    btn.textContent = collapsed.has(key) ? "▸" : "▾";
    btn.setAttribute("aria-label", collapsed.has(key) ? "展开" : "折叠");
    btn.addEventListener("click", () => {
      collapsed.has(key) ? collapsed.delete(key) : collapsed.add(key);
      render(latest);
    });
    name.appendChild(btn);
  } else {
    const pad = document.createElement("span");
    pad.className = "fold-pad";
    name.appendChild(pad);
  }
  name.appendChild(document.createTextNode(r.name));

  const kind = document.createElement("td");
  kind.textContent = KIND_WORD[r.kind] || r.kind;
  kind.className = "muted";

  const status = document.createElement("td");
  const dot = document.createElement("span");
  dot.className = "status-dot " + (r.status === "running" ? "up" : "pending");
  status.append(dot, " " + (r.status === "running" ? "运行" : r.status));

  const cpu = document.createElement("td");
  cpu.className = "col-bar";
  if (r.status === "running") {
    cpu.appendChild(bar(r.cpu_pct, cpuLabel(r)));
  }

  const mem = document.createElement("td");
  mem.className = "col-bar";
  if (r.mem_total > 0 && r.status === "running") {
    const used = r.mem_used || 0; // omitted by the API when zero
    const pct = (used / r.mem_total) * 100;
    mem.appendChild(bar(pct, `${fmtBytes(used)} / ${fmtBytes(r.mem_total)}`));
  }

  const disk = document.createElement("td");
  disk.className = "col-num muted";
  if (r.disk_total > 0) {
    disk.textContent = `${fmtBytes(r.disk_used)} / ${fmtBytes(r.disk_total)}`;
  }

  tr.append(name, kind, status, cpu, mem, disk);
  return tr;
}

function cpuLabel(r) {
  const pct = (r.cpu_pct || 0).toFixed(1) + "%";
  return r.cpu_cores ? `${pct} × ${r.cpu_cores} 核` : pct;
}

// Utilization bar: neutral blue, warning ≥70%, critical ≥90%; value as text
// beside the bar so meaning never rides on color alone.
function bar(pct, label) {
  if (!Number.isFinite(pct)) pct = 0; // never let NaN reach the width style
  const wrap = document.createElement("div");
  wrap.className = "meter";
  const track = document.createElement("div");
  track.className = "meter-track";
  const fill = document.createElement("div");
  fill.className = "meter-fill" + (pct >= 90 ? " crit" : pct >= 70 ? " warn" : "");
  fill.style.width = Math.min(100, Math.max(0.5, pct)) + "%";
  track.appendChild(fill);
  const text = document.createElement("span");
  text.className = "meter-text";
  text.textContent = label;
  wrap.append(track, text);
  return wrap;
}

function fmtBytes(n) {
  if (!n || n < 0) return "0";
  const units = ["B", "K", "M", "G", "T"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return (n >= 10 || i === 0 ? Math.round(n) : n.toFixed(1)) + units[i];
}

// Refresh only while the page is visible — probes are cheap, don't be rude.
let timer = setInterval(tick, 10000);
function tick() {
  if (!document.hidden) refresh();
}
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refresh();
});
refresh();
