const $ = (id) => document.getElementById(id);
const state = {
  token: localStorage.getItem("admin_token") || "",
  segment: null,
  timer: null,
  dirty: false,
  expanded: false
};

function toast(msg) {
  const el = $("toast");
  el.textContent = msg;
  el.classList.remove("hidden");
  setTimeout(() => el.classList.add("hidden"), 3200);
}

async function api(path, opts = {}) {
  const headers = opts.headers || {};
  if (!(opts.body instanceof FormData)) headers["Content-Type"] = "application/json";
  if (state.token) headers.Authorization = `Bearer ${state.token}`;
  const res = await fetch(`/api/v1${path}`, { ...opts, headers });
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch {}
    throw new Error(msg);
  }
  const ct = res.headers.get("content-type") || "";
  return ct.includes("json") ? res.json() : res.text();
}

async function boot() {
  if (!state.token) {
    $("viewer").value = "未登录，请先在主页面登录。";
    return;
  }
  $("refreshBtn").onclick = () => loadLogs(false);
  $("saveSettingsBtn").onclick = saveSettings;
  $("realtime").onchange = updateRealtime;
  $("wrapLines").onchange = updateWrapLines;
  $("expandBtn").onclick = toggleExpanded;
  $("viewer").oninput = () => { state.dirty = true; updateMeta(); };
  $("limit").onchange = () => loadLogs(false);
  window.addEventListener("keydown", (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
      e.preventDefault();
      saveLogs();
    }
  });
  await loadSettings();
  await loadLogs(false);
}

async function loadSettings() {
  const s = await api("/settings");
  $("enabled").value = String(Boolean(s.api_debug_enabled));
  $("level").value = s.api_debug_level || "info";
  $("requestBody").value = String(Boolean(s.api_debug_request_body));
  $("responseBody").value = String(Boolean(s.api_debug_response_body));
  $("maxBody").value = s.api_debug_max_body_chars || 4000;
}

async function saveSettings() {
  try {
    const s = await api("/settings");
    await api("/settings", {
      method: "PUT",
      body: JSON.stringify({
        ...s,
        api_debug_enabled: $("enabled").value === "true",
        api_debug_level: $("level").value,
        api_debug_request_body: $("requestBody").value === "true",
        api_debug_response_body: $("responseBody").value === "true",
        api_debug_max_body_chars: Number($("maxBody").value || 4000)
      })
    });
    toast("调试日志设置已保存");
  } catch (e) { toast(e.message); }
}

async function loadLogs(fromTimer) {
  if (fromTimer && state.dirty) return;
  try {
    const limit = Math.min(Math.max(Number($("limit").value || 100), 1), 5000);
    const res = await api(`/api-debug-logs?limit=${limit}`);
    state.segment = res;
    $("viewer").value = res.content || "";
    if (!res.content) $("viewer").value = "暂无调试日志";
    state.dirty = false;
    $("viewer").scrollTop = $("viewer").scrollHeight;
    updateMeta();
  } catch (e) {
    $("viewer").value = e.message;
    state.dirty = false;
    updateMeta();
  }
}

async function saveLogs() {
  if (!state.segment?.editable || !state.segment.file) {
    toast("当前没有可保存的日志片段");
    return;
  }
  try {
    await api("/api-debug-logs/save", {
      method: "POST",
      body: JSON.stringify({
        file: state.segment.file,
        start_line: state.segment.start_line,
        end_line: state.segment.end_line,
        content: $("viewer").value
      })
    });
    toast("日志已保存");
    state.dirty = false;
    await loadLogs(false);
  } catch (e) { toast(e.message); }
}

function updateRealtime() {
  if (state.timer) {
    clearInterval(state.timer);
    state.timer = null;
  }
  if ($("realtime").checked) {
    state.timer = setInterval(() => loadLogs(true), 2000);
    loadLogs(true);
  }
}

function toggleExpanded() {
  state.expanded = !state.expanded;
  document.body.classList.toggle("expanded", state.expanded);
  $("expandBtn").textContent = state.expanded ? "还原" : "放大";
}

function updateMeta() {
  const s = state.segment;
  const dirty = state.dirty ? "，未保存" : "";
  if (!s?.file) {
    $("meta").textContent = `暂无日志${dirty}`;
    return;
  }
  $("meta").textContent = `${s.file} 第 ${s.start_line}-${s.end_line} 行 / 共 ${s.total_lines} 行${dirty}`;
}

function updateWrapLines() {
  $("viewer").classList.toggle("wrap-lines", $("wrapLines").checked);
}

boot();
