/* opencode-free-gateway admin app — talks to /admin/api/* */
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

/* ---------- login / setup ---------- */

async function checkAuth() {
  const r = await fetch("/admin/api/status");
  if (r.ok) { showApp(); return; }
  // 401: login (or first-run setup)
  try {
    const setup = await (await fetch("/admin/api/setup")).json();
    if (setup.setupRequired) {
      setupMode = true;
      $("setupHint").style.display = "";
      $("loginBtn").textContent = "Set password";
    }
  } catch (_) {}
  showLogin();
}

let setupMode = false;

$("loginBtn").addEventListener("click", async () => {
  const pass = $("loginPass").value;
  try {
    if (setupMode) {
      // first run: password field acts as the initial setup
      await api.put("/admin/api/settings", { password: pass });
      setupMode = false;
      toast("Password set — sign in");
      $("setupHint").style.display = "none";
      $("loginBtn").textContent = "Sign in";
      $("loginPass").value = "";
      $("loginErr").textContent = "";
      return;
    }
    const d = await api.post("/admin/api/login", { password: pass });
    if (d.token) { showApp(); await refreshAll(); }
    else $("loginErr").textContent = "Wrong password";
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

/* ---------- navigation ---------- */

document.querySelectorAll(".nav-btn").forEach((b) => {
  b.addEventListener("click", () => {
    document.querySelectorAll(".nav-btn").forEach((x) => x.classList.remove("active"));
    b.classList.add("active");
    document.querySelectorAll(".page").forEach((p) => p.classList.add("hidden"));
    $("page-" + b.dataset.page).classList.remove("hidden");
  });
});

/* ---------- refresh + render ---------- */

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
      toast("Server unreachable");
    }
  }
}
$("refreshBtn").addEventListener("click", refreshAll);

function renderChrome() {
  const pill = $("runPill");
  pill.textContent = status.running ? "running" : "stopped";
  pill.className = "pill" + (status.running ? "" : " down");
}

