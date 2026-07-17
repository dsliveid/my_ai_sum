const state = { token: localStorage.getItem("admin_token") || "", page: "dashboard", providers: [], proxies: [], models: [], localKeys: [], editingProviderId: "", editingProvider: null, editingProxyId: "", editingModelId: "", editingLocalKeyId: "", usageStart: "", usageEnd: "" };
const $ = (id) => document.getElementById(id);

function toast(msg) {
  const el = $("toast");
  el.textContent = formatErrorMessage(msg);
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
    try { msg = formatErrorPayload(await res.json(), msg); } catch {}
    throw new Error(msg);
  }
  const ct = res.headers.get("content-type") || "";
  return ct.includes("json") ? res.json() : res.text();
}

function html(strings, ...vals) {
  return strings.map((s, i) => s + (vals[i] ?? "")).join("");
}

function escapeHtml(v) {
  return String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function formatErrorMessage(err, fallback = "请求失败") {
  if (err instanceof Error) return err.message || fallback;
  return formatErrorPayload(err, fallback);
}

function formatErrorPayload(payload, fallback = "请求失败") {
  if (payload == null) return fallback;
  if (typeof payload === "string") {
    const parsed = tryParseJSON(payload);
    return parsed ? formatErrorPayload(parsed, payload) : (payload || fallback);
  }
  if (typeof payload !== "object") return String(payload);
  const lines = [];
  appendErrorObject(lines, payload.error, payload);
  appendObjectMessage(lines, payload);
  appendMetaLine(lines, "request_id", payload.request_id || payload.requestId);
  appendMetaLine(lines, "error_type", payload.error_type);
  appendMetaLine(lines, "upstream_status", payload.upstream_status);
  appendMetaLine(lines, "content_type", payload.content_type);
  appendMetaLine(lines, "body_preview", payload.body_preview);
  if (lines.length === 0) {
    try { return JSON.stringify(payload, null, 2); } catch { return fallback; }
  }
  return lines.join("\n");
}

function appendErrorObject(lines, err, root) {
  if (err == null) return;
  if (typeof err === "string") {
    lines.push(err);
    return;
  }
  if (typeof err !== "object") {
    lines.push(String(err));
    return;
  }
  const message = firstString(err.message, err.msg, err.error_description, err.detail);
  const code = firstString(err.type, err.code, err.status, err.param);
  if (message && code) lines.push(`${code}: ${message}`);
  else if (message) lines.push(message);
  else if (code) lines.push(code);
  else {
    try { lines.push(JSON.stringify(err, null, 2)); } catch {}
  }
  appendMetaLine(lines, "param", err.param && err.param !== code ? err.param : "");
  appendMetaLine(lines, "request_id", err.request_id || err.requestId || root?.request_id || root?.requestId);
}

function appendObjectMessage(lines, obj) {
  const message = firstString(obj.message, obj.msg, obj.error_description, obj.detail);
  const code = firstString(obj.type, obj.code, obj.status);
  if (!message && !code) return;
  const line = message && code ? `${code}: ${message}` : (message || code);
  if (!lines.includes(line)) lines.push(line);
}

function appendMetaLine(lines, key, value) {
  if (value == null || value === "") return;
  const line = `${key}: ${typeof value === "object" ? JSON.stringify(value) : value}`;
  if (!lines.includes(line)) lines.push(line);
}

function firstString(...values) {
  for (const value of values) {
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

async function boot() {
  document.querySelectorAll(".sidebar button").forEach((b) => {
    b.onclick = () => {
      if (!state.token) {
        showOnly("login");
        return;
      }
      state.page = b.dataset.page;
      render();
    };
  });
  $("setupBtn").onclick = setup;
  $("loginBtn").onclick = login;
  $("logoutBtn").onclick = logout;
  $("debugLogsBtn").onclick = openDebugLogs;
  try {
    const s = await api("/setup/status", { headers: {} });
    if (!s.initialized) return showOnly("setup");
    if (!state.token) return showOnly("login");
    await api("/auth/me");
    showOnly("app");
    await loadBasics();
    render();
  } catch {
    showOnly("login");
  }
}

function showOnly(id) {
  ["setup", "login", "app"].forEach((x) => $(x).classList.toggle("hidden", x !== id));
  $("sidebar").classList.toggle("hidden", id !== "app");
  $("logoutBtn").classList.toggle("hidden", id !== "app");
  $("debugLogsBtn").classList.toggle("hidden", id !== "app");
  $("status").textContent = id === "app" ? "运行中" : "等待操作";
}

function openDebugLogs() {
  window.open("/debug-logs.html", "_blank", "noopener");
}

async function setup() {
  try {
    await api("/setup/init", { method: "POST", body: JSON.stringify({ username: $("setupUser").value, password: $("setupPass").value }) });
    toast("初始化完成");
    showOnly("login");
  } catch (e) { toast(e.message); }
}

async function login() {
  try {
    const res = await api("/auth/login", { method: "POST", body: JSON.stringify({ username: $("loginUser").value, password: $("loginPass").value }) });
    state.token = res.token;
    localStorage.setItem("admin_token", state.token);
    showOnly("app");
    await loadBasics();
    render();
  } catch (e) { toast(e.message); }
}

function logout() {
  localStorage.removeItem("admin_token");
  state.token = "";
  showOnly("login");
}

async function loadBasics() {
  const [providers, proxies, models, localKeys] = await Promise.all([
    api("/provider-keys"), api("/proxies"), api("/model-mappings"), api("/local-api-keys")
  ]);
  state.providers = Array.isArray(providers) ? providers : [];
  state.proxies = Array.isArray(proxies) ? proxies : [];
  state.models = Array.isArray(models) ? models : [];
  state.localKeys = Array.isArray(localKeys) ? localKeys : [];
}

async function render() {
  document.querySelectorAll(".sidebar button").forEach((b) => b.classList.toggle("active", b.dataset.page === state.page));
  const pages = { dashboard, providers, proxies, models, localkeys, chat, usage, logs, settings };
  if (!pages[state.page]) state.page = "dashboard";
  try {
    await pages[state.page]();
  } catch (e) {
    toast(e.message || String(e));
    if (/token|unauthorized|401|expired|missing admin/i.test(e.message || "")) {
      localStorage.removeItem("admin_token");
      state.token = "";
      showOnly("login");
    }
  }
}

async function dashboard() {
  const [usage, logs] = await Promise.all([api("/usage/provider-keys"), api("/request-logs?limit=20")]);
  const usageRows = Array.isArray(usage) ? usage : [];
  const logRows = Array.isArray(logs) ? logs : [];
  const total = usageRows.reduce((s, x) => s + (x.total_tokens || 0), 0);
  $("page").innerHTML = html`
    <div class="panel">
      <h1>仪表盘</h1>
      <div class="cards">
        <div class="metric">外部服务<strong>${state.providers.length}</strong></div>
        <div class="metric">模型映射<strong>${state.models.length}</strong></div>
        <div class="metric">本地 Key<strong>${state.localKeys.length}</strong></div>
        <div class="metric">总 Token<strong>${formatTokens(total)}</strong></div>
      </div>
      <h2>最近请求</h2>${logTable(logRows)}
    </div>`;
}

function providerOptions(selected = "") {
  return state.providers.map((p) => `<option value="${p.id}" ${p.id === selected ? "selected" : ""}>${escapeHtml(p.name)} (${escapeHtml(p.provider_type)})</option>`).join("");
}

function proxyOptions(selected = "") {
  return `<option value="">无</option>` + state.proxies.map((p) => `<option value="${p.id}" ${p.id === selected ? "selected" : ""}>${escapeHtml(p.name)}</option>`).join("");
}

function protocolOptions(selected = "") {
  const items = [
    ["responses", "Responses"],
    ["chat_completions", "Chat Completions"]
  ];
  return items.map(([value, label]) => `<option value="${value}" ${value === selected ? "selected" : ""}>${label}</option>`).join("");
}

async function providers() {
  const editing = state.editingProvider || state.providers.find((p) => p.id === state.editingProviderId);
  $("page").innerHTML = html`
  <div class="panel"><h1>外部服务</h1>
    <div class="grid three">
      <label>名称<input id="pkName" value="${escapeHtml(editing?.name || "")}" autocomplete="off"></label>
      <label>类型<select id="pkType"><option value="relay" ${editing?.provider_type === "relay" ? "selected" : ""}>中转站</option><option value="deepseek" ${editing?.provider_type === "deepseek" ? "selected" : ""}>DeepSeek</option><option value="grok" ${editing?.provider_type === "grok" ? "selected" : ""}>Grok/xAI</option><option value="openai_compatible" ${editing?.provider_type === "openai_compatible" ? "selected" : ""}>OpenAI 兼容</option></select></label>
      <label>Base URL<input id="pkBase" value="${editing ? escapeHtml(editing.base_url || "") : ""}" placeholder="官方平台可留空" autocomplete="off"></label>
      <label>API Key<input id="pkKey" type="text" list="pkKeyOptions" value="${editing ? escapeHtml(editing.api_key || "") : ""}" autocomplete="off" autocapitalize="off" spellcheck="false" data-lpignore="true" data-1p-ignore="true" data-form-type="other" placeholder="${editing ? "" : ""}"><datalist id="pkKeyOptions"><option value="grok_device_oauth"></option></datalist></label>
      <label>模型列表<input id="pkModels" value="${escapeHtml(editing?.models || "")}" placeholder="deepseek-chat,grok-3-mini" autocomplete="off"></label>
      <label>请求协议<select id="pkRequestProtocol">${protocolOptions(editing?.request_protocol || "responses")}</select></label>
      <label>代理模式<select id="pkProxyMode"><option value="none" ${editing?.proxy_mode === "none" ? "selected" : ""}>不使用</option><option value="default" ${editing?.proxy_mode === "default" ? "selected" : ""}>默认代理</option><option value="custom" ${editing?.proxy_mode === "custom" ? "selected" : ""}>指定代理</option></select></label>
      <label>指定代理<select id="pkProxy">${proxyOptions(editing?.proxy_id || "")}</select></label>
      <label>启用<select id="pkEnabled"><option value="true" ${editing?.enabled !== false ? "selected" : ""}>启用</option><option value="false" ${editing?.enabled === false ? "selected" : ""}>停用</option></select></label>
    </div><div class="actions"><button id="addPk">${editing ? "保存 Key" : "新增 Key"}</button>${editing ? '<button id="copyPk" class="secondary">复制 API Key</button><button id="cancelPk" class="secondary">取消编辑</button>' : ""}</div>
    ${providerTable()}
  </div>`;
  $("addPk").onclick = saveProvider;
  $("pkType").onchange = updateProviderAuthControls;
  $("pkKey").oninput = updateProviderAuthControls;
  $("pkProxyMode").onchange = updateProviderProxyControls;
  updateProviderAuthControls();
  updateProviderProxyControls();
  if (!editing) {
    $("pkBase").value = "";
    $("pkKey").value = "";
  }
  if (editing) {
    $("copyPk").onclick = copyProviderKey;
    $("cancelPk").onclick = () => { state.editingProviderId = ""; state.editingProvider = null; providers(); };
  }
}

function providerTable() {
  return `<table><thead><tr><th>名称</th><th>类型</th><th>Base URL</th><th>请求协议</th><th>Key</th><th>模型</th><th>代理</th><th>状态</th><th>操作</th></tr></thead><tbody>` +
    state.providers.map((p) => `<tr><td>${escapeHtml(p.name)}</td><td>${p.provider_type}</td><td>${escapeHtml(p.base_url)}</td><td>${protocolLabel(p.request_protocol || "responses")}</td><td>${escapeHtml(p.api_key)}</td><td>${escapeHtml(p.models || "")}</td><td>${p.proxy_mode}</td><td>${p.enabled ? "启用" : "停用"} ${escapeHtml(p.last_check_status || "")}</td><td><button onclick="editProvider('${p.id}')">编辑</button> <button onclick="testProvider('${p.id}')">测试</button> <button class="danger" onclick="del('/provider-keys/${p.id}')">删除</button></td></tr>`).join("") +
    `</tbody></table>`;
}

function updateProviderProxyControls() {
  const mode = $("pkProxyMode")?.value || "none";
  if ($("pkProxy")) {
    $("pkProxy").disabled = mode !== "custom";
    if (mode !== "custom") $("pkProxy").value = "";
  }
}

function updateProviderAuthControls() {
  const type = $("pkType")?.value || "";
  const key = $("pkKey")?.value.trim() || "";
  if (type === "grok" && key === "grok_device_oauth") {
    const base = $("pkBase");
    if (base && (!base.value.trim() || base.value.trim() === "https://api.x.ai/v1")) {
      base.value = "https://cli-chat-proxy.grok.com/v1";
    }
    if ($("pkRequestProtocol")) $("pkRequestProtocol").value = "responses";
    const models = $("pkModels");
    if (models && !models.value.trim()) models.value = "grok-4.5";
  }
}

async function saveProvider() {
  try {
    const path = state.editingProviderId ? `/provider-keys/${state.editingProviderId}` : "/provider-keys";
    const method = state.editingProviderId ? "PUT" : "POST";
    await api(path, { method, body: JSON.stringify({
      name: $("pkName").value, provider_type: $("pkType").value, base_url: $("pkBase").value, request_protocol: $("pkRequestProtocol").value,
      api_key: $("pkKey").value, models: $("pkModels").value, proxy_mode: $("pkProxyMode").value,
      proxy_id: $("pkProxyMode").value === "custom" ? $("pkProxy").value : "", enabled: $("pkEnabled").value === "true"
    }) });
    toast(state.editingProviderId ? "已更新外部服务" : "已新增外部服务");
    state.editingProviderId = "";
    state.editingProvider = null;
    await loadBasics(); providers();
  } catch (e) { toast(e.message); }
}

async function editProvider(id) {
  try {
    state.editingProviderId = id;
    state.editingProvider = await api(`/provider-keys/${id}?reveal=1`);
    providers();
  } catch (e) { toast(e.message); }
}

function providerLabel(id) {
  const p = state.providers.find((x) => x.id === id);
  return p ? `${escapeHtml(p.name)} (${escapeHtml(p.provider_type)})` : escapeHtml(id || "");
}
async function copyProviderKey() {
  const key = $("pkKey").value;
  if (!key) {
    toast("API Key 为空");
    return;
  }
  try {
    await navigator.clipboard.writeText(key);
    toast("API Key 已复制");
  } catch {
    $("pkKey").select();
    document.execCommand("copy");
    toast("API Key 已复制");
  }
}
async function testProvider(id) {
  try {
    toast("测试中，如首次使用 grok_device_oauth 将显示授权码");
    const r = await api(`/provider-keys/${id}/test`, { method: "POST" });
    if (r.status === "authorization_required" || r.status === "authorization_pending") {
      let copied = false;
      if (r.user_code && navigator.clipboard?.writeText) {
        try {
          await navigator.clipboard.writeText(r.user_code);
          copied = true;
        } catch {}
      }
      if (r.authorization_url) window.open(r.authorization_url, "_blank", "noopener");
      alert(`请在 xAI 授权页面输入用户码：\n${r.user_code}\n\n${copied ? "用户码已复制到剪贴板，可直接粘贴。" : "如果没有自动填入，请手动复制上面的用户码。"}\n完成授权后，再点击一次“测试”。`);
    } else {
      toast(`${r.status} ${r.error || ""}`);
    }
    await loadBasics(); render();
  } catch (e) { toast(e.message); }
}

async function proxies() {
  const editing = state.proxies.find((p) => p.id === state.editingProxyId);
  $("page").innerHTML = html`
  <div class="panel"><h1>代理</h1>
    <div class="grid three">
      <label>名称<input id="pxName" value="${escapeHtml(editing?.name || "")}"></label><label>类型<select id="pxType"><option value="http" ${editing?.type === "http" ? "selected" : ""}>HTTP</option><option value="https" ${editing?.type === "https" ? "selected" : ""}>HTTPS</option><option value="socks5" ${editing?.type === "socks5" ? "selected" : ""}>SOCKS5</option></select></label>
      <label>地址<input id="pxHost" value="${escapeHtml(editing?.host || "127.0.0.1")}"></label><label>端口<input id="pxPort" type="number" value="${editing?.port || 7890}"></label>
      <label>用户名<input id="pxUser" value="${editing ? escapeHtml(editing.username || "") : ""}" autocomplete="off"></label><label>密码<input id="pxPass" type="password" value="" autocomplete="new-password" placeholder="${editing ? "留空则不修改" : ""}"></label>
      <label>默认<select id="pxDefault"><option value="false">否</option><option value="true" ${editing?.is_default ? "selected" : ""}>是</option></select></label>
      <label>启用<select id="pxEnabled"><option value="true" ${editing?.enabled !== false ? "selected" : ""}>启用</option><option value="false" ${editing?.enabled === false ? "selected" : ""}>停用</option></select></label>
    </div><p class="muted">本地 Clash、v2rayN 等代理地址为 http://127.0.0.1:7890 时，类型通常选择 HTTP，用户名和密码留空。</p><div class="actions"><button id="addPx">${editing ? "保存代理" : "新增代理"}</button>${editing ? '<button id="cancelPx" class="secondary">取消编辑</button>' : ""}</div>
    <table><thead><tr><th>名称</th><th>类型</th><th>地址</th><th>默认</th><th>状态</th><th>操作</th></tr></thead><tbody>${state.proxies.map((p) => `<tr><td>${escapeHtml(p.name)}</td><td>${p.type}</td><td>${p.host}:${p.port}</td><td>${p.is_default ? "是" : "否"}</td><td>${escapeHtml(p.last_check_status || "")}</td><td><button onclick="editProxy('${p.id}')">编辑</button> <button onclick="testProxy('${p.id}')">测试</button> <button class="danger" onclick="del('/proxies/${p.id}')">删除</button></td></tr>`).join("")}</tbody></table>
  </div>`;
  $("addPx").onclick = saveProxy;
  if (!editing) {
    $("pxUser").value = "";
    $("pxPass").value = "";
  }
  if (editing) $("cancelPx").onclick = () => { state.editingProxyId = ""; proxies(); };
}

async function saveProxy() {
  try {
    const path = state.editingProxyId ? `/proxies/${state.editingProxyId}` : "/proxies";
    const method = state.editingProxyId ? "PUT" : "POST";
    await api(path, { method, body: JSON.stringify({ name: $("pxName").value, type: $("pxType").value, host: $("pxHost").value, port: Number($("pxPort").value), username: $("pxUser").value, password: $("pxPass").value, is_default: $("pxDefault").value === "true", enabled: $("pxEnabled").value === "true" }) });
    toast(state.editingProxyId ? "已更新代理" : "已新增代理");
    state.editingProxyId = "";
    await loadBasics(); proxies();
  } catch (e) { toast(e.message); }
}
function editProxy(id) { state.editingProxyId = id; proxies(); }
async function testProxy(id) { try { const r = await api(`/proxies/${id}/test`, { method: "POST" }); toast(`${r.status} ${r.message || r.error || ""}`); await loadBasics(); render(); } catch (e) { toast(e.message); } }

async function models() {
  const editing = state.models.find((m) => m.id === state.editingModelId);
  $("page").innerHTML = html`
  <div class="panel"><h1>模型映射</h1>
    <div class="grid three"><label>本地模型<input id="mmLocal" value="${escapeHtml(editing?.local_model || "")}" autocomplete="off"></label><label>上游模型<input id="mmUp" value="${escapeHtml(editing?.upstream_model || "")}" autocomplete="off"></label><label>外部服务<select id="mmProvider">${providerOptions(editing?.provider_key_id || "")}</select></label><label>能力<select id="mmCap"><option value="chat" ${editing?.capability === "chat" ? "selected" : ""}>chat</option><option value="embedding" ${editing?.capability === "embedding" ? "selected" : ""}>embedding</option></select></label><label>启用<select id="mmEnabled"><option value="true" ${editing?.enabled !== false ? "selected" : ""}>启用</option><option value="false" ${editing?.enabled === false ? "selected" : ""}>停用</option></select></label></div>
    <div class="actions"><button id="addMm">${editing ? "保存映射" : "新增映射"}</button>${editing ? '<button id="cancelMm" class="secondary">取消编辑</button>' : ""}</div>
    <table><thead><tr><th>本地模型</th><th>上游模型</th><th>外部服务</th><th>外部服务ID</th><th>能力</th><th>状态</th><th>操作</th></tr></thead><tbody>${state.models.map((m) => `<tr><td>${m.local_model}</td><td>${m.upstream_model}</td><td>${providerLabel(m.provider_key_id)}</td><td>${m.provider_key_id}</td><td>${m.capability}</td><td>${m.enabled ? "启用" : "停用"}</td><td><button onclick="editModel('${m.id}')">编辑</button> <button class="danger" onclick="del('/model-mappings/${m.id}')">删除</button></td></tr>`).join("")}</tbody></table>
  </div>`;
  $("addMm").onclick = saveModel;
  if (editing) $("cancelMm").onclick = () => { state.editingModelId = ""; models(); };
}

async function saveModel() {
  try {
    const path = state.editingModelId ? `/model-mappings/${state.editingModelId}` : "/model-mappings";
    const method = state.editingModelId ? "PUT" : "POST";
    await api(path, { method, body: JSON.stringify({ local_model: $("mmLocal").value, upstream_model: $("mmUp").value, provider_key_id: $("mmProvider").value, capability: $("mmCap").value, enabled: $("mmEnabled").value === "true" }) });
    toast(state.editingModelId ? "已更新模型映射" : "已新增模型映射");
    state.editingModelId = "";
    await loadBasics(); models();
  } catch (e) { toast(e.message); }
}
function editModel(id) { state.editingModelId = id; models(); }

async function localkeys() {
  const editing = state.localKeys.find((k) => k.id === state.editingLocalKeyId);
  const baseUrl = `${location.origin}/v1`;
  $("page").innerHTML = html`<div class="panel"><h1>本地 API Key</h1>
  <div class="metric"><span class="muted">Base URL</span><strong id="localBaseUrl">${escapeHtml(baseUrl)}</strong><div class="actions"><button id="copyBaseUrl" class="secondary">复制 Base URL</button></div><div class="muted">客户端按 OpenAI 兼容格式调用，API Key 使用下方生成的本地 Key。</div></div>
  <div class="grid three">
    <label>名称<input id="lkName" placeholder="名称" value="${escapeHtml(editing?.name || "")}" autocomplete="off"></label>
    <label>外部服务<select id="lkProvider">${providerOptions(editing?.provider_key_id || "")}</select></label>
    <label>协议转换<select id="lkProtocolEnabled"><option value="false" ${editing?.protocol_conversion_enabled ? "" : "selected"}>不启用</option><option value="true" ${editing?.protocol_conversion_enabled ? "selected" : ""}>启用</option></select></label>
    <label>客户端协议<select id="lkClientProtocol">${protocolOptions(editing?.client_protocol || "responses")}</select></label>
    <label>上游协议<select id="lkUpstreamProtocol">${protocolOptions(editing?.upstream_protocol || defaultProviderProtocol(editing?.provider_key_id || ""))}</select></label>
    ${editing ? '<label>状态<select id="lkEnabled"><option value="true">启用</option><option value="false">停用</option></select></label>' : ""}
  </div>
  <div class="actions"><button id="addLk">${editing ? "保存 Key" : "生成 Key"}</button>${editing ? '<button id="cancelLk" class="secondary">取消编辑</button>' : ""}</div>
  <table><thead><tr><th>名称</th><th>ID</th><th>外部服务</th><th>协议转换</th><th>API Key</th><th>状态</th><th>最近使用</th><th>操作</th></tr></thead><tbody>${state.localKeys.map((k) => `<tr><td>${escapeHtml(k.name)}</td><td>${k.id}</td><td>${providerLabel(k.provider_key_id)}</td><td>${protocolConversionLabel(k)}</td><td>${escapeHtml(k.key_masked || "")}</td><td>${k.enabled ? "启用" : "停用"}</td><td>${formatDateTime(k.last_used_at)}</td><td><button onclick="copyLocalKey('${k.id}')">复制 Key</button> <button onclick="editLocalKey('${k.id}')">编辑</button> <button class="danger" onclick="del('/local-api-keys/${k.id}')">删除</button></td></tr>`).join("")}</tbody></table></div>`;
  $("copyBaseUrl").onclick = copyBaseUrl;
  if (editing) $("lkEnabled").value = editing.enabled ? "true" : "false";
  $("lkProvider").onchange = () => {
    $("lkUpstreamProtocol").value = defaultProviderProtocol($("lkProvider").value);
  };
  $("addLk").onclick = saveLocalKey;
  if (editing) $("cancelLk").onclick = () => { state.editingLocalKeyId = ""; localkeys(); };
}

function defaultProviderProtocol(providerID) {
  const provider = state.providers.find((p) => p.id === providerID) || state.providers[0];
  return provider?.request_protocol || "responses";
}

function protocolLabel(protocol) {
  if (protocol === "responses") return "Responses";
  if (protocol === "chat_completions") return "Chat Completions";
  return escapeHtml(protocol || "");
}

function protocolConversionLabel(k) {
  if (!k.protocol_conversion_enabled) return `未启用 (${protocolLabel(k.client_protocol || "responses")} -> ${protocolLabel(k.upstream_protocol || "responses")})`;
  return `${protocolLabel(k.client_protocol)} -> ${protocolLabel(k.upstream_protocol)}`;
}

async function copyBaseUrl() {
  const text = $("localBaseUrl").textContent;
  try {
    await navigator.clipboard.writeText(text);
    toast("Base URL 已复制");
  } catch {
    toast(text);
  }
}

async function saveLocalKey() {
  try {
    const conversionEnabled = $("lkProtocolEnabled").value === "true";
    const clientProtocol = $("lkClientProtocol").value;
    const upstreamProtocol = $("lkUpstreamProtocol").value;
    if (!conversionEnabled && clientProtocol !== upstreamProtocol) {
      const ok = confirm("客户端协议和上游协议不一致，不启用转换协议可能会存在上游 API 不支持客户端协议的问题，请确认是否保存。");
      if (!ok) return;
    }
    const payload = {
      name: $("lkName").value || "default",
      provider_key_id: $("lkProvider").value,
      protocol_conversion_enabled: conversionEnabled,
      client_protocol: clientProtocol,
      upstream_protocol: upstreamProtocol
    };
    if (state.editingLocalKeyId) {
      payload.enabled = $("lkEnabled").value === "true";
      await api(`/local-api-keys/${state.editingLocalKeyId}`, { method: "PUT", body: JSON.stringify(payload) });
      toast("已更新本地 Key");
      state.editingLocalKeyId = "";
    } else {
      const r = await api("/local-api-keys", { method: "POST", body: JSON.stringify(payload) });
      alert(`请立即保存本地 API Key：\n${r.key}`);
    }
    await loadBasics(); localkeys();
  } catch (e) { toast(e.message); }
}
function editLocalKey(id) { state.editingLocalKeyId = id; localkeys(); }

async function copyLocalKey(id) {
  try {
    const item = await api(`/local-api-keys/${id}?reveal=1`);
    if (!item.key) {
      toast("这个 Key 是旧版本生成的，未保存明文，无法恢复。请重新生成一个本地 Key。");
      return;
    }
    await navigator.clipboard.writeText(item.key);
    toast("本地 API Key 已复制");
  } catch (e) { toast(e.message); }
}

async function chat() {
  $("page").innerHTML = html`
  <div class="panel"><h1>API 对话测试</h1>
    <div class="grid three">
      <label>测试目标<select id="chatTarget"><option value="upstream">外部上游 API</option><option value="local">本地兼容 API</option></select></label>
      <label id="chatProviderWrap">外部服务<select id="chatProvider">${providerOptions()}</select></label>
      <label id="chatLocalKeyWrap">本地 Key<select id="chatLocalKey">${state.localKeys.map((k, i) => `<option value="${k.id}" ${i === 0 ? "selected" : ""}>${escapeHtml(k.name)}</option>`).join("")}</select></label>
      <label>模型<input id="chatModel" list="chatModelOptions" autocomplete="off"><datalist id="chatModelOptions"></datalist></label>
      <label>流式<select id="chatStream"><option value="false">否</option><option value="true">是</option></select></label>
      <label>temperature<input id="chatTemp" type="number" step="0.1" value="0.7"></label>
    </div>
    <label>系统提示词<textarea id="chatSystem">You are a helpful assistant.</textarea></label>
    <label>用户消息<textarea id="chatUser">hello</textarea></label>
    <div class="actions"><button id="runChat">发送测试</button></div>
    <div id="chatOut" class="chat-output"></div>
  </div>`;
  if (!$("chatTarget").value) $("chatTarget").value = "upstream";
  if (!$("chatProvider").value && state.providers[0]) $("chatProvider").value = state.providers[0].id;
  if (!$("chatLocalKey").value && state.localKeys[0]) $("chatLocalKey").value = state.localKeys[0].id;
  $("chatTarget").onchange = updateChatControls;
  $("chatProvider").onchange = updateChatControls;
  $("chatLocalKey").onchange = updateChatControls;
  updateChatControls();
  $("runChat").onclick = runChat;
}

function splitModels(models) {
  return String(models || "").split(",").map((x) => x.trim()).filter(Boolean);
}

function updateChatControls() {
  const target = $("chatTarget").value;
  $("chatProviderWrap").classList.toggle("hidden", target !== "upstream");
  $("chatLocalKeyWrap").classList.toggle("hidden", target !== "local");
  let models = [];
  if (target === "upstream") {
    const providerID = $("chatProvider").value || state.providers[0]?.id || "";
    if (!$("chatProvider").value && providerID) $("chatProvider").value = providerID;
    const provider = state.providers.find((p) => p.id === providerID);
    models = splitModels(provider?.models);
  } else {
    if (!$("chatLocalKey").value && state.localKeys[0]) $("chatLocalKey").value = state.localKeys[0].id;
    const localKey = state.localKeys.find((k) => k.id === $("chatLocalKey").value);
    const provider = state.providers.find((p) => p.id === localKey?.provider_key_id);
    models = splitModels(provider?.models);
  }
  if (models.length === 0) {
    $("chatModelOptions").innerHTML = "";
    $("chatModel").value = "";
    return;
  }
  $("chatModelOptions").innerHTML = models.map((m) => `<option value="${escapeHtml(m)}"></option>`).join("");
  $("chatModel").value = models[0];
}

async function runChat() {
  const out = $("chatOut");
  out.textContent = "";
  const target = $("chatTarget").value;
  if (target === "upstream" && !$("chatProvider").value) {
    out.textContent = "请先选择外部服务";
    return;
  }
  if (target === "local" && !$("chatLocalKey").value) {
    out.textContent = "请先生成本地 API Key";
    return;
  }
  if (!$("chatModel").value) {
    out.textContent = "请输入模型，或先在对应外部服务中配置模型列表";
    return;
  }
  const body = {
    model: $("chatModel").value,
    stream: $("chatStream").value === "true",
    temperature: Number($("chatTemp").value || 0.7),
    messages: [{ role: "system", content: $("chatSystem").value }, { role: "user", content: $("chatUser").value }]
  };
  if (target === "upstream") body.provider_key_id = $("chatProvider").value;
  if (target === "local") body.local_api_key_id = $("chatLocalKey").value;
  try {
    const res = await fetch(`/api/v1/test-chat/${target}`, { method: "POST", headers: { "Content-Type": "application/json", Authorization: `Bearer ${state.token}` }, body: JSON.stringify(body) });
    if (body.stream) {
      if (!res.ok) {
        const text = await res.text();
        const parsed = tryParseJSON(text);
        throw new Error(parsed ? formatErrorPayload(parsed, `HTTP ${res.status}`) : (text || `HTTP ${res.status}`));
      }
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        out.textContent += dec.decode(value);
      }
    } else {
      const text = await res.text();
      const parsed = tryParseJSON(text);
      if (!res.ok) {
        throw new Error(parsed ? formatErrorPayload(parsed, `HTTP ${res.status}`) : (text || `HTTP ${res.status}`));
      }
      out.textContent = parsed ? JSON.stringify(parsed, null, 2) : `响应不是 JSON，可能是外部服务地址返回了网页或网关错误页。\n\n${text}`;
    }
    await loadBasics();
  } catch (e) { out.textContent = formatErrorMessage(e); }
}

function tryParseJSON(text) {
  try { return JSON.parse(text); } catch { return null; }
}

async function usage() {
  const [byKey, byModel, daily, records] = await Promise.all([
    api(`/usage/provider-keys${usageParams()}`),
    api(`/usage/models${usageParams()}`),
    api(`/usage/daily${usageParams()}`),
    api(`/usage/records${usageParams({ limit: 50 })}`)
  ]);
  const byKeyRows = Array.isArray(byKey) ? byKey : [];
  const byModelRows = Array.isArray(byModel) ? byModel : [];
  const dailyRows = Array.isArray(daily) ? daily : [];
  const recordRows = Array.isArray(records) ? records : [];
  $("page").innerHTML = html`<div class="panel"><h1>Token 用量</h1>
    <div class="actions usage-filters">
      <label>开始日期<input id="usageStart" type="date" value="${escapeHtml(state.usageStart)}"></label>
      <label>结束日期<input id="usageEnd" type="date" value="${escapeHtml(state.usageEnd)}"></label>
      <button id="usageSearch">查询</button>
      <button class="secondary" data-usage-preset="this_week">本周</button>
      <button class="secondary" data-usage-preset="last_week">上周</button>
      <button class="secondary" data-usage-preset="this_month">本月</button>
      <button class="secondary" data-usage-preset="last_month">上月</button>
      <button id="usageClear" class="secondary">全部</button>
      <button id="exportUsage" class="secondary">导出 CSV</button>
    </div>
    <h2>按外部服务</h2>${usageTable(byKeyRows, "provider")}
    <h2>按模型</h2>${usageTable(byModelRows, "model")}
    <h2>按日期</h2><table><thead><tr><th>日期</th><th>外部服务</th><th>上游模型</th><th>请求</th><th>输入</th><th>输出</th><th>总量</th></tr></thead><tbody>${dailyRows.map((x) => `<tr><td>${x.date}</td><td>${providerLabel(x.provider_key_id)}</td><td>${escapeHtml(x.model || "")}</td><td>${x.request_count}</td><td>${tokenCell(x.prompt_tokens)}</td><td>${tokenCell(x.completion_tokens)}</td><td>${tokenCell(x.total_tokens)}</td></tr>`).join("")}</tbody></table>
    <h2>明细</h2>${usageRecordsTable(recordRows)}</div>`;
  $("usageSearch").onclick = () => {
    state.usageStart = $("usageStart").value;
    state.usageEnd = $("usageEnd").value;
    usage();
  };
  $("usageClear").onclick = () => {
    state.usageStart = "";
    state.usageEnd = "";
    usage();
  };
  document.querySelectorAll("[data-usage-preset]").forEach((btn) => {
    btn.onclick = () => {
      const range = usagePresetRange(btn.dataset.usagePreset);
      state.usageStart = range.start;
      state.usageEnd = range.end;
      usage();
    };
  });
  $("exportUsage").onclick = downloadUsage;
}

async function downloadUsage() {
  try {
    const res = await fetch(`/api/v1/usage/export${usageParams()}`, { headers: { Authorization: `Bearer ${state.token}` } });
    if (!res.ok) throw new Error(await res.text());
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "usage.csv";
    a.click();
    URL.revokeObjectURL(url);
  } catch (e) { toast(e.message); }
}

function usageParams(extra = {}) {
  const p = new URLSearchParams(extra);
  if (state.usageStart) p.set("start_date", state.usageStart);
  if (state.usageEnd) p.set("end_date", state.usageEnd);
  const s = p.toString();
  return s ? `?${s}` : "";
}

function usagePresetRange(name) {
  const today = startOfLocalDay(new Date());
  if (name === "this_week") {
    const start = addDays(today, 1 - weekDayMonday(today));
    return { start: formatLocalDate(start), end: formatLocalDate(addDays(start, 6)) };
  }
  if (name === "last_week") {
    const thisWeekStart = addDays(today, 1 - weekDayMonday(today));
    const start = addDays(thisWeekStart, -7);
    return { start: formatLocalDate(start), end: formatLocalDate(addDays(start, 6)) };
  }
  if (name === "this_month") {
    const start = new Date(today.getFullYear(), today.getMonth(), 1);
    const end = new Date(today.getFullYear(), today.getMonth() + 1, 0);
    return { start: formatLocalDate(start), end: formatLocalDate(end) };
  }
  if (name === "last_month") {
    const start = new Date(today.getFullYear(), today.getMonth() - 1, 1);
    const end = new Date(today.getFullYear(), today.getMonth(), 0);
    return { start: formatLocalDate(start), end: formatLocalDate(end) };
  }
  return { start: "", end: "" };
}

function startOfLocalDay(d) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

function weekDayMonday(d) {
  const day = d.getDay();
  return day === 0 ? 7 : day;
}

function addDays(d, days) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + days);
}

