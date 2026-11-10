/* ==========================================================================
   common.js —— my_ai_sum 管理后台公共运行时（零构建链，全局挂载）
   必须在 app.js / debug-logs.js 之前引入。
   提供：$ / state(token) / api(401 统一收口) / toast / escapeHtml / raw / html
        / setBusy / emptyRow / copyText / setToken / clearToken / onUnauthorized
   ========================================================================== */
(function () {
  "use strict";

  /* ------------------------------------------------------------ DOM 快捷方法 */
  const $ = (id) => document.getElementById(id);

  /* ----------------------------------------------------- token 存储（会话级） */
  const TOKEN_KEY = "admin_token";

  // 唯一一处持久存储访问：仅用于把旧版本遗留的 token 迁移到 sessionStorage 后立即清除
  const legacyTokenStorage = () => {
    try { return window.localStorage; } catch (_) { return null; }
  };

  (function migrateLegacyToken() {
    const legacy = legacyTokenStorage();
    if (!legacy) return;
    try {
      if (!sessionStorage.getItem(TOKEN_KEY)) {
        const old = legacy.getItem(TOKEN_KEY);
        if (old) sessionStorage.setItem(TOKEN_KEY, old);
      }
      // 无论迁移与否，遗留的 localStorage token 一律清除（token 不再落持久存储）
      legacy.removeItem(TOKEN_KEY);
    } catch (_) {
      /* 隐私模式 / 存储被禁用时静默忽略 */
    }
  })();

  function readToken() {
    try {
      return sessionStorage.getItem(TOKEN_KEY) || "";
    } catch (_) {
      return "";
    }
  }

  function setToken(token) {
    state.token = token || "";
    try {
      if (state.token) sessionStorage.setItem(TOKEN_KEY, state.token);
      else sessionStorage.removeItem(TOKEN_KEY);
    } catch (_) {}
  }

  function clearToken() {
    state.token = "";
    try {
      sessionStorage.removeItem(TOKEN_KEY);
    } catch (_) {}
    const legacy = legacyTokenStorage();
    if (legacy) {
      try { legacy.removeItem(TOKEN_KEY); } catch (_) {}
    }
  }

  /* ------------------------------------------------------------ 全局状态容器 */
  // 页面脚本在自身作用域内扩展 state 字段（不要重新声明 const state）
  const state = { token: readToken() };

  /* ------------------------------------------------------------ HTML 转义 */
  function escapeHtml(v) {
    return String(v ?? "").replace(/[&<>"']/g, (c) => ({
      "&": "&amp;",
      "<": "&lt;",
      ">": "&gt;",
      '"': "&quot;",
      "'": "&#39;"
    }[c]));
  }

  /** 显式声明「这段内容已是可信 HTML，请原样插入」 */
  class RawHTML {
    constructor(value) {
      this.value = String(value ?? "");
    }
    toString() {
      return this.value;
    }
  }

  function raw(v) {
    return v instanceof RawHTML ? v : new RawHTML(v);
  }

  /**
   * html 标签模板：插值默认转义，需原样 HTML 时显式 raw()。
   * 兼容旧调用点：内部函数返回的可信 HTML 字符串必须用 raw(...) 包裹。
   */
  function html(strings, ...vals) {
    let out = strings[0];
    for (let i = 0; i < vals.length; i += 1) {
      const v = vals[i];
      out += (v instanceof RawHTML ? v.value : escapeHtml(v)) + strings[i + 1];
    }
    return out;
  }

  /* -------------------------------------------------- 401 统一收口与钩子 */
  let unauthorizedHandler = null;

  /** 注册登录失效后的视图切换逻辑（页面脚本在 boot 时注册） */
  function onUnauthorized(fn) {
    unauthorizedHandler = typeof fn === "function" ? fn : null;
  }

  function handleUnauthorized() {
    clearToken();
    if (unauthorizedHandler) {
      try {
        unauthorizedHandler();
        return;
      } catch (_) {}
    }
    // 兜底：无注册钩子时重载页面，让脚本走未登录分支
    if (typeof location !== "undefined" && location.reload) {
      try { location.reload(); } catch (_) {}
    }
  }

  /* ------------------------------------------------------------------ api */
  async function api(path, opts = {}) {
    const headers = opts.headers || {};
    if (!(opts.body instanceof FormData)) headers["Content-Type"] = "application/json";
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const res = await fetch(`/api/v1${path}`, { ...opts, headers });

    // ---- 401 统一收口：清 token + 切登录视图 + 标准错误 ----
    if (res.status === 401) {
      handleUnauthorized();
      const err = new Error("登录状态已失效，请重新登录");
      err.status = 401;
      err.unauthorized = true;
      throw err;
    }

    if (!res.ok) {
      let msg = res.statusText;
      try {
        const payload = await res.json();
        msg = typeof formatErrorPayload === "function" ? formatErrorPayload(payload, msg) : (payload?.error || msg);
      } catch (_) {}
      throw new Error(msg);
    }
    const ct = res.headers.get("content-type") || "";
    return ct.includes("json") ? res.json() : res.text();
  }

  /* ---------------------------------------------------------------- toast */
  /** 轻提示：支持多行文案（CSS white-space: pre-line），timer 清理防累积 */
  function toast(msg) {
    const el = $("toast");
    if (!el) return;
    const text = typeof formatErrorMessage === "function"
      ? formatErrorMessage(msg)
      : String(msg ?? "");
    el.textContent = text;
    el.classList.remove("hidden");
    if (el._toastTimer) clearTimeout(el._toastTimer);
    el._toastTimer = setTimeout(() => {
      el.classList.add("hidden");
      el._toastTimer = null;
    }, 3200);
  }

  /* -------------------------------------------------------------- setBusy */
  /**
   * 防重复提交：禁用按钮 + loading 态，返回 restore 函数用于恢复。
   * 重复调用 setBusy(el, true) 是幂等的。
   */
  function setBusy(el, busy = true, busyText = "") {
    const noop = () => {};
    if (!el) return noop;

    if (busy) {
      if (el.dataset.busyActive === "1") return noop;
      el.dataset.busyActive = "1";
      el.dataset.originalText = el.textContent;
      el.dataset.originalDisabled = el.disabled ? "1" : "0";
      el.disabled = true;
      el.classList.add("is-busy");
      el.setAttribute("aria-busy", "true");
      if (busyText) el.textContent = busyText;
      return () => setBusy(el, false);
    }

    if (el.dataset.busyActive !== "1") return noop;
    el.dataset.busyActive = "";
    el.disabled = el.dataset.originalDisabled === "1";
    el.classList.remove("is-busy");
    el.removeAttribute("aria-busy");
    if (el.dataset.originalText != null) el.textContent = el.dataset.originalText;
    delete el.dataset.originalText;
    delete el.dataset.originalDisabled;
    return noop;
  }

  /* ------------------------------------------------------------- emptyRow */
  /**
   * 表格空状态占位行：文案 + 下一步引导。
   * 返回 RawHTML 对象，可直接用于 html`` 插值或字符串拼接。
   */
  function emptyRow(colspan, title = "暂无数据", hint = "") {
    const cols = Number(colspan) > 0 ? Number(colspan) : 1;
    const hintHtml = hint ? `<span class="empty-row-hint">${escapeHtml(hint)}</span>` : "";
    return raw(
      `<tr class="empty-row"><td colspan="${cols}">` +
      `<span class="empty-row-title">${escapeHtml(title)}</span>${hintHtml}` +
      `</td></tr>`
    );
  }

  /* ------------------------------------------------------------- copyText */
  /** 复制文本；传入 btn 时在该按钮上给出「已复制」反馈（兼容 debug-logs 旧签名） */
  async function copyText(text, btn) {
    const value = String(text ?? "");
    const feedback = () => {
      if (!btn) return;
      const orig = btn.textContent;
      btn.textContent = "已复制 ✓";
      setTimeout(() => { btn.textContent = orig; }, 1600);
    };
    if (navigator.clipboard && window.isSecureContext) {
      try {
        await navigator.clipboard.writeText(value);
        feedback();
        return true;
      } catch (_) {}
    }
    const ta = document.createElement("textarea");
    ta.value = value;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-9999px";
    document.body.appendChild(ta);
    ta.select();
    ta.setSelectionRange(0, ta.value.length);
    let ok = false;
    try { ok = document.execCommand("copy"); } catch (_) {}
    ta.remove();
    if (ok) feedback();
    return ok;
  }

  /* ------------------------------------------------------------ 全局导出 */
  window.$ = $;
  window.state = state;
  window.api = api;
  window.toast = toast;
  window.escapeHtml = escapeHtml;
  window.raw = raw;
  window.RawHTML = RawHTML;
  window.html = html;
  window.setBusy = setBusy;
  window.emptyRow = emptyRow;
  window.copyText = copyText;
  window.setToken = setToken;
  window.clearToken = clearToken;
  window.onUnauthorized = onUnauthorized;
})();
