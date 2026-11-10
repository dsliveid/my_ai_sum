/* ==========================================================================
   app.js —— 管理后台主应用逻辑
   公共运行时（$ / state / api(401 统一收口) / toast / html / raw / escapeHtml
   / setBusy / emptyRow / copyText）由 common.js 提供，必须先于本文件引入。
   注意：html`` 插值默认转义，需原样 HTML 时必须显式 raw(...)。
   ========================================================================== */

Object.assign(state, {
  page: "dashboard",
  providers: [],
  proxies: [],
  models: [],
  localKeys: [],
  editingProviderId: "",
  editingProvider: null,
  editingProxyId: "",
  editingModelId: "",
  editingLocalKeyId: "",
  usageStart: "",
  usageEnd: ""
});

const authPasswordInputIDs = ["setupPass", "loginPass", "oldPassword", "newPassword", "confirmPassword"];

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
  // 401 统一收口钩子：清 token 后切回登录视图
  onUnauthorized(() => {
    showOnly("login");
    toast("登录状态已失效，请重新登录");
  });
  document.addEventListener("click", handleDelegatedAction);
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
  $("changePasswordBtn").onclick = openPasswordDialog;
  $("savePasswordBtn").onclick = changePassword;
  $("cancelPasswordBtn").onclick = closePasswordDialog;
  $("passwordDialog").onclick = (e) => {
    if (e.target === $("passwordDialog")) closePasswordDialog();
  };
  closePasswordDialog();
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
  $("changePasswordBtn").classList.toggle("hidden", id !== "app");
  syncAuthInputState(id);
  if (id !== "app") closePasswordDialog();
  $("status").textContent = id === "app" ? "运行中" : "等待操作";
}

function syncAuthInputState(activeView) {
  setFieldsDisabled(["setupUser", "setupPass"], activeView !== "setup");
  setFieldsDisabled(["loginUser", "loginPass"], activeView !== "login");
  if (activeView !== "setup") clearPasswordFields(["setupPass"]);
  if (activeView !== "login") clearPasswordFields(["loginPass"]);
}

function setFieldsDisabled(ids, disabled) {
  ids.forEach((id) => {
    const el = $(id);
    if (el) el.disabled = disabled;
  });
}

function clearPasswordFields(ids = authPasswordInputIDs) {
  ids.forEach((id) => {
    const el = $(id);
    if (el) el.value = "";
  });
}

function openDebugLogs() {
  window.open("/debug-logs.html", "_blank", "noopener");
}

function openPasswordDialog() {
  setFieldsDisabled(["oldPassword", "newPassword", "confirmPassword"], false);
  clearPasswordFields(["oldPassword", "newPassword", "confirmPassword"]);
  $("passwordDialog").classList.remove("hidden");
  $("oldPassword").focus();
}

function closePasswordDialog() {
  $("passwordDialog").classList.add("hidden");
  clearPasswordFields(["oldPassword", "newPassword", "confirmPassword"]);
  setFieldsDisabled(["oldPassword", "newPassword", "confirmPassword"], true);
}

async function changePassword() {
  const oldPassword = $("oldPassword").value;
  const newPassword = $("newPassword").value;
  const confirmPassword = $("confirmPassword").value;
  if (newPassword.length < 6) {
    toast("新密码至少 6 位");
    return;
  }
  if (newPassword !== confirmPassword) {
    toast("两次输入的新密码不一致");
    return;
  }
  try {
    await api("/auth/password", { method: "POST", body: JSON.stringify({ old_password: oldPassword, new_password: newPassword }) });
    closePasswordDialog();
    toast("密码已修改");
  } catch (e) { toast(e.message); }
}

async function setup() {
  try {
    await api("/setup/init", { method: "POST", body: JSON.stringify({ username: $("setupUser").value, password: $("setupPass").value }) });
    clearPasswordFields(["setupPass"]);
    toast("初始化完成");
    showOnly("login");
  } catch (e) { toast(e.message); }
}

async function login() {
  try {
    const res = await api("/auth/login", { method: "POST", body: JSON.stringify({ username: $("loginUser").value, password: $("loginPass").value }) });
    clearPasswordFields(["loginPass"]);
    setToken(res.token);
    showOnly("app");
    await loadBasics();
    render();
  } catch (e) { toast(e.message); }
}

function logout() {
  clearToken();
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
    // 401 已由 common.js 的 api() 统一收口处理，此处只提示业务错误
    if (e && e.unauthorized) return;
    toast(e.message || String(e));
  }
}

function dashboard() { return renderDashboard(); }