function formatLocalDate(d) {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

function formatDateTime(value) {
  if (!value) return "";
  const s = String(value);
  if (/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(s)) return escapeHtml(s);
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return escapeHtml(s.replace("T", " ").replace(/Z$/, ""));
  return `${formatLocalDate(d)} ${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}:${String(d.getSeconds()).padStart(2, "0")}`;
}

function formatTokens(value) {
  const n = Number(value || 0);
  const abs = Math.abs(n);
  if (abs >= 1000000) return `${trimUnit(n / 1000000)}M`;
  if (abs >= 1000) return `${trimUnit(n / 1000)}K`;
  return String(n);
}

function trimUnit(n) {
  return n.toFixed(n >= 10 ? 1 : 2).replace(/\.0+$/, "").replace(/(\.\d)0$/, "$1");
}

function tokenCell(value) {
  const n = Number(value || 0);
  return `<span title="${n.toLocaleString()}">${formatTokens(n)}</span>`;
}

function usageTable(rows, type) {
  return `<table><thead><tr><th>维度</th><th>请求</th><th>输入</th><th>输出</th><th>总量</th></tr></thead><tbody>${rows.map((x) => `<tr><td>${type === "provider" ? providerLabel(x.key) : escapeHtml(x.key)}</td><td>${x.request_count}</td><td>${tokenCell(x.prompt_tokens)}</td><td>${tokenCell(x.completion_tokens)}</td><td>${tokenCell(x.total_tokens)}</td></tr>`).join("")}</tbody></table>`;
}
function usageRecordsTable(rows) {
  return `<table><thead><tr><th>时间</th><th>来源</th><th>外部服务</th><th>上游模型</th><th>本地模型</th><th>输入</th><th>输出</th><th>总量</th><th>来源</th></tr></thead><tbody>${rows.map((x) => `<tr><td>${formatDateTime(x.created_at)}</td><td>${x.source}</td><td>${providerLabel(x.provider_key_id)}</td><td>${escapeHtml(x.upstream_model || "")}</td><td>${escapeHtml(x.local_model || "")}</td><td>${tokenCell(x.prompt_tokens)}</td><td>${tokenCell(x.completion_tokens)}</td><td>${tokenCell(x.total_tokens)}</td><td>${x.usage_source}</td></tr>`).join("")}</tbody></table>`;
}

async function logs() {
  const rows = await api("/request-logs?limit=200");
  $("page").innerHTML = `<div class="panel"><h1>请求日志</h1>${logTable(Array.isArray(rows) ? rows : [])}</div>`;
}
function logTable(rows) {
  return `<table><thead><tr><th>时间</th><th>来源</th><th>外部服务</th><th>上游模型</th><th>本地模型</th><th>状态</th><th>Token</th><th>耗时</th><th>错误</th></tr></thead><tbody>${rows.map((x) => `<tr><td>${formatDateTime(x.created_at)}</td><td>${x.source}</td><td>${providerLabel(x.provider_key_id)}</td><td>${escapeHtml(x.upstream_model || "")}</td><td>${escapeHtml(x.local_model || "")}</td><td class="${x.success ? "ok" : "bad"}">${x.status_code}</td><td>${x.prompt_tokens}/${x.completion_tokens}/${x.total_tokens}</td><td>${x.latency_ms}ms</td><td>${escapeHtml(x.error_message)}</td></tr>`).join("")}</tbody></table>`;
}

async function settings() {
  const s = await api("/settings");
  $("page").innerHTML = html`<div class="panel"><h1>系统设置</h1><div class="grid three"><label>监听地址<input id="setHost" value="${s.host}"></label><label>端口<input id="setPort" type="number" value="${s.port}"></label><label>自动打开浏览器<select id="setAuto"><option value="false">否</option><option value="true" ${s.auto_open ? "selected" : ""}>是</option></select></label></div><div class="actions"><button id="saveSettings">保存</button><button id="reloadSettings" class="secondary">保存并重载监听</button></div><p class="muted">监听地址和端口会在重载监听后立即生效；如果新地址不可用，当前服务会继续保持原监听。</p></div>`;
  $("saveSettings").onclick = () => saveSettings(false);
  $("reloadSettings").onclick = () => saveSettings(true);
}

async function saveSettings(reload) {
  try {
    const host = $("setHost").value.trim() || "127.0.0.1";
    const port = Number($("setPort").value);
    const current = await api("/settings");
    await api("/settings", { method: "PUT", body: JSON.stringify({ ...current, host, port, auto_open: $("setAuto").value === "true", log_level: current.log_level || "info" }) });
    if (!reload) {
      toast("设置已保存");
      return;
    }
    const r = await api("/settings/reload", { method: "POST", body: JSON.stringify({}) });
    toast("监听已重载");
    const nextURL = browserURLForListen(r.host, r.port);
    if (nextURL !== location.origin + "/") {
      setTimeout(() => { location.href = nextURL; }, 700);
    }
  } catch (e) { toast(e.message); }
}

function browserURLForListen(host, port) {
  let h = host || "127.0.0.1";
  if (h === "0.0.0.0" || h === "::") h = "localhost";
  if (h.includes(":") && !h.startsWith("[")) h = `[${h}]`;
  return `${location.protocol}//${h}:${port}/`;
}

async function del(path) {
  if (!confirm("确认删除？")) return;
  try { await api(path, { method: "DELETE" }); toast("已删除"); await loadBasics(); render(); } catch (e) { toast(e.message); }
}

window.testProvider = testProvider;
window.editProvider = editProvider;
window.testProxy = testProxy;
window.editProxy = editProxy;
window.editModel = editModel;
window.editLocalKey = editLocalKey;
window.copyLocalKey = copyLocalKey;
window.del = del;
boot();
