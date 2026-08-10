/* opencode-free-gateway 管理面板 — 调用 /admin/api/* */
const $ = (id) => document.getElementById(id);

let status = null;
let settings = null;

const api = {
  async get(path) { return (await fetch(path)).json(); },
  async post(path, body) {
    const r = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {}),
    });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data?.error?.message || "HTTP " + r.status);
    return data;
  },
  async put(path, body) {
    const r = await fetch(path, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {}),
    });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data?.error?.message || "HTTP " + r.status);
    return data;
  },
};

function toast(msg) {
  const el = $("toast");
  el.textContent = msg;
  el.classList.remove("hidden");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => el.classList.add("hidden"), 2600);
}

/* ---------- 移动端菜单 ---------- */
$("menuBtn").addEventListener("click", () => {
  $("topnav").classList.toggle("open");
});
document.querySelectorAll(".nav-btn").forEach((b) => {
  b.addEventListener("click", () => $("topnav").classList.remove("open"));
});

/* ---------- 登录 / 首次设置 ---------- */

let setupMode = false;

async function checkAuth() {
  const r = await fetch("/admin/api/status");
  if (r.ok) { showApp(); return; }
  try {
    const setup = await (await fetch("/admin/api/setup")).json();
    if (setup.setupRequired) {
      setupMode = true;
      $("setupHint").style.display = "";
      $("loginBtn").textContent = "设置密码";
    }
  } catch (_) {}
  showLogin();
}

$("loginBtn").addEventListener("click", async () => {
  const pass = $("loginPass").value;
  if (!pass) { $("loginErr").textContent = "请输入密码"; return; }
  try {
    if (setupMode) {
      await api.put("/admin/api/settings", { password: pass });
      setupMode = false;
      toast("密码已设置，请登录");
      $("setupHint").style.display = "none";
      $("loginBtn").textContent = "登录";
      $("loginPass").value = "";
      $("loginErr").textContent = "";
      return;
    }
    const d = await api.post("/admin/api/login", { password: pass });
    if (d.token) { showApp(); await refreshAll(); }
    else $("loginErr").textContent = "密码错误";
  } catch (e) {
    $("loginErr").textContent = e.message;
  }
});
$("loginPass").addEventListener("keydown", (e) => { if (e.key === "Enter") $("loginBtn").click(); });

function showApp() {
  $("loginView").classList.add("hidden");
  $("appView").classList.remove("hidden");
}
function showLogin() {
  $("appView").classList.add("hidden");
  $("loginView").classList.remove("hidden");
  $("loginPass").value = "";
  $("loginErr").textContent = "";
}

$("logoutBtn").addEventListener("click", async () => {
  await fetch("/admin/api/logout", { method: "POST" }).catch(() => {});
  showLogin();
});

/* ---------- 导航 ---------- */

document.querySelectorAll(".nav-btn").forEach((b) => {
  b.addEventListener("click", () => {
    document.querySelectorAll(".nav-btn").forEach((x) => x.classList.remove("active"));
    b.classList.add("active");
    document.querySelectorAll(".page").forEach((p) => p.classList.add("hidden"));
    $("page-" + b.dataset.page).classList.remove("hidden");
  });
});

/* ---------- 刷新 + 渲染 ---------- */

async function refreshAll() {
  try {
    status = await api.get("/admin/api/status");
    settings = await api.get("/admin/api/settings");
    renderChrome();
    renderOverview();
    renderWorkers();
    renderPool();
    renderCallKeys();
    renderGateway();
    renderUsage();
  } catch (e) {
    if (e instanceof TypeError && /Failed to fetch/.test(e.message)) {
      toast("服务器不可达");
    }
  }
}
$("refreshBtn").addEventListener("click", refreshAll);

function renderChrome() {
  const pill = $("runPill");
  pill.textContent = status.running ? "运行中" : "已停止";
  pill.className = "pill" + (status.running ? "" : " down");
}