async function renderDashboard() {
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
      <h2>最近请求</h2>${raw(logTable(logRows))}
    </div>`;
}

/* --------------------------------------------------------- 下拉/片段构造器 */
/* 以下函数返回「可信 HTML 片段」，内部自行 escapeHtml 处理数据，
   在 html`` 中插值时必须用 raw(...) 包裹。 */

function providerOptions(selected = "") {
  return state.providers.map((p) => `<option value="${escapeHtml(p.id)}" ${p.id === selected ? "selected" : ""}>${escapeHtml(p.name)} (${escapeHtml(p.provider_type)})</option>`).join("");
}

function proxyOptions(selected = "") {
  return `<option value="">无</option>` + state.proxies.map((p) => `<option value="${escapeHtml(p.id)}" ${p.id === selected ? "selected" : ""}>${escapeHtml(p.name)}</option>`).join("");
}

function protocolOptions(selected = "") {
  const items = [
    ["responses", "Responses"],
    ["chat_completions", "Chat Completions"],
    ["commandcode", "CommandCode（仅 Go 套餐）"]
  ];
  return items.map(([value, label]) => `<option value="${value}" ${value === selected ? "selected" : ""}>${escapeHtml(label)}</option>`).join("");
}

async function providers() {
  const editing = state.editingProvider || state.providers.find((p) => p.id === state.editingProviderId);
  $("page").innerHTML = html`
  <div class="panel"><h1>外部服务</h1>
    <div class="grid three">
      <label>名称<input id="pkName" name="provider_display_${Date.now()}" value="${editing?.name || ""}" autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false" data-lpignore="true" data-1p-ignore="true" data-form-type="other"></label>
      <label>类型<select id="pkType"><option value="relay" ${editing?.provider_type === "relay" ? "selected" : ""}>中转站</option><option value="deepseek" ${editing?.provider_type === "deepseek" ? "selected" : ""}>DeepSeek</option><option value="grok" ${editing?.provider_type === "grok" ? "selected" : ""}>Grok/xAI</option><option value="openai_compatible" ${editing?.provider_type === "openai_compatible" ? "selected" : ""}>OpenAI 兼容</option></select></label>
      <label>Base URL<input id="pkBase" name="provider_endpoint_${Date.now()}" value="${editing ? editing.base_url || "" : ""}" placeholder="官方平台可留空" autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false" data-lpignore="true" data-1p-ignore="true" data-form-type="other"></label>
      <label>API Key<input id="pkKey" name="provider_secret_${Date.now()}" type="text" list="pkKeyOptions" value="${editing ? editing.api_key || "" : ""}" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false" data-lpignore="true" data-1p-ignore="true" data-form-type="other" data-bwignore="true" placeholder="${editing ? "" : ""}"><datalist id="pkKeyOptions"><option value="grok_device_oauth"></option></datalist></label>
      <label>模型列表<input id="pkModels" name="provider_models_${Date.now()}" value="${editing?.models || ""}" placeholder="deepseek-chat,grok-3-mini" autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false" data-lpignore="true" data-1p-ignore="true" data-form-type="other"></label>
      <label>请求协议<select id="pkRequestProtocol">${raw(protocolOptions(editing?.request_protocol || "responses"))}</select>
        <span class="field-hint">「CommandCode（仅 Go 套餐）」仅适用于 CommandCode 订阅的 Go 套餐；其他套餐（Pro / Max / GOAT）请继续选择 chat_completions 或 responses。</span>
      </label>
      <label>代理模式<select id="pkProxyMode"><option value="none" ${editing?.proxy_mode === "none" ? "selected" : ""}>不使用</option><option value="default" ${editing?.proxy_mode === "default" ? "selected" : ""}>默认代理</option><option value="custom" ${editing?.proxy_mode === "custom" ? "selected" : ""}>指定代理</option></select></label>
      <label>指定代理<select id="pkProxy">${raw(proxyOptions(editing?.proxy_id || ""))}</select></label>
      <label>启用<select id="pkEnabled"><option value="true" ${editing?.enabled !== false ? "selected" : ""}>启用</option><option value="false" ${editing?.enabled === false ? "selected" : ""}>停用</option></select></label>
    </div><div class="actions"><button id="addPk" data-action="save-provider">${editing ? "保存 Key" : "新增 Key"}</button>${editing ? raw('<button id="copyPk" class="secondary" data-action="copy-provider-key">复制 API Key</button><button id="cancelPk" class="secondary" data-action="cancel-provider">取消编辑</button>') : ""}</div>
    ${raw(providerTable())}
  </div>`;
  $("pkType").onchange = updateProviderAuthControls;
  $("pkKey").oninput = updateProviderAuthControls;
  $("pkProxyMode").onchange = updateProviderProxyControls;
  updateProviderAuthControls();
  updateProviderProxyControls();
  if (!editing) {
    $("pkBase").value = "";
    $("pkKey").value = "";
  }
  for (const id of ["pkName", "pkBase", "pkKey", "pkModels"]) {
    const el = $(id);
    if (!el) continue;
    el.setAttribute("readonly", "readonly");
    setTimeout(() => el.removeAttribute("readonly"), 120);
  }
}

/** 表格单元格截断辅助：返回 RawHTML，自动带 title 悬停查看完整值 */
function truncCell(value, extraClass = "") {
  const text = String(value ?? "");
  if (!text) return raw("");
  const cls = extraClass ? `cell-ellipsis ${extraClass}` : "cell-ellipsis";
  return raw(`<span class="${cls}" title="${escapeHtml(text)}">${escapeHtml(text)}</span>`);
}

function providerTable() {
  const rows = state.providers.length
    ? state.providers.map((p) => {
      const deletePath = `/provider-keys/${p.id}`;
      return `<tr>
        <td>${escapeHtml(p.name)}</td>
        <td>${escapeHtml(p.provider_type)}</td>
        <td>${truncCell(p.base_url)}</td>
        <td>${protocolLabel(p.request_protocol || "responses")}</td>
        <td>${truncCell(p.api_key)}</td>
        <td>${truncCell(p.models || "")}</td>
        <td>${escapeHtml(p.proxy_mode)}</td>
        <td>${p.enabled ? "启用" : "停用"} ${escapeHtml(p.last_check_status || "")}</td>
        <td>
          <button data-action="edit-provider" data-id="${escapeHtml(p.id)}">编辑</button>
          <button data-action="test-provider" data-id="${escapeHtml(p.id)}">测试</button>
          <button class="danger" data-action="del" data-path="${escapeHtml(deletePath)}" data-label="外部服务「${escapeHtml(p.name)}」">删除</button>
        </td></tr>`;
    }).join("")
    : emptyRow(9, "还没有外部服务", "请在上方表单填写名称、Base URL 与 API Key 后点击「新增 Key」。");
  return `<table class="provider-table data-table"><thead><tr><th>名称</th><th>类型</th><th>Base URL</th><th>请求协议</th><th>Key</th><th>模型</th><th>代理</th><th>状态</th><th>操作</th></tr></thead><tbody>` + rows +
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
  const btn = $("addPk");
  const restore = setBusy(btn, true, "保存中...");
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
  } catch (e) { toast(e.message); } finally { restore(); }
}

async function editProvider(id) {
  try {
    state.editingProviderId = id;
    state.editingProvider = await api(`/provider-keys/${id}?reveal=1`);
    providers();
  } catch (e) { toast(e.message); }
}

/** 返回可信 HTML 片段（已转义），在 html`` 中插值需 raw() */
function providerLabel(id) {
  const p = state.providers.find((x) => x.id === id);
  return p ? `${escapeHtml(p.name)} (${escapeHtml(p.provider_type)})` : escapeHtml(id || "");
}
function splitModelList(models) {
  return String(models || "").split(",").map((x) => x.trim()).filter(Boolean);
}

function modelOptionValues(providerID, field) {
  const values = new Set(field === "local_model" ? ["all"] : []);
  const selectedProvider = state.providers.find((p) => p.id === providerID);
  const providers = selectedProvider ? [selectedProvider] : state.providers;
  providers.forEach((p) => splitModelList(p.models).forEach((model) => values.add(model)));
  state.models.forEach((m) => {
    const mappedProviderMatches = !providerID || m.provider_key_id === providerID;
    if (mappedProviderMatches && m[field]) values.add(m[field]);
  });
  return Array.from(values);
}

function modelDatalist(id, providerID, field) {
  return `<datalist id="${id}">${modelOptionValues(providerID, field).map((model) => `<option value="${escapeHtml(model)}"></option>`).join("")}</datalist>`;
}

function refreshModelMappingOptions() {
  const providerID = $("mmProvider")?.value || "";
  if ($("mmLocalOptions")) $("mmLocalOptions").innerHTML = modelOptionValues(providerID, "local_model").map((model) => `<option value="${escapeHtml(model)}"></option>`).join("");
  if ($("mmUpOptions")) $("mmUpOptions").innerHTML = modelOptionValues(providerID, "upstream_model").map((model) => `<option value="${escapeHtml(model)}"></option>`).join("");
}

async function copyProviderKey() {
  const key = $("pkKey").value;
  if (!key) {
    toast("API Key 为空");
    return;
  }
  try {
    if (await copyText(key)) {
      toast("API Key 已复制");
    } else {
      $("pkKey").select();
      toast("复制失败，请手动复制");
    }
  } catch {
    $("pkKey").select();
    toast("复制失败，请手动复制");
  }
}
function showGrokOAuthPrompt(providerID, result) {
  const code = result.user_code || "";
  const url = result.authorization_url || grokOAuthConsentURL(code) || result.verification_uri || "https://accounts.x.ai/oauth2/device";
  const existing = $("grokOAuthPrompt");
  if (existing) existing.remove();
  const modal = document.createElement("div");
  modal.id = "grokOAuthPrompt";
  modal.className = "modal";
  modal.innerHTML = html`
    <div class="dialog grok-oauth-dialog">
      <h2>Grok Device OAuth 授权</h2>
      <div class="oauth-instructions">
        <p>用户码：<strong id="grokOAuthCode" tabindex="0">${code}</strong></p>
        <p>授权页：点击测试后程序会自动打开授权页，如果没有自动打开，可以手动点击打开授权页打开</p>
      </div>
      <div class="actions">
        <button id="openGrokOAuthPage" type="button">打开授权页</button>
        <button id="finishGrokOAuth" type="button">我已授权</button>
        <button id="closeGrokOAuthPrompt" class="secondary" type="button">关闭</button>
      </div>
    </div>`;
  document.body.appendChild(modal);
  $("openGrokOAuthPage").onclick = () => window.open(url, "_blank", "noopener");
  $("finishGrokOAuth").onclick = () => finishGrokOAuth(providerID, modal);
  $("closeGrokOAuthPrompt").onclick = () => modal.remove();
  modal.onclick = (e) => {
    if (e.target === modal) modal.remove();
  };
  $("grokOAuthCode").onclick = () => {
    const range = document.createRange();
    range.selectNodeContents($("grokOAuthCode"));
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
  };
}

async function finishGrokOAuth(providerID, modal) {
  const button = $("finishGrokOAuth");
  if (button) {
    button.disabled = true;
    button.textContent = "获取中...";
  }
  try {
    const r = await api(`/provider-keys/${providerID}/test`, { method: "POST" });
    if (isGrokOAuthPendingStatus(r.status)) {
      toast("还没有检测到授权完成，请在授权页完成授权后再点击“我已授权”。");
      if (button) {
        button.disabled = false;
        button.textContent = "我已授权";
      }
      return;
    }
    modal.remove();
    toast(`${r.status} ${r.error || ""}`);
    await loadBasics();
    await providers();
  } catch (e) {
    toast(e.message);
    if (button) {
      button.disabled = false;
      button.textContent = "我已授权";
    }
  }
}

function isGrokOAuthPendingStatus(status) {
  return status === "authorization_required" || status === "authorization_pending";
}

function grokOAuthConsentURL(code) {
  const value = String(code || "").trim();
  if (!value) return "";
  return `https://accounts.x.ai/oauth2/device/consent?user_code=${encodeURIComponent(value)}`;
}