function fmt(n) { return Number(n || 0).toLocaleString(); }
function fmtRate(r) { return r == null ? "—" : (r * 100).toFixed(1) + "%"; }
function esc(s) {
  return String(s ?? "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function renderOverview() {
  const cards = [
    { k: "Upstream", v: esc(status.baseUrl || "—") },
    { k: "Workers", v: status.workers?.length ?? 0 },
    { k: "Pool proxies", v: status.pool?.length ?? 0 },
    { k: "Call keys", v: status.callKeyCount ?? 0 },
    { k: "Auth required", v: status.authEnabled ? "on" : "off" },
  ];
  $("ovCards").innerHTML = cards.map((c) =>
    '<div class="stat-card"><div class="k">' + c.k + '</div><div class="v">' + c.v + '</div></div>').join("");

  const t = status.totals || {};
  $("ovTotals").textContent = fmt(t.TotalTokens ?? t.totalTokens ?? 0) + " tok total";

  const rows = status.workers || [];
  if (!rows.length) { $("ovWorkers").innerHTML = '<tr><td colspan="9" class="muted">No workers configured</td></tr>'; return; }
  // pair with stats
  const statMap = {};
  (status.workerStats || []).forEach((s) => { statMap[s.AccountID || s.accountId] = s; });
  $("ovWorkers").innerHTML = rows.map((w) => {
    const st = statMap[w.id];
    const badge = '<span class="badge ' + (w.status === "banned-24h" ? "banned" : w.status === "cooldown" ? "cooldown" : "ready") + '">' + (w.status === "banned-24h" ? "banned 24h" : w.status) + '</span>';
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

/* ---------- workers ---------- */

function renderWorkers() {
  const box = $("workerList");
  const ws = (status?.workers || []);
  if (!ws.length) { box.innerHTML = '<div class="muted">No workers yet — add one below.</div>'; return; }
  box.innerHTML = ws.map((w) => {
    const badge = '<span class="badge ' + (w.status === "banned-24h" ? "banned" : w.status === "cooldown" ? "cooldown" : "ready") + '">' + (w.status === "banned-24h" ? "banned 24h" : w.status) + '</span>';
    const proxy = w.proxyId ? esc(w.proxyId) : '<span class="muted">direct</span>';
    return '<div class="worker-row">' +
      '<div class="grow"><div class="id">' + esc(w.id) + ' ' + badge + '</div>' +
      '<div class="meta">proxy: ' + proxy + '</div>' +
      (w.lastError ? '<div class="err-text" title="' + esc(w.lastError) + '">' + esc(w.lastError) + '</div>' : '') +
      '</div>' +
      '<button class="btn btn-small" data-edit-worker="' + esc(w.id) + '">Edit</button>' +
      '<button class="btn btn-small danger" data-del-worker="' + esc(w.id) + '">Delete</button>' +
      '</div>';
  }).join("");

  box.querySelectorAll("[data-del-worker]").forEach((b) => {
    b.addEventListener("click", async () => {
      const id = b.dataset.delWorker;
      if (!confirm("Delete worker " + id + "? Its stats history is kept.")) return;
      try {
        await api.post("/admin/api/workers", { action: "delete", id });
        toast("Worker deleted (stats kept)");
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
  openModal(isEdit ? "Edit worker" : "Add worker", `
    <label class="field">ID</label>
    <input class="input" id="wId" value="${esc(isEdit ? id : "")}" ${isEdit ? "readonly" : ""}>
    <label class="field">API key</label>
    <input class="input mono" id="wKey" value="${esc(w?.apiKey ?? "")}" placeholder="sk-...">
    <label class="field">Proxy</label>
    <select class="input" id="wProxy">
      <option value="">(direct)</option>
      ${poolOptions}
    </select>
    <button class="btn btn-primary" style="margin-top:14px">Save</button>
  `, async (bodyBox) => {
    const spec = {
      action: "upsert",
      id: bodyBox.querySelector("#wId").value.trim(),
      apiKey: bodyBox.querySelector("#wKey").value,
      proxyId: bodyBox.querySelector("#wProxy").value || null,
    };
    if (!spec.id) { toast("ID required"); return; }
    await api.post("/admin/api/workers", spec);
    toast("Saved");
    await refreshAll();
  });
}

/* ---------- pool ---------- */

function renderPool() {
  const box = $("poolList");
  const pool = status?.pool || [];
  if (!pool.length) { box.innerHTML = '<div class="muted">Proxy pool is empty. Import a TXT or add manually.</div>'; return; }
  box.innerHTML = pool.map((p) => {
    const mask = p.Username ? esc(p.Username) + ":****" : "";
    const proto = p.Type || "http";
    const state = p.Enabled && p.Usable
      ? '<span class="badge ready">ok</span>'
      : p.Enabled
        ? '<span class="badge cooldown">unusable</span>'
        : '<span class="badge off">disabled</span>';
    return '<div class="pool-row">' +
      '<span class="addr" title="' + esc(p.Name ?? "") + '">' + proto + '://' + esc(mask) + '@' + esc(p.Host) + ':' + esc(p.Port) + '</span>' +
      state +
      '<button class="btn btn-small" data-toggle-proxy="' + esc(p.ID ?? p.id) + '">' + (p.Enabled ? "Disable" : "Enable") + '</button>' +
      '<button class="btn btn-small danger" data-del-proxy="' + esc(p.ID ?? p.id) + '">Delete</button>' +
      '</div>';
  }).join("");

  box.querySelectorAll("[data-del-proxy]").forEach((b) => {
    b.addEventListener("click", async () => {
      const pid = b.dataset.delProxy;
      const d = await api.post("/admin/api/pool/remove", { id: pid });
      toast("Removed");
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
    let msg = "Imported " + d.added + " · skipped " + d.skipped;
    if (d.invalidLines && d.invalidLines.length) msg += " · invalid " + d.invalidLines.length;
    $("importResult").textContent = msg + (d.invalidLines?.length ? " — " + d.invalidLines.slice(0, 3).join(" | ") : "");
    await refreshAll();
  } catch (err) { toast(err.message); }
});

$("probeBtn").addEventListener("click", async () => {
  try {
    const d = await api.post("/admin/api/pool/probe", {});
    const ok = Object.keys(d.latencies || {}).length;
    toast("Probe done: " + ok + " reachable");
    await refreshAll();
  } catch (err) { toast(err.message); }
});

$("pruneBtn").addEventListener("click", async () => {
  if (!confirm("Remove all disabled / unusable proxies?")) return;
  try {
    const d = await api.post("/admin/api/pool/prune", {});
    toast("Pruned " + d.removed + " dead proxies");
    await refreshAll();
  } catch (err) { toast(err.message); }
});

/* ---------- call keys ---------- */

function renderCallKeys() {
  const box = $("keyList");
  const keys = settings?.callKeys || [];
  if (!keys.length) { box.innerHTML = '<div class="muted">No call keys yet.</div>'; return; }
  box.innerHTML = keys.map((k) =>
    '<div class="worker-row"><div class="grow">' +
    '<div class="id mono">' + esc(k.Key) + '</div>' +
    '<div class="meta">' + esc(k.Name || "") + ' · ' + (k.Enabled ? "enabled" : "disabled") + '</div></div>' +
    '<button class="btn btn-small" data-toggle-key="' + esc(k.Key) + '">' + (k.Enabled ? "Disable" : "Enable") + '</button>' +
    '<button class="btn btn-small danger" data-del-key="' + esc(k.Key) + '">Delete</button></div>').join("");
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
  openModal("Add call key", `
    <label class="field">Key</label>
    <input class="input mono" id="kKey" placeholder="sk-client-...">
    <label class="field">Label (optional)</label>
    <input class="input" id="kName" placeholder="my script">
    <button class="btn btn-primary" style="margin-top:14px">Add</button>
  `, async (bodyBox) => {
    await api.post("/admin/api/callkeys", {
      action: "upsert",
      key: bodyBox.querySelector("#kKey").value.trim(),
      name: bodyBox.querySelector("#kName").value.trim(),
      enabled: true,
    });
    toast("Key added");
    await refreshAll();
  });
});

/* ---------- gateway ---------- */

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
    toast("Saved");
    await refreshAll();
  } catch (e) { toast(e.message); }
});

/* ---------- usage ---------- */

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

/* ---------- modal ---------- */

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

/* ---------- boot ---------- */

(async function boot() {
  await checkAuth();
  // if app is visible, load data
  if (!$("appView").classList.contains("hidden")) {
    await refreshAll();
  }
})();