function fmt(n) { return Number(n || 0).toLocaleString(); }
function fmtRate(r) { return r == null ? "—" : (r * 100).toFixed(1) + "%"; }
function esc(s) {
  return String(s ?? "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function renderOverview() {
  const cards = [
    { k: "上游", v: esc(status.baseUrl || "—") },
    { k: "Worker", v: status.workers?.length ?? 0 },
    { k: "代理", v: status.pool?.length ?? 0 },
    { k: "调用 Key", v: status.callKeyCount ?? 0 },
    { k: "鉴权", v: status.authEnabled ? "开启" : "关闭" },
  ];
  $("ovCards").innerHTML = cards.map((c) =>
    '<div class="stat-card"><div class="k">' + c.k + '</div><div class="v">' + c.v + '</div></div>').join("");

  const t = status.totals || {};
  $("ovTotals").textContent = fmt(t.TotalTokens ?? t.totalTokens ?? 0) + " tok 总计";

  const rows = status.workers || [];
  if (!rows.length) { $("ovWorkers").innerHTML = '<tr><td colspan="9" class="muted">暂无 worker</td></tr>'; return; }
  const statMap = {};
  (status.workerStats || []).forEach((s) => { statMap[s.AccountID || s.accountId] = s; });
  $("ovWorkers").innerHTML = rows.map((w) => {
    const st = statMap[w.id];
    const badge = '<span class="badge ' + (w.status === "banned-24h" ? "banned" : w.status === "cooldown" ? "cooldown" : "ready") + '">' + (w.status === "banned-24h" ? "封禁 24h" : w.status === "cooldown" ? "冷却中" : "就绪") + '</span>';
    const err = w.lastError
      ? '<div class="err-text" title="' + esc(w.lastError) + '">' + esc(w.lastError) + '</div>'
      : '<span class="muted">—</span>';
    return '<tr>' +
      '<td><strong>' + esc(w.id) + '</strong></td>' +
      '<td>' + badge + '</td>' +
      '<td class="mono">' + fmt(st?.RequestCount ?? st?.requestCount) + '</td>' +
      '<td class="mono" style="color:var(--ok)">' + fmt(st?.SuccessCount ?? st?.successCount) + '</td>' +
      '<td class="mono" style="color:var(--err)">' + fmt(st?.ErrorCount ?? st?.errorCount) + '</td>' +
      '<td class="mono">' + fmt(st?.TotalTokens ?? st?.totalTokens) + '</td>' +
      '<td class="mono">' + fmt(st?.CacheReadTokens ?? st?.cacheReadTokens) + ' / ' + fmt(st?.CacheWriteTokens ?? st?.cacheWriteTokens) + '</td>' +
      '<td class="mono">' + fmtRate(st?.rate) + '</td>' +
      '<td>' + err + '</td></tr>';
  }).join("");
}

/* ---------- worker ---------- */

function renderWorkers() {
  const box = $("workerList");
  const ws = (status?.workers || []);
  if (!ws.length) { box.innerHTML = '<div class="muted">暂无 worker — 点击添加。</div>'; return; }
  box.innerHTML = ws.map((w) => {
    const badge = '<span class="badge ' + (w.status === "banned-24h" ? "banned" : w.status === "cooldown" ? "cooldown" : "ready") + '">' + (w.status === "banned-24h" ? "封禁 24h" : w.status === "cooldown" ? "冷却中" : "就绪") + '</span>';
    const proxy = w.proxyId ? esc(w.proxyId) : '<span class="muted">直连</span>';
    return '<div class="worker-row">' +
      '<div class="grow"><div class="id">' + esc(w.id) + ' ' + badge + '</div>' +
      '<div class="meta">代理: ' + proxy + '</div>' +
      (w.lastError ? '<div class="err-text" title="' + esc(w.lastError) + '">' + esc(w.lastError) + '</div>' : '') +
      '</div>' +
      '<button class="btn btn-small" data-edit-worker="' + esc(w.id) + '">编辑</button>' +
      '<button class="btn btn-small danger" data-del-worker="' + esc(w.id) + '">删除</button>' +
      '</div>';
  }).join("");

  box.querySelectorAll("[data-del-worker]").forEach((b) => {
    b.addEventListener("click", async () => {
      const id = b.dataset.delWorker;
      if (!confirm("删除 worker " + id + "？统计历史会保留。")) return;
      try {
        await api.post("/admin/api/workers", { action: "delete", id });
        toast("已删除（统计保留）");
        await refreshAll();
      } catch (e) { toast(e.message); }
    });
  });
  box.querySelectorAll("[data-edit-worker]").forEach((b) => {
    b.addEventListener("click", () => openWorkerModal(b.dataset.editWorker));
  });
}

$("addWorkerBtn").addEventListener("click", () => openWorkerModal(""));

function openWorkerModal(id) {
  const isEdit = !!id;
  const w = isEdit ? (status?.workers || []).find((x) => x.id === id) : null;
  const poolOptions = (status?.pool || [])
    .map((p) => '<option value="' + esc(p.ID ?? p.id) + '"' + ((w?.proxyId === (p.ID ?? p.id)) ? " selected" : "") + '>' + esc(p.Name ?? p.name) + '</option>')
    .join("");
  openModal(isEdit ? "编辑 worker" : "添加 worker", `
    <label class="field">ID</label>
    <input class="input" id="wId" value="${esc(isEdit ? id : "")}" ${isEdit ? "readonly" : ""}>
    <label class="field">API Key</label>
    <input class="input mono" id="wKey" value="${esc(w?.apiKey ?? "")}" placeholder="sk-...">
    <label class="field">代理</label>
    <select class="input" id="wProxy">
      <option value="">（直连）</option>
      ${poolOptions}
    </select>
    <button class="btn btn-primary" style="margin-top:14px">保存</button>
  `, async (bodyBox) => {
    const spec = {
      action: "upsert",
      id: bodyBox.querySelector("#wId").value.trim(),
      apiKey: bodyBox.querySelector("#wKey").value,
      proxyId: bodyBox.querySelector("#wProxy").value || null,
    };
    if (!spec.id) { toast("ID 不能为空"); return; }
    await api.post("/admin/api/workers", spec);
    toast("已保存");
    await refreshAll();
  });
}

/* ---------- 代理池 ---------- */

function renderPool() {
  const box = $("poolList");
  const pool = status?.pool || [];
  if (!pool.length) { box.innerHTML = '<div class="muted">代理池为空。导入 TXT 或手动添加。</div>'; return; }
  box.innerHTML = pool.map((p) => {
    const mask = p.Username ? esc(p.Username) + ":****" : "";
    const proto = p.Type || "http";
    const state = p.Enabled && p.Usable
      ? '<span class="badge ready">正常</span>'
      : p.Enabled
        ? '<span class="badge cooldown">不可用</span>'
        : '<span class="badge off">已禁用</span>';
    return '<div class="pool-row">' +
      '<span class="addr" title="' + esc(p.Name ?? "") + '">' + proto + '://' + esc(mask) + '@' + esc(p.Host) + ':' + esc(p.Port) + '</span>' +
      state +
      '<button class="btn btn-small" data-toggle-proxy="' + esc(p.ID ?? p.id) + '">' + (p.Enabled ? "禁用" : "启用") + '</button>' +
      '<button class="btn btn-small danger" data-del-proxy="' + esc(p.ID ?? p.id) + '">删除</button>' +
      '</div>';
  }).join("");

  box.querySelectorAll("[data-del-proxy]").forEach((b) => {
    b.addEventListener("click", async () => {
      await api.post("/admin/api/pool/remove", { id: b.dataset.delProxy });
      toast("已删除");
      await refreshAll();
    });
  });
  box.querySelectorAll("[data-toggle-proxy]").forEach((b) => {
    b.addEventListener("click", async () => {
      await api.post("/admin/api/pool/toggle", { id: b.dataset.toggleProxy });
      await refreshAll();
    });
  });
}

$("txtImport").addEventListener("change", async (e) => {
  const file = e.target.files && e.target.files[0];
  e.target.value = "";
  if (!file) return;
  const text = await file.text();
  try {
    const d = await api.post("/admin/api/pool/import", { text });
    let msg = "导入 " + d.added + " 条 · 跳过 " + d.skipped;
    if (d.invalidLines && d.invalidLines.length) msg += " · 无效 " + d.invalidLines.length;
    $("importResult").textContent = msg + (d.invalidLines?.length ? " — " + d.invalidLines.slice(0, 3).join(" | ") : "");
    await refreshAll();
  } catch (err) { toast(err.message); }
});

$("probeBtn").addEventListener("click", async () => {
  try {
    const d = await api.post("/admin/api/pool/probe", {});
    const ok = Object.keys(d.latencies || {}).length;
    toast("探活完成：" + ok + " 个可达");
    await refreshAll();
  } catch (err) { toast(err.message); }
});

$("pruneBtn").addEventListener("click", async () => {
  if (!confirm("删除所有已禁用 / 不可用的代理？")) return;
  try {
    const d = await api.post("/admin/api/pool/prune", {});
    toast("已清理 " + d.removed + " 个失效代理");
    await refreshAll();
  } catch (err) { toast(err.message); }
});

/* ---------- 调用 Key ---------- */

function renderCallKeys() {
  const box = $("keyList");
  const keys = settings?.callKeys || [];
  if (!keys.length) { box.innerHTML = '<div class="muted">暂无调用 Key。</div>'; return; }
  box.innerHTML = keys.map((k) =>
    '<div class="worker-row"><div class="grow">' +
    '<div class="id mono">' + esc(k.Key) + '</div>' +
    '<div class="meta">' + esc(k.Name || "") + ' · ' + (k.Enabled ? "启用中" : "已禁用") + '</div></div>' +
    '<button class="btn btn-small" data-toggle-key="' + esc(k.Key) + '">' + (k.Enabled ? "禁用" : "启用") + '</button>' +
    '<button class="btn btn-small danger" data-del-key="' + esc(k.Key) + '">删除</button></div>').join("");
  box.querySelectorAll("[data-del-key]").forEach((b) => {
    b.addEventListener("click", async () => {
      await api.post("/admin/api/callkeys", { action: "delete", key: b.dataset.delKey });
      await refreshAll();
    });
  });
  box.querySelectorAll("[data-toggle-key]").forEach((b) => {
    b.addEventListener("click", async () => {
      const k = settings.callKeys.find((x) => x.Key === b.dataset.toggleKey);
      await api.post("/admin/api/callkeys", { action: "upsert", key: k.Key, name: k.Name, enabled: !k.Enabled });
      await refreshAll();
    });
  });
}

$("addKeyBtn").addEventListener("click", () => {
  openModal("添加调用 Key", `
    <label class="field">Key</label>
    <input class="input mono" id="kKey" placeholder="sk-client-...">
    <label class="field">备注（可选）</label>
    <input class="input" id="kName" placeholder="我的脚本">
    <button class="btn btn-primary" style="margin-top:14px">添加</button>
  `, async (bodyBox) => {
    await api.post("/admin/api/callkeys", {
      action: "upsert",
      key: bodyBox.querySelector("#kKey").value.trim(),
      name: bodyBox.querySelector("#kName").value.trim(),
      enabled: true,
    });
    toast("Key 已添加");
    await refreshAll();
  });
});

/* ---------- 网关 ---------- */

function renderGateway() {
  if (!settings) return;
  $("gBase").value = settings.baseUrl || "";
  $("gPort").value = settings.port || 9876;
  $("gUA").value = settings.cliUserAgent || "";
  $("gClient").value = settings.cliClient || "";
  $("gProject").value = settings.cliProject || "";
  $("gSynth").checked = !!settings.synthesizeCli;
  $("gFreeFilter").checked = !!settings.freeModelsFilter;
  $("gReqAuth").checked = !!settings.requireCallKeyAuth;
  $("gPassword").value = "";
}

$("saveGatewayBtn").addEventListener("click", async () => {
  try {
    await api.put("/admin/api/settings", {
      baseUrl: $("gBase").value,
      port: Number($("gPort").value),
      synthesizeCli: $("gSynth").checked,
      cliUserAgent: $("gUA").value,
      cliClient: $("gClient").value,
      cliProject: $("gProject").value,
      freeModelsFilter: $("gFreeFilter").checked,
      requireCallKeyAuth: $("gReqAuth").checked,
      password: $("gPassword").value || undefined,
    });
    toast("已保存");
    await refreshAll();
  } catch (e) { toast(e.message); }
});

/* ---------- 用量 ---------- */

function renderUsage() {
  const rows = status?.workerStats || [];
  $("usageBody").innerHTML = rows.map((s) => {
    const id = s.AccountID || s.accountId;
    return '<tr>' +
      '<td><strong>' + esc(id) + '</strong></td>' +
      '<td class="mono">' + fmt(s.RequestCount ?? s.requestCount) + '</td>' +
      '<td class="mono">' + fmt(s.ChatCount ?? s.chatCount) + '</td>' +
      '<td class="mono">' + fmt(s.ModelsCount ?? s.modelsCount) + '</td>' +
      '<td class="mono">' + fmt(s.PromptTokens ?? s.promptTokens) + '</td>' +
      '<td class="mono">' + fmt(s.CompletionTokens ?? s.completionTokens) + '</td>' +
      '<td class="mono"><strong>' + fmt(s.TotalTokens ?? s.totalTokens) + '</strong></td>' +
      '<td class="mono">' + fmt(s.CacheReadTokens ?? s.cacheReadTokens) + '</td>' +
      '<td class="mono">' + fmt(s.CacheWriteTokens ?? s.cacheWriteTokens) + '</td>' +
      '<td class="muted">' + esc(s.lastRequestAt ? new Date(s.lastRequestAt).toLocaleString() : "—") + '</td>' +
      '</tr>';
  }).join("");
}

/* ---------- 弹窗 ---------- */

function openModal(title, html, onSave) {
  $("modalTitle").textContent = title;
  $("modalBody").innerHTML = html;
  $("modal").classList.remove("hidden");
  const saveBtn = $("modalBody").querySelector(".btn-primary");
  if (saveBtn && onSave) {
    saveBtn.onclick = async () => {
      try {
        await onSave($("modalBody"));
        closeModal();
        await refreshAll();
      } catch (e) { toast(e.message); }
    };
  }
}
function closeModal() { $("modal").classList.add("hidden"); }
$("modalClose").addEventListener("click", closeModal);
$("modal").addEventListener("click", (e) => { if (e.target === $("modal")) closeModal(); });

/* ---------- 启动 ---------- */

(async function boot() {
  await checkAuth();
  if (!$("appView").classList.contains("hidden")) {
    await refreshAll();
  }
})();