async function testProvider(id) {
  try {
    toast("测试中，如首次使用 grok_device_oauth 将显示授权码");
    const r = await api(`/provider-keys/${id}/test`, { method: "POST" });
    if (isGrokOAuthPendingStatus(r.status)) {
      await loadBasics();
      await providers();
      showGrokOAuthPrompt(id, r);
      if (r.authorization_url) window.open(r.authorization_url, "_blank", "noopener");
      return;
    } else {
      toast(`${r.status} ${r.error || ""}`);
    }
    await loadBasics(); render();
  } catch (e) { toast(e.message); }
}

async function proxies() {
  const editing = state.proxies.find((p) => p.id === state.editingProxyId);
  const proxyRows = state.proxies.length
    ? state.proxies.map((p) => `<tr>
        <td>${escapeHtml(p.name)}</td>
        <td>${escapeHtml(p.type)}</td>
        <td>${escapeHtml(p.host)}:${escapeHtml(p.port)}</td>
        <td>${p.is_default ? "是" : "否"}</td>
        <td>${escapeHtml(p.last_check_status || "")}</td>
        <td>
          <button data-action="edit-proxy" data-id="${escapeHtml(p.id)}">编辑</button>
          <button data-action="test-proxy" data-id="${escapeHtml(p.id)}">测试</button>
          <button class="danger" data-action="del" data-path="${escapeHtml(`/proxies/${p.id}`)}" data-label="代理「${escapeHtml(p.name)}」">删除</button>
        </td></tr>`).join("")
    : emptyRow(6, "还没有配置代理", "如需通过本地 Clash / v2rayN 出网，请填写地址与端口后点击「新增代理」；直连可跳过本页。");
  $("page").innerHTML = html`
  <div class="panel"><h1>代理</h1>
    <div class="grid three">
      <label>名称<input id="pxName" value="${editing?.name || ""}"></label>
      <label>类型<select id="pxType"><option value="http" ${editing?.type === "http" ? "selected" : ""}>HTTP</option><option value="https" ${editing?.type === "https" ? "selected" : ""}>HTTPS</option><option value="socks5" ${editing?.type === "socks5" ? "selected" : ""}>SOCKS5</option></select></label>
      <label>地址<input id="pxHost" value="${editing?.host || "127.0.0.1"}"></label>
      <label>端口<input id="pxPort" type="number" value="${editing?.port || 7890}"></label>
      <label>用户名<input id="pxUser" value="${editing ? editing.username || "" : ""}" autocomplete="off"></label>
      <label>密码<input id="pxPass" type="password" value="" autocomplete="new-password" placeholder="${editing ? "留空则不修改" : ""}"></label>
      <label>默认<select id="pxDefault"><option value="false">否</option><option value="true" ${editing?.is_default ? "selected" : ""}>是</option></select></label>
      <label>启用<select id="pxEnabled"><option value="true" ${editing?.enabled !== false ? "selected" : ""}>启用</option><option value="false" ${editing?.enabled === false ? "selected" : ""}>停用</option></select></label>
    </div><p class="muted">本地 Clash、v2rayN 等代理地址为 http://127.0.0.1:7890 时，类型通常选择 HTTP，用户名和密码留空。</p>
    <div class="actions"><button id="addPx" data-action="save-proxy">${editing ? "保存代理" : "新增代理"}</button>${editing ? raw('<button id="cancelPx" class="secondary" data-action="cancel-proxy">取消编辑</button>') : ""}</div>
    <table class="data-table"><thead><tr><th>名称</th><th>类型</th><th>地址</th><th>默认</th><th>状态</th><th>操作</th></tr></thead><tbody>${raw(proxyRows)}</tbody></table>
  </div>`;
  if (!editing) {
    $("pxUser").value = "";
    $("pxPass").value = "";
  }
}

