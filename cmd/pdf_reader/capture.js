// capture.js —— 注入到 pdf.js viewer 里的划词错题本。
// 由 pdf_reader 在伺服 viewer.html 时塞进去，不是油猴脚本，改完重启工具就生效。
(function () {
  'use strict';

  // ── 配置 ────────────────────────────────────────────────────────────
  const CONTEXT_CHARS = 80; // 上下文往前后各抓多少字，觉得不够就改这个数

  const LS_KEY = 'pdf-reader-capture';
  let enabled = localStorage.getItem(LS_KEY) !== 'off';
  let saved = 0;

  // ── 当前在读哪个文件 ────────────────────────────────────────────────
  function currentFile() {
    const q = new URLSearchParams(location.search).get('file') || '';
    try {
      return decodeURIComponent(q).replace(/^\/files\//, '');
    } catch {
      return q;
    }
  }

  // ── 选中了什么、在第几页、上下文是什么 ──────────────────────────────
  // pdf.js 的文字层是真 DOM：.page[data-page-number] 里套一层 .textLayer，
  // 里面全是 <span>。所以从选区往上爬就能拿到页码。
  function pageOf(node) {
    const el = node && (node.nodeType === 1 ? node : node.parentElement);
    const page = el && el.closest && el.closest('.page[data-page-number]');
    if (page) return page.getAttribute('data-page-number');
    // 兜底：读工具栏那个页码输入框（选区拿不到时至少不是空的）
    const input = document.getElementById('pageNumber');
    return input ? input.value : '';
  }

  function contextOf(node, text) {
    const el = node && (node.nodeType === 1 ? node : node.parentElement);
    const layer = el && el.closest && el.closest('.textLayer');
    if (!layer) return '';
    const full = [...layer.querySelectorAll('span')]
      .map((s) => s.textContent)
      .join(' ')
      .replace(/\s+/g, ' ')
      .trim();
    const i = full.indexOf(text.replace(/\s+/g, ' ').trim());
    if (i < 0) return full.slice(0, CONTEXT_CHARS * 2);
    const from = Math.max(0, i - CONTEXT_CHARS);
    const to = Math.min(full.length, i + text.length + CONTEXT_CHARS);
    return (from > 0 ? '…' : '') + full.slice(from, to) + (to < full.length ? '…' : '');
  }

  function grab() {
    const sel = window.getSelection();
    const text = sel ? String(sel).replace(/\s+/g, ' ').trim() : '';
    if (!text) return null;
    const node = sel.anchorNode;
    return { text, page: pageOf(node), context: contextOf(node, text), file: currentFile() };
  }

  // ── 存 ──────────────────────────────────────────────────────────────
  async function save(withNote) {
    const item = grab();
    if (!item) {
      toast('⚠️ 先选中一段文字再按');
      return;
    }
    if (withNote) {
      const note = prompt(`备注（可留空）\n\n${item.text.slice(0, 120)}`, '');
      if (note === null) return toast('⏸️ 取消了');
      item.note = note;
    }
    try {
      const res = await fetch('/api/wrong', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(item),
      });
      if (!res.ok) throw new Error(await res.text());
      const data = await res.json();
      saved++;
      toast(`✅ 已存第 ${item.page} 页（本次 ${saved} 条 · 共 ${data.total} 条）`);
    } catch (e) {
      toast('❌ 存失败：' + e.message);
      console.error('[错题本]', e);
    }
  }

  // ── 快捷键 ──────────────────────────────────────────────────────────
  // 捕获阶段，否则会被 pdf.js viewer 自己的快捷键 handler 吃掉
  document.addEventListener(
    'keydown',
    (e) => {
      if (!e.ctrlKey || !e.shiftKey) return;
      const k = e.key.toLowerCase();

      if (k === 's') {
        e.preventDefault();
        e.stopPropagation();
        if (!enabled) return toast('⏸️ 划词错题本是关的（Ctrl+Shift+A 开）');
        save(false);
      } else if (k === 'n') {
        e.preventDefault();
        e.stopPropagation();
        if (!enabled) return toast('⏸️ 划词错题本是关的（Ctrl+Shift+A 开）');
        save(true);
      } else if (k === 'a') {
        e.preventDefault();
        enabled = !enabled;
        localStorage.setItem(LS_KEY, enabled ? 'on' : 'off');
        toast(enabled ? '✅ 划词错题本已开启' : '⏸️ 划词错题本已关闭');
      } else if (k === 'd') {
        e.preventDefault();
        const item = grab();
        console.log('[错题本] 当前文件:', currentFile());
        console.log('[错题本] 开关:', enabled ? '开' : '关', '· 本次已存:', saved);
        console.log('[错题本] 这次抓到的:', item);
        console.log('[错题本] 文字层数量:', document.querySelectorAll('.textLayer').length);
        toast('🔍 已打印到控制台（F12）');
      }
    },
    true
  );

  // ── 角落提示条 ──────────────────────────────────────────────────────
  let tipEl, tipTimer;
  function toast(msg) {
    if (!document.body) return;
    if (!tipEl || !tipEl.isConnected) {
      tipEl = document.createElement('div');
      tipEl.style.cssText = [
        'position:fixed', 'right:16px', 'bottom:16px', 'z-index:2147483647',
        'background:rgba(28,28,30,.92)', 'color:#f2f2f7',
        'font:13px/1.5 system-ui,sans-serif', 'padding:8px 12px',
        'border-radius:8px', 'pointer-events:none', 'max-width:60vw',
        'box-shadow:0 4px 16px rgba(0,0,0,.3)', 'transition:opacity .25s', 'opacity:0',
      ].join(';');
      document.body.appendChild(tipEl);
    }
    tipEl.textContent = msg;
    tipEl.style.opacity = '1';
    clearTimeout(tipTimer);
    tipTimer = setTimeout(() => { tipEl.style.opacity = '0'; }, 2600);
  }

  toast('📖 划词错题本就绪：Ctrl+Shift+S 存 · Ctrl+Shift+N 带备注存 · Ctrl+Shift+D 诊断');
})();
