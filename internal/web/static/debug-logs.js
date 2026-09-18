const $ = (id) => document.getElementById(id);

const state = {
  token: localStorage.getItem("admin_token") || "",
  segment: null,
  parsedLogs: [],
  filteredLogs: [],
  expandedSet: new Set(),
  activeTab: "structured", // 'structured' | 'raw'
  timer: null,
  dirty: false,
  expanded: false,
  filters: {
    search: "",
    level: "all",
    status: "all",
    stream: "all"
  }
};

function toast(msg) {
  const el = $("toast");
  el.textContent = msg;
  el.classList.remove("hidden");
  clearTimeout(el._timer);
  el._timer = setTimeout(() => el.classList.add("hidden"), 2800);
}

function escapeHtml(str) {
  return String(str || "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}

async function copyText(text, btn) {
  try {
    await navigator.clipboard.writeText(text);
    if (btn) {
      const orig = btn.textContent;
      btn.textContent = "已复制 ✓";
      setTimeout(() => { btn.textContent = orig; }, 1600);
    } else {
      toast("已复制到剪贴板");
    }
  } catch {
    // Fallback
    const ta = document.createElement("textarea");
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    document.body.removeChild(ta);
    if (btn) {
      const orig = btn.textContent;
      btn.textContent = "已复制 ✓";
      setTimeout(() => { btn.textContent = orig; }, 1600);
    } else {
      toast("已复制到剪贴板");
    }
  }
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
    $("emptyMsg").textContent = "未登录，请先在主页面登录。";
    $("emptyState").classList.remove("hidden");
    return;
  }

  // 绑定视图切换
  $("tabStructured").onclick = () => switchTab("structured");
  $("tabRaw").onclick = () => switchTab("raw");

  const triggerRefresh = async () => {
    const icons = document.querySelectorAll(".refresh-icon");
    icons.forEach((el) => el.classList.add("spinning"));
    try {
      await loadLogs(false);
      toast("已刷新日志");
    } finally {
      setTimeout(() => {
        icons.forEach((el) => el.classList.remove("spinning"));
      }, 600);
    }
  };

  // 快捷控制
  $("refreshBtn").onclick = triggerRefresh;
  const filterRefresh = $("filterRefreshBtn");
  if (filterRefresh) filterRefresh.onclick = triggerRefresh;
  $("expandAllBtn").onclick = expandAll;
  $("collapseAllBtn").onclick = collapseAll;
  $("expandBtn").onclick = toggleExpanded;
  $("saveRawBtn").onclick = saveLogs;
  $("saveSettingsBtn").onclick = saveSettings;

  // 筛选器绑定
  $("searchInput").oninput = handleSearch;
  $("clearSearchBtn").onclick = () => {
    $("searchInput").value = "";
    handleSearch();
  };
  $("filterStatus").onchange = handleFilterChange;
  $("filterStream").onchange = handleFilterChange;
  $("limit").onchange = () => loadLogs(false);
  $("realtime").onchange = updateRealtime;
  $("wrapLines").onchange = updateWrapLines;
  $("resetFilterBtn").onclick = resetFilters;

  // 级别过滤药丸按钮
  document.querySelectorAll("#levelFilters .level-pill").forEach((btn) => {
    btn.onclick = () => {
      document.querySelectorAll("#levelFilters .level-pill").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      state.filters.level = btn.dataset.level || "all";
      applyFiltersAndRender();
    };
  });

  // 原始文本编辑 dirty 检测与快捷键保存 / 刷新
  $("viewer").oninput = () => { state.dirty = true; updateMeta(); };
  window.addEventListener("keydown", (e) => {
    const tag = (e.target && e.target.tagName) || "";
    const isInput = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
    if (!isInput && e.key.toLowerCase() === "r" && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault();
      triggerRefresh();
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
      e.preventDefault();
      saveLogs();
    }
  });

  await loadSettings();
  await loadLogs(false);
}

function switchTab(tab) {
  state.activeTab = tab;
  $("tabStructured").classList.toggle("active", tab === "structured");
  $("tabRaw").classList.toggle("active", tab === "raw");
  $("structuredWrap").classList.toggle("hidden", tab !== "structured");
  $("rawWrap").classList.toggle("hidden", tab !== "raw");
  $("filterToolbar").classList.toggle("hidden", tab !== "structured");
  $("expandAllBtn").classList.toggle("hidden", tab !== "structured");
  $("collapseAllBtn").classList.toggle("hidden", tab !== "structured");
}

async function loadSettings() {
  try {
    const s = await api("/settings");
    $("globalLogLevel").value = s.log_level || "info";
    $("detailedLogEnabled").value = String(Boolean(s.api_debug_enabled));
  } catch (e) {
    toast("加载配置失败: " + e.message);
  }
}

async function saveSettings() {
  try {
    const s = await api("/settings");
    const globalLevel = $("globalLogLevel").value;
    const isDetailed = $("detailedLogEnabled").value === "true";
    await api("/settings", {
      method: "PUT",
      body: JSON.stringify({
        ...s,
        log_level: globalLevel,
        api_debug_enabled: isDetailed,
        api_debug_level: globalLevel,
        api_debug_request_body: isDetailed,
        api_debug_response_body: isDetailed,
        api_debug_max_body_chars: s.api_debug_max_body_chars || 20000
      })
    });
    toast("配置已成功保存");
  } catch (e) {
    toast("保存失败: " + e.message);
  }
}

// 智能聚合流式分块或解析 JSON 字符串
function formatStreamResponseBody(rawText) {
  if (!rawText || typeof rawText !== "string") return rawText;
  const trimmed = rawText.trim();
  if (!trimmed) return "";

  // 1. 如果本身是单个有效 JSON，直接返回解析后的对象
  try {
    const parsed = JSON.parse(trimmed);
    return parsed;
  } catch {}

  // 2. 如果是多行 SSE 响应（如包含 data: 或换行隔开的多个 JSON）
  const lines = trimmed.split("\n").map((l) => l.trim()).filter(Boolean);
  let aggregatedContent = "";
  let reasoningContent = "";
  let lastCompleted = null;
  let hasChunks = false;

  for (const line of lines) {
    let payloadStr = line;
    if (line.startsWith("data:")) {
      payloadStr = line.replace(/^data:\s*/, "").trim();
    }
    if (!payloadStr || payloadStr === "[DONE]") continue;

    try {
      const obj = JSON.parse(payloadStr);
      hasChunks = true;

      // OpenAI 格式
      if (Array.isArray(obj.choices) && obj.choices.length > 0) {
        const delta = obj.choices[0].delta || {};
        if (delta.content) aggregatedContent += delta.content;
        if (delta.reasoning_content) reasoningContent += delta.reasoning_content;
      }
      // Responses 格式
      if (obj.response) {
        lastCompleted = obj.response;
      }
      if (obj.type === "response.output_text.delta" && obj.delta) {
        aggregatedContent += obj.delta;
      }
      if (obj.type === "response.output_text.done" && obj.text) {
        aggregatedContent = obj.text;
      }
    } catch {}
  }

  if (lastCompleted) {
    if (aggregatedContent && !lastCompleted.output_text) {
      lastCompleted.output_text = aggregatedContent;
    }
    return lastCompleted;
  }

  if (hasChunks && (aggregatedContent || reasoningContent)) {
    const res = {
      aggregated_message: {
        role: "assistant",
        content: aggregatedContent
      }
    };
    if (reasoningContent) {
      res.aggregated_message.reasoning_content = reasoningContent;
    }
    return res;
  }

  return rawText;
}

// 解析从服务器拉取到的单行/多行日志并去重聚合
function parseLogSegment(segment) {
  const lines = segment?.lines || [];
  const logMap = new Map();

  lines.forEach((line, index) => {
    const trimmed = line.trim();
    if (!trimmed) return;

    let entry = null;
    try {
      entry = JSON.parse(trimmed);
    } catch {
      // 无法被 JSON 解析的非格式化行
      entry = {
        _isRaw: true,
        request_id: `raw_line_${index}`,
        time: "",
        level: "info",
        raw_text: line
      };
    }

    const reqId = entry.request_id || `log_${index}`;

    // 处理流式预览聚合：同一 request_id 的记录合并保留最新一条
    if (entry.stream || entry.response_body_preview) {
      entry.response_body_parsed = formatStreamResponseBody(entry.response_body_preview);
    }

    if (entry.request_body_preview) {
      try {
        entry.request_body_parsed = JSON.parse(entry.request_body_preview);
      } catch {
        entry.request_body_parsed = entry.request_body_preview;
      }
    }

    // 存入 Map：若流式出现多条相同 request_id，自然去重保留最新完整记录
    logMap.set(reqId, { ...entry, _idx: index });
  });

  const parsed = Array.from(logMap.values());
  // 默认按时间倒序排列（最新在上）
  parsed.sort((a, b) => {
    if (a.time && b.time) {
      return b.time.localeCompare(a.time);
    }
    return b._idx - a._idx;
  });

  return parsed;
}

async function loadLogs(fromTimer) {
  if (fromTimer && state.dirty) return;
  try {
    const limit = Math.min(Math.max(Number($("limit").value || 100), 1), 5000);
    const res = await api(`/api-debug-logs?limit=${limit}`);
    state.segment = res;

    // 更新控制台源码框
    if (!state.dirty) {
      $("viewer").value = res.content || "";
    }

    // 解析结构化数据
    state.parsedLogs = parseLogSegment(res);
    applyFiltersAndRender();
    updateMeta();
  } catch (e) {
    if (!fromTimer) toast("加载日志失败: " + e.message);
  }
}

function handleSearch() {
  const val = $("searchInput").value.trim();
  state.filters.search = val.toLowerCase();
  $("clearSearchBtn").classList.toggle("hidden", !val);
  applyFiltersAndRender();
}

function handleFilterChange() {
  state.filters.status = $("filterStatus").value;
  state.filters.stream = $("filterStream").value;
  applyFiltersAndRender();
}

function resetFilters() {
  $("searchInput").value = "";
  $("filterStatus").value = "all";
  $("filterStream").value = "all";
  state.filters.search = "";
  state.filters.level = "all";
  state.filters.status = "all";
  state.filters.stream = "all";
  $("clearSearchBtn").classList.add("hidden");
  document.querySelectorAll("#levelFilters .level-pill").forEach((b) => {
    b.classList.toggle("active", b.dataset.level === "all");
  });
  applyFiltersAndRender();
}

function applyFiltersAndRender() {
  const { search, level, status, stream } = state.filters;
  let countError = 0;
  let countInfo = 0;
  let countDebug = 0;

  // 统计各级别数量
  state.parsedLogs.forEach((item) => {
    const lvl = (item.level || "info").toLowerCase();
    if (lvl === "error") countError++;
    else if (lvl === "info") countInfo++;
    else if (lvl === "debug") countDebug++;
  });

  $("countAll").textContent = state.parsedLogs.length;
  $("countError").textContent = countError;
  $("countInfo").textContent = countInfo;
  $("countDebug").textContent = countDebug;

  // 过滤
  state.filteredLogs = state.parsedLogs.filter((item) => {
    // 级别筛选
    const itemLevel = (item.level || "info").toLowerCase();
    if (level !== "all" && itemLevel !== level) return false;

    // 状态筛选
    if (status === "success") {
      if (item.success === false || (item.status_code && item.status_code >= 400)) return false;
    } else if (status === "error") {
      if (item.success !== false && (!item.status_code || item.status_code < 400)) return false;
    }

    // 流式筛选
    if (stream === "stream" && !item.stream) return false;
    if (stream === "non-stream" && item.stream) return false;

    // 关键词搜索
    if (search) {
      const matchTarget = [
        item.path,
        item.method,
        item.request_id,
        item.upstream_url,
        item.provider_name,
        item.local_model,
        item.upstream_model,
        item.error_message,
        item.raw_text,
        typeof item.request_body_preview === "string" ? item.request_body_preview : "",
        typeof item.response_body_preview === "string" ? item.response_body_preview : ""
      ].join(" ").toLowerCase();

      if (!matchTarget.includes(search)) return false;
    }

    return true;
  });

  renderLogList();
}

// 格式化 JSON 语法着色
function syntaxHighlightJSON(json) {
  let str = "";
  if (typeof json !== "string") {
    str = JSON.stringify(json, null, 2);
  } else {
    try {
      str = JSON.stringify(JSON.parse(json), null, 2);
    } catch {
      str = json;
    }
  }

  // 转义 HTML
  str = escapeHtml(str);

  // 正则着色
  return str.replace(
    /("(\\u[a-zA-Z0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+\-]?\d+)?)/g,
    (match) => {
      let cls = "json-num";
      if (/^"/.test(match)) {
        if (/:$/.test(match)) {
          cls = "json-key";
        } else {
          cls = "json-str";
        }
      } else if (/true|false/.test(match)) {
        cls = "json-bool";
      } else if (/null/.test(match)) {
        cls = "json-null";
      }
      return `<span class="${cls}">${match}</span>`;
    }
  );
}

function renderLogList() {
  const container = $("logList");
  const empty = $("emptyState");

  if (state.filteredLogs.length === 0) {
    container.innerHTML = "";
    empty.classList.remove("hidden");
    const hasFilter = state.filters.search || state.filters.level !== "all" || state.filters.status !== "all" || state.filters.stream !== "all";
    $("emptyMsg").textContent = hasFilter ? "未找到符合筛选条件的日志" : "暂无详细日志";
    $("resetFilterBtn").classList.toggle("hidden", !hasFilter);
    return;
  }

  empty.classList.add("hidden");
  let html = "";

  state.filteredLogs.forEach((log) => {
    const id = log.request_id || `log_${log._idx}`;
    const isOpen = state.expandedSet.has(id);

    if (log._isRaw) {
      html += `
        <div class="log-card status-ok" data-id="${escapeHtml(id)}">
          <div class="log-summary">
            <span class="badge badge-info">RAW</span>
            <span class="log-path">${escapeHtml(log.raw_text)}</span>
          </div>
        </div>
      `;
      return;
    }

    const level = (log.level || "info").toLowerCase();
    const isSuccess = log.success !== false && (!log.status_code || log.status_code < 400);
    const statusClass = isSuccess ? "status-ok" : "status-error";
    const statusCodeBadgeClass = isSuccess ? "ok" : "bad";
    const statusText = log.status_code ? `${log.status_code}` : (isSuccess ? "OK" : "ERR");

    // 时间显示格式
    const timeParts = (log.time || "").split(" ");
    const timeShort = timeParts[1] || log.time || "--:--:--";

    // 模型展示
    let modelDisplay = "";
    if (log.local_model || log.upstream_model) {
      if (log.local_model === log.upstream_model || !log.local_model) {
        modelDisplay = `<span class="model-tag">${escapeHtml(log.upstream_model || log.local_model)}</span>`;
      } else {
        modelDisplay = `<span class="model-tag">${escapeHtml(log.local_model)}</span> → <span class="model-tag">${escapeHtml(log.upstream_model)}</span>`;
      }
    }

    // Token 展示
    let tokensDisplay = "";
    if (log.total_tokens) {
      tokensDisplay = `<span class="metric-tokens" title="输入 ${log.prompt_tokens || 0} / 输出 ${log.completion_tokens || 0} / 总计 ${log.total_tokens}">${log.total_tokens} tokens</span>`;
    }

    // 耗时展示
    const latencyDisplay = log.latency_ms !== undefined ? `<span class="metric-latency">${log.latency_ms}ms</span>` : "";

    html += `
      <div class="log-card ${statusClass} ${isOpen ? "open" : ""}" data-id="${escapeHtml(id)}">
        <div class="log-summary" onclick="toggleLog('${escapeHtml(id)}')">
          <span class="log-caret">▶</span>
          <span class="log-time" title="${escapeHtml(log.time)}">${escapeHtml(timeShort)}</span>
          <span class="badge badge-${escapeHtml(level)}">${escapeHtml(level.toUpperCase())}</span>
          ${log.method ? `<span class="badge badge-method">${escapeHtml(log.method)}</span>` : ""}
          <span class="badge badge-status ${statusCodeBadgeClass}">${escapeHtml(statusText)}</span>
          <span class="log-path">${escapeHtml(log.path || "/")}</span>
          ${log.stream ? `<span class="badge badge-stream">STREAM</span>` : ""}
          ${log.provider_name ? `<span class="badge badge-provider">${escapeHtml(log.provider_name)}</span>` : ""}
          ${modelDisplay ? `<div class="log-models">${modelDisplay}</div>` : ""}
          <div class="log-metrics">
            ${tokensDisplay}
            ${latencyDisplay}
          </div>
        </div>

        <div class="log-detail" id="detail_${escapeHtml(id)}">
          <div class="detail-grid">
            <div class="grid-field">
              <span class="field-label">Request ID</span>
              <span class="field-value">${escapeHtml(log.request_id || "N/A")} <button class="btn-copy" type="button" data-copy="${escapeHtml(log.request_id || "")}" onclick="copyFromData(this)">复制</button></span>
            </div>
            <div class="grid-field">
              <span class="field-label">时间戳</span>
              <span class="field-value">${escapeHtml(log.time || "N/A")}</span>
            </div>
            <div class="grid-field">
              <span class="field-label">上游目标地址</span>
              <span class="field-value" title="${escapeHtml(log.upstream_url || "")}">${escapeHtml(log.upstream_url || "直连或本地")}</span>
            </div>
            <div class="grid-field">
              <span class="field-label">调用来源</span>
              <span class="field-value">${escapeHtml(log.source || "gateway")}</span>
            </div>
            <div class="grid-field">
              <span class="field-label">外部服务 (Provider)</span>
              <span class="field-value">${escapeHtml(log.provider_name || log.provider_key_id || "N/A")}</span>
            </div>
            <div class="grid-field">
              <span class="field-label">Token 统计</span>
              <span class="field-value">Prompt: ${log.prompt_tokens || 0} / Completion: ${log.completion_tokens || 0} / Total: ${log.total_tokens || 0}</span>
            </div>
          </div>

          ${log.error_message ? `
            <div class="error-alert">
              <div class="error-title">⚠️ 错误详情 (Error Message)</div>
              <div class="error-body">${escapeHtml(log.error_message)}</div>
            </div>
          ` : ""}

          <div class="payload-sections">
            ${log.request_body_parsed ? `
              <div class="payload-block">
                <div class="payload-header">
                  <span class="payload-title">📥 请求体 (Request Body)</span>
                  <div class="payload-actions">
                    <button class="btn-sm secondary" type="button" onclick="copyPayload(this)">复制 JSON</button>
                  </div>
                </div>
                <pre class="code-viewer">${syntaxHighlightJSON(log.request_body_parsed)}</pre>
              </div>
            ` : ""}

            ${log.response_body_parsed ? `
              <div class="payload-block">
                <div class="payload-header">
                  <span class="payload-title">📤 响应体 ${log.stream ? "(流式聚合结果)" : "(Response Body)"}</span>
                  <div class="payload-actions">
                    <button class="btn-sm secondary" type="button" onclick="copyPayload(this)">复制内容</button>
                  </div>
                </div>
                <pre class="code-viewer">${syntaxHighlightJSON(log.response_body_parsed)}</pre>
              </div>
            ` : ""}

            <div class="payload-block">
              <div class="payload-header">
                <span class="payload-title">📄 完整日志记录 (Full Entry JSON)</span>
                <div class="payload-actions">
                  <button class="btn-sm secondary" type="button" onclick="copyPayload(this)">复制完整 JSON</button>
                </div>
              </div>
              <pre class="code-viewer">${syntaxHighlightJSON(log)}</pre>
            </div>
          </div>
        </div>
      </div>
    `;
  });

  container.innerHTML = html;
}

window.toggleLog = function(id) {
  if (state.expandedSet.has(id)) {
    state.expandedSet.delete(id);
  } else {
    state.expandedSet.add(id);
  }
  const card = document.querySelector(`.log-card[data-id="${id}"]`);
  if (card) {
    card.classList.toggle("open", state.expandedSet.has(id));
  }
};

window.copyText = copyText;

window.copyPayload = function(btn) {
  const block = btn.closest(".payload-block");
  const pre = block ? block.querySelector(".code-viewer") : null;
  if (pre) {
    copyText(pre.textContent, btn);
  }
};

window.copyFromData = function(btn) {
  const text = btn.dataset.copy || "";
  if (text) {
    copyText(text, btn);
  }
};

function expandAll() {
  state.filteredLogs.forEach((l) => state.expandedSet.add(l.request_id || `log_${l._idx}`));
  document.querySelectorAll(".log-card").forEach((c) => c.classList.add("open"));
}

function collapseAll() {
  state.expandedSet.clear();
  document.querySelectorAll(".log-card").forEach((c) => c.classList.remove("open"));
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
    toast("日志已成功保存");
    state.dirty = false;
    await loadLogs(false);
  } catch (e) {
    toast("保存失败: " + e.message);
  }
}

function updateRealtime() {
  if (state.timer) {
    clearInterval(state.timer);
    state.timer = null;
  }
  const isRealtime = $("realtime").checked;
  $("liveBadge").classList.toggle("hidden", !isRealtime);

  if (isRealtime) {
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
  const dirty = state.dirty ? "，文本已修改未保存" : "";
  if (!s?.file) {
    $("meta").textContent = `暂无日志${dirty}`;
    return;
  }
  $("meta").textContent = `${s.file} (第 ${s.start_line}-${s.end_line} 行 / 共 ${s.total_lines} 行) · 显示 ${state.filteredLogs.length} 条记录${dirty}`;
}

function updateWrapLines() {
  $("viewer").classList.toggle("wrap-lines", $("wrapLines").checked);
}

boot();