async function saveProxy() {
  const restore = setBusy($("addPx"), true, "保存中...");
  try {
    const path = state.editingProxyId ? `/proxies/${state.editingProxyId}` : "/proxies";
    const method = state.editingProxyId ? "PUT" : "POST";
    await api(path, { method, body: JSON.stringify({ name: $("pxName").value, type: $("pxType").value, host: $("pxHost").value, port: Number($("pxPort").value), username: $("pxUser").value, password: $("pxPass").value, is_default: $("pxDefault").value === "true", enabled: $("pxEnabled").value === "true" }) });
    toast(state.editingProxyId ? "已更新代理" : "已新增代理");
    state.editingProxyId = "";
    await loadBasics(); proxies();
  } catch (e) { toast(e.message); } finally { restore(); }
}
function editProxy(id) { state.editingProxyId = id; proxies(); }
async function testProxy(id) { try { const r = await api(`/proxies/${id}/test`, { method: "POST" }); toast(`${r.status} ${r.message || r.error || ""}`); await loadBasics(); render(); } catch (e) { toast(e.message); } }

async function models() {
  const editing = state.models.find((m) => m.id === state.editingModelId);
  const selectedProviderID = editing?.provider_key_id || state.providers[0]?.id || "";
  const modelRows = state.models.length
    ? state.models.map((m) => `<tr>
        <td>${truncCell(m.local_model)}</td>
        <td>${truncCell(m.upstream_model)}</td>
        <td>${raw(providerLabel(m.provider_key_id))}</td>
        <td>${truncCell(m.provider_key_id)}</td>
        <td>${escapeHtml(m.capability)}</td>
        <td>${m.enabled ? "启用" : "停用"}</td>
        <td>
          <button data-action="edit-model" data-id="${escapeHtml(m.id)}">编辑</button>
          <button class="danger" data-action="del" data-path="${escapeHtml(`/model-mappings/${m.id}`)}" data-label="模型映射「${escapeHtml(m.local_model)}」">删除</button>
        </td></tr>`).join("")
    : emptyRow(7, "还没有模型映射", "请填写本地模型名与上游真实模型名，并选择对应的外部服务后点击「新增映射」。");
  $("page").innerHTML = html`
  <div class="panel"><h1>模型映射</h1>
    <div class="grid three">
      <label>本地模型<input id="mmLocal" list="mmLocalOptions" value="${editing?.local_model || ""}" autocomplete="off">${raw(modelDatalist("mmLocalOptions", selectedProviderID, "local_model"))}</label>
      <label>上游模型<input id="mmUp" list="mmUpOptions" value="${editing?.upstream_model || ""}" autocomplete="off">${raw(modelDatalist("mmUpOptions", selectedProviderID, "upstream_model"))}</label>
      <label>外部服务<select id="mmProvider">${raw(providerOptions(editing?.provider_key_id || ""))}</select></label>
      <label>能力<select id="mmCap"><option value="chat" ${editing?.capability === "chat" ? "selected" : ""}>chat</option><option value="embedding" ${editing?.capability === "embedding" ? "selected" : ""}>embedding</option></select></label>
      <label>启用<select id="mmEnabled"><option value="true" ${editing?.enabled !== false ? "selected" : ""}>启用</option><option value="false" ${editing?.enabled === false ? "selected" : ""}>停用</option></select></label>
    </div>
    <div class="actions"><button id="addMm" data-action="save-model">${editing ? "保存映射" : "新增映射"}</button>${editing ? raw('<button id="cancelMm" class="secondary" data-action="cancel-model">取消编辑</button>') : ""}</div>
    <table class="data-table"><thead><tr><th>本地模型</th><th>上游模型</th><th>外部服务</th><th>外部服务ID</th><th>能力</th><th>状态</th><th>操作</th></tr></thead><tbody>${raw(modelRows)}</tbody></table>
  </div>`;
  $("mmProvider").onchange = refreshModelMappingOptions;
  refreshModelMappingOptions();
}

async function saveModel() {
  const restore = setBusy($("addMm"), true, "保存中...");
  try {
    const path = state.editingModelId ? `/model-mappings/${state.editingModelId}` : "/model-mappings";
    const method = state.editingModelId ? "PUT" : "POST";
    await api(path, { method, body: JSON.stringify({ local_model: $("mmLocal").value, upstream_model: $("mmUp").value, provider_key_id: $("mmProvider").value, capability: $("mmCap").value, enabled: $("mmEnabled").value === "true" }) });
    toast(state.editingModelId ? "已更新模型映射" : "已新增模型映射");
    state.editingModelId = "";
    await loadBasics(); models();
  } catch (e) { toast(e.message); } finally { restore(); }
}
function editModel(id) { state.editingModelId = id; models(); }

async function localkeys() {
  const editing = state.localKeys.find((k) => k.id === state.editingLocalKeyId);
  const baseUrl = `${location.origin}/v1`;
  const keyRows = state.localKeys.length
    ? state.localKeys.map((k) => `<tr>
        <td>${escapeHtml(k.name)}</td>
        <td>${truncCell(k.id)}</td>
        <td>${raw(providerLabel(k.provider_key_id))}</td>
        <td>${raw(protocolConversionLabel(k))}</td>
        <td>${truncCell(k.key_masked || "")}</td>
        <td>${k.enabled ? "启用" : "停用"}</td>
        <td>${raw(formatDateTime(k.last_used_at))}</td>
        <td>
          <button data-action="copy-local-key" data-id="${escapeHtml(k.id)}">复制 Key</button>
          <button data-action="edit-local-key" data-id="${escapeHtml(k.id)}">编辑</button>
          <button class="danger" data-action="del" data-path="${escapeHtml(`/local-api-keys/${k.id}`)}" data-label="本地 API Key「${escapeHtml(k.name)}」">删除</button>
        </td></tr>`).join("")
    : emptyRow(8, "还没有本地 API Key", "请填写名称、选择外部服务后点击「生成 Key」，客户端即可用它调用 /v1 接口。");
  $("page").innerHTML = html`<div class="panel"><h1>本地 API Key</h1>
  <div class="metric"><span class="muted">Base URL</span><strong id="localBaseUrl">${baseUrl}</strong><div class="actions"><button id="copyBaseUrl" class="secondary" data-action="copy-base-url">复制 Base URL</button></div><div class="muted">客户端按 OpenAI 兼容格式调用，API Key 使用下方生成的本地 Key。</div></div>
  <div class="grid three">
    <label>名称<input id="lkName" placeholder="名称" value="${editing?.name || ""}" autocomplete="off"></label>
    <label>外部服务<select id="lkProvider">${raw(providerOptions(editing?.provider_key_id || ""))}</select></label>
    <label>协议转换<select id="lkProtocolEnabled"><option value="false" ${editing?.protocol_conversion_enabled ? "" : "selected"}>不启用</option><option value="true" ${editing?.protocol_conversion_enabled ? "selected" : ""}>启用</option></select></label>
    <label>客户端协议<select id="lkClientProtocol">${raw(protocolOptions(editing?.client_protocol || "responses"))}</select></label>
    <label>上游协议<select id="lkUpstreamProtocol">${raw(protocolOptions(editing?.upstream_protocol || defaultProviderProtocol(editing?.provider_key_id || "")))}</select></label>
    ${editing ? raw('<label>状态<select id="lkEnabled"><option value="true">启用</option><option value="false">停用</option></select></label>') : ""}
  </div>
  <div class="actions"><button id="addLk" data-action="save-local-key">${editing ? "保存 Key" : "生成 Key"}</button>${editing ? raw('<button id="cancelLk" class="secondary" data-action="cancel-local-key">取消编辑</button>') : ""}</div>
  <p class="danger-note">删除本地 API Key 会立即失效，正在使用该 Key 的客户端将无法继续调用网关，请在确认无业务依赖后再执行删除。</p>
  <table class="data-table"><thead><tr><th>名称</th><th>ID</th><th>外部服务</th><th>协议转换</th><th>API Key</th><th>状态</th><th>最近使用</th><th>操作</th></tr></thead><tbody>${raw(keyRows)}</tbody></table></div>`;
  $("copyBaseUrl").onclick = copyBaseUrl;
  if (editing) $("lkEnabled").value = editing.enabled ? "true" : "false";
  $("lkProvider").onchange = () => {
    $("lkUpstreamProtocol").value = defaultProviderProtocol($("lkProvider").value);
  };
}

function defaultProviderProtocol(providerID) {
  const provider = state.providers.find((p) => p.id === providerID) || state.providers[0];
  return provider?.request_protocol || "responses";
}

/** 返回可信 HTML 片段（已转义），在 html`` 中插值需 raw() */
function protocolLabel(protocol) {
  if (protocol === "responses") return "Responses";
  if (protocol === "chat_completions") return "Chat Completions";
  if (protocol === "commandcode") return "CommandCode（仅 Go 套餐）";
  return escapeHtml(protocol || "");
}

function protocolConversionLabel(k) {
  if (!k.protocol_conversion_enabled) return `未启用 (${protocolLabel(k.client_protocol || "responses")} -> ${protocolLabel(k.upstream_protocol || "responses")})`;
  return `${protocolLabel(k.client_protocol)} -> ${protocolLabel(k.upstream_protocol)}`;
}

async function copyBaseUrl() {
  const text = $("localBaseUrl").textContent;
  if (await copyText(text)) {
    toast("Base URL 已复制");
  } else {
    toast("Base URL 复制失败，请手动复制： " + text);
  }
}

async function saveLocalKey() {
  const restore = setBusy($("addLk"), true, "处理中...");
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
  } catch (e) { toast(e.message); } finally { restore(); }
}
function editLocalKey(id) { state.editingLocalKeyId = id; localkeys(); }

async function copyLocalKey(id) {
  try {
    const item = await api(`/local-api-keys/${id}?reveal=1`);
    if (!item.key) {
      toast("这个 Key 是旧版本生成的，未保存明文，无法恢复。请重新生成一个本地 Key。");
      return;
    }
    if (await copyText(item.key)) {
      toast("本地 API Key 已复制");
    } else {
      toast("本地 API Key 复制失败，请手动复制");
    }
  } catch (e) { toast(e.message); }
}

async function chat() {
  const localKeyOptions = state.localKeys.map((k, i) => `<option value="${escapeHtml(k.id)}" ${i === 0 ? "selected" : ""}>${escapeHtml(k.name)}</option>`).join("");
  $("page").innerHTML = html`
  <div class="panel"><h1>API 对话测试</h1>
    <div class="grid three">
      <label>测试目标<select id="chatTarget"><option value="upstream">外部上游 API</option><option value="local">本地兼容 API</option></select></label>
      <label id="chatProviderWrap">外部服务<select id="chatProvider">${raw(providerOptions())}</select></label>
      <label id="chatLocalKeyWrap">本地 Key<select id="chatLocalKey">${raw(localKeyOptions)}</select></label>
      <label>模型<input id="chatModel" list="chatModelOptions" autocomplete="off"><datalist id="chatModelOptions"></datalist></label>
      <label>流式<select id="chatStream"><option value="false">否</option><option value="true">是</option></select></label>
      <label>temperature<input id="chatTemp" type="number" step="0.1" value="0.7"></label>
    </div>
    <label>系统提示词<textarea id="chatSystem">You are a helpful assistant.</textarea></label>
    <label>用户消息<textarea id="chatUser">hello</textarea></label>
    <div class="actions"><button id="runChat" data-action="run-chat">发送测试</button></div>
    <div id="chatOut" class="chat-output"></div>
  </div>`;
  if (!$("chatTarget").value) $("chatTarget").value = "upstream";
  if (!$("chatProvider").value && state.providers[0]) $("chatProvider").value = state.providers[0].id;
  if (!$("chatLocalKey").value && state.localKeys[0]) $("chatLocalKey").value = state.localKeys[0].id;
  $("chatTarget").onchange = updateChatControls;
  $("chatProvider").onchange = updateChatControls;
  $("chatLocalKey").onchange = updateChatControls;
  updateChatControls();
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
  const restore = setBusy($("runChat"), true, "请求中...");
  const target = $("chatTarget").value;
  if (target === "upstream" && !$("chatProvider").value) {
    restore();
    out.textContent = "请先选择外部服务";
    return;
  }
  if (target === "local" && !$("chatLocalKey").value) {
    restore();
    out.textContent = "请先生成本地 API Key";
    return;
  }
  if (!$("chatModel").value) {
    restore();
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
  } catch (e) { out.textContent = formatErrorMessage(e); } finally { restore(); }
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
  const dailyBody = dailyRows.length
    ? dailyRows.map((x) => `<tr>
        <td>${escapeHtml(x.date)}</td>
        <td>${raw(providerLabel(x.provider_key_id))}</td>
        <td>${truncCell(x.model || "")}</td>
        <td>${Number(x.request_count || 0)}</td>
        <td>${raw(tokenCell(x.prompt_tokens))}</td>
        <td>${raw(cacheHitCell(x))}</td>
        <td>${raw(tokenCell(x.completion_tokens))}</td>
        <td>${raw(tokenCell(x.total_tokens))}</td></tr>`).join("")
    : emptyRow(8, "所选时间范围内暂无按日统计", "可点击上方「本周 / 本月」快捷按钮切换范围，或先发起一次 API 调用。");
  $("page").innerHTML = html`<div class="panel"><h1>Token 用量</h1>
    <div class="actions usage-filters">
      <label>开始日期<input id="usageStart" type="date" value="${state.usageStart}"></label>
      <label>结束日期<input id="usageEnd" type="date" value="${state.usageEnd}"></label>
      <button data-action="usage-search">查询</button>
      <button class="secondary" data-action="usage-preset" data-preset="this_week">本周</button>
      <button class="secondary" data-action="usage-preset" data-preset="last_week">上周</button>
      <button class="secondary" data-action="usage-preset" data-preset="this_month">本月</button>
      <button class="secondary" data-action="usage-preset" data-preset="last_month">上月</button>
      <button class="secondary" data-action="usage-clear">全部</button>
      <button class="secondary" data-action="usage-export">导出 CSV</button>
    </div>
    <p class="field-hint">「缓存命中率」= 上游返回的 cached_tokens / 输入 tokens；输入为 0 或上游未返回缓存统计时显示 -。</p>
    <h2>按外部服务</h2>${raw(usageTable(byKeyRows, "provider"))}
    <h2>按模型</h2>${raw(usageTable(byModelRows, "model"))}
    <h2>按日期</h2><table class="data-table"><thead><tr><th>日期</th><th>外部服务</th><th>上游模型</th><th>请求</th><th>输入</th><th>缓存命中率</th><th>输出</th><th>总量</th></tr></thead><tbody>${raw(dailyBody)}</tbody></table>
    <h2>明细</h2>${raw(usageRecordsTable(recordRows))}</div>`;
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

/** 缓存命中率单元格：cached_tokens / prompt_tokens，prompt_tokens 为 0 时显示 "-" */
function cacheHitCell(row) {
  const prompt = Number(row?.prompt_tokens || 0);
  const cached = Number(row?.cached_tokens || 0);
  if (!prompt || prompt <= 0 || Number.isNaN(prompt)) {
    return `<span class="muted" title="输入 tokens 为 0，无法计算命中率">-</span>`;
  }
  const pct = Math.round((cached / prompt) * 1000) / 10;
  const title = `缓存命中 ${cached.toLocaleString()} / 输入 ${prompt.toLocaleString()}`;
  return `<span title="${title}">${pct.toFixed(1)}%</span>`;
}

function usageTable(rows, type) {
  if (!rows.length) {
    return `<table class="data-table"><thead><tr><th>维度</th><th>请求</th><th>输入</th><th>缓存命中率</th><th>输出</th><th>总量</th></tr></thead><tbody>${String(emptyRow(6, "暂无统计数据", "所选时间范围内没有可聚合的调用记录。"))}</tbody></table>`;
  }
  const body = rows.map((x) => `<tr>
      <td>${type === "provider" ? raw(providerLabel(x.key)) : truncCell(x.key)}</td>
      <td>${Number(x.request_count || 0)}</td>
      <td>${raw(tokenCell(x.prompt_tokens))}</td>
      <td>${raw(cacheHitCell(x))}</td>
      <td>${raw(tokenCell(x.completion_tokens))}</td>
      <td>${raw(tokenCell(x.total_tokens))}</td></tr>`).join("");
  return `<table class="data-table"><thead><tr><th>维度</th><th>请求</th><th>输入</th><th>缓存命中率</th><th>输出</th><th>总量</th></tr></thead><tbody>${body}</tbody></table>`;
}

function usageRecordsTable(rows) {
  if (!rows.length) {
    return `<table class="data-table"><thead><tr><th>时间</th><th>来源</th><th>来源 IP</th><th>外部服务</th><th>上游模型</th><th>本地模型</th><th>输入</th><th>缓存命中率</th><th>输出</th><th>总量</th><th>用量来源</th></tr></thead><tbody>${String(emptyRow(11, "暂无调用明细", "客户端发起一次 /v1 请求后即可在此查看逐条用量。"))}</tbody></table>`;
  }
  const body = rows.map((x) => `<tr>
      <td>${raw(formatDateTime(x.created_at))}</td>
      <td>${escapeHtml(x.source)}</td>
      <td>${truncCell(x.client_ip || "")}</td>
      <td>${raw(providerLabel(x.provider_key_id))}</td>
      <td>${truncCell(x.upstream_model || "")}</td>
      <td>${truncCell(x.local_model || "")}</td>
      <td>${raw(tokenCell(x.prompt_tokens))}</td>
      <td>${raw(cacheHitCell(x))}</td>
      <td>${raw(tokenCell(x.completion_tokens))}</td>
      <td>${raw(tokenCell(x.total_tokens))}</td>
      <td>${escapeHtml(x.usage_source)}</td></tr>`).join("");
  return `<table class="data-table"><thead><tr><th>时间</th><th>来源</th><th>来源 IP</th><th>外部服务</th><th>上游模型</th><th>本地模型</th><th>输入</th><th>缓存命中率</th><th>输出</th><th>总量</th><th>用量来源</th></tr></thead><tbody>${body}</tbody></table>`;
}

async function logs() {
  const rows = await api("/request-logs?limit=200");
  $("page").innerHTML = `<div class="panel"><h1>请求日志</h1>${logTable(Array.isArray(rows) ? rows : [])}</div>`;
}

/** 返回可信 HTML 片段（内部已转义），在 html`` 中插值需 raw() */
function logTable(rows) {
  if (!rows.length) {
    return `<table class="data-table"><thead><tr><th>时间</th><th>来源</th><th>来源 IP</th><th>外部服务</th><th>上游模型</th><th>本地模型</th><th>状态</th><th>Token</th><th>耗时</th><th>错误</th></tr></thead><tbody>${String(emptyRow(10, "暂无请求记录", "客户端调用 /v1/chat/completions 或 /v1/responses 后会在此生成记录。"))}</tbody></table>`;
  }
  const body = rows.map((x) => `<tr>
      <td>${formatDateTime(x.created_at)}</td>
      <td>${escapeHtml(x.source)}</td>
      <td>${truncCell(x.client_ip || "")}</td>
      <td>${providerLabel(x.provider_key_id)}</td>
      <td>${truncCell(x.upstream_model || "")}</td>
      <td>${truncCell(x.local_model || "")}</td>
      <td class="${x.success ? "ok" : "bad"}">${escapeHtml(x.status_code)}</td>
      <td>${Number(x.prompt_tokens || 0)} / ${Number(x.completion_tokens || 0)} / ${Number(x.total_tokens || 0)}</td>
      <td>${escapeHtml(x.latency_ms)}ms</td>
      <td class="cell-wrap">${escapeHtml(x.error_message)}</td></tr>`).join("");
  return `<table class="data-table"><thead><tr><th>时间</th><th>来源</th><th>来源 IP</th><th>外部服务</th><th>上游模型</th><th>本地模型</th><th>状态</th><th>Token</th><th>耗时</th><th>错误</th></tr></thead><tbody>${body}</tbody></table>`;
}

async function settings() {
  const s = await api("/settings");
  $("page").innerHTML = html`<div class="panel"><h1>系统设置</h1>
    <div class="grid three">
      <label>监听地址<input id="setHost" value="${s.host}"></label>
      <label>端口<input id="setPort" type="number" value="${s.port}"></label>
      <label>自动打开浏览器<select id="setAuto"><option value="false">否</option><option value="true" ${s.auto_open ? "selected" : ""}>是</option></select></label>
    </div>
    <div class="actions">
      <button data-action="save-settings" data-reload="0">保存</button>
      <button class="secondary" data-action="save-settings" data-reload="1">保存并重载监听</button>
    </div>
    <p class="muted">监听地址和端口会在重载监听后立即生效；如果新地址不可用，当前服务会继续保持原监听。</p>
    <p class="danger-note">重载监听会短暂中断当前连接（进行中的代理请求可能失败），也会改变访问地址；请确认新地址可用后再执行。</p>
  </div>`;
}

async function saveSettings(reload) {
  const restore = setBusy($("page").querySelector('[data-reload="' + (reload ? "1" : "0") + '"]'), true, "保存中...");
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
  } catch (e) { toast(e.message); } finally { restore(); }
}

function browserURLForListen(host, port) {
  let h = host || "127.0.0.1";
  if (h === "0.0.0.0" || h === "::") h = "localhost";
  if (h.includes(":") && !h.startsWith("[")) h = `[${h}]`;
  return `${location.protocol}//${h}:${port}/`;
}

/** 删除操作统一收口：破坏性操作给出影响面提示 */
async function del(path, label = "") {
  const target = label ? `${label}` : "该记录";
  if (!confirm(`确认删除${target}？\n\n删除后将立即生效且无法恢复，正在使用该配置的客户端请求会失败。`)) return;
  try { await api(path, { method: "DELETE" }); toast("已删除"); await loadBasics(); render(); } catch (e) { toast(e.message); }
}

/* ------------------------------------------------------------------ 事件委托 */
/* 所有按钮统一走 data-action，避免内联 onclick 与内联注入风险（CSP 友好）。 */
const delegatedActions = {
  "save-provider": () => saveProvider(),
  "cancel-provider": () => { state.editingProviderId = ""; state.editingProvider = null; providers(); },
  "copy-provider-key": () => copyProviderKey(),
  "edit-provider": (el) => editProvider(el.dataset.id),
  "test-provider": (el) => testProvider(el.dataset.id),
  "save-proxy": () => saveProxy(),
  "cancel-proxy": () => { state.editingProxyId = ""; proxies(); },
  "edit-proxy": (el) => editProxy(el.dataset.id),
  "test-proxy": (el) => testProxy(el.dataset.id),
  "save-model": () => saveModel(),
  "cancel-model": () => { state.editingModelId = ""; models(); },
  "edit-model": (el) => editModel(el.dataset.id),
  "save-local-key": () => saveLocalKey(),
  "cancel-local-key": () => { state.editingLocalKeyId = ""; localkeys(); },
  "edit-local-key": (el) => editLocalKey(el.dataset.id),
  "copy-local-key": (el) => copyLocalKey(el.dataset.id),
  "copy-base-url": () => copyBaseUrl(),
  "run-chat": () => runChat(),
  "usage-search": () => {
    state.usageStart = $("usageStart").value;
    state.usageEnd = $("usageEnd").value;
    usage();
  },
  "usage-clear": () => {
    state.usageStart = "";
    state.usageEnd = "";
    usage();
  },
  "usage-preset": (el) => {
    const range = usagePresetRange(el.dataset.preset);
    state.usageStart = range.start;
    state.usageEnd = range.end;
    usage();
  },
  "usage-export": () => downloadUsage(),
  "save-settings": (el) => saveSettings(el.dataset.reload === "1"),
  "del": (el) => del(el.dataset.path, el.dataset.label || "")
};

function handleDelegatedAction(event) {
  const el = event.target instanceof Element ? event.target.closest("[data-action]") : null;
  if (!el) return;
  const handler = delegatedActions[el.dataset.action];
  if (!handler) return;
  event.preventDefault();
  handler(el);
}

boot();
