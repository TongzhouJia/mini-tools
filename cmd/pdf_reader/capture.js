// capture.js —— 注入到 pdf.js viewer 里：划词存错题 / 翻译 / 朗读。
// 由 pdf_reader 在伺服 viewer.html 时塞进去，不是油猴脚本，改完重启工具就生效。
//
// 快捷键是单独字母（a / f / r / d），不带修饰键——阅读时不打字，按着顺手。
// 代价是要跟 pdf.js 自己的单键抢：r 是「旋转页面」、s 是选择工具、h 是抓手。
// 处理办法见下面 onKey 里的两条规矩。
(function () {
  'use strict';

  // ── 配置 ────────────────────────────────────────────────────────────
  const CONTEXT_CHARS = 80; // 上下文往前后各抓多少字，觉得不够就改这个数

  const KEYS = {
    a: 'wrong',     // 存错题本
    f: 'translate', // 划词翻译
    r: 'speak',     // 朗读
    d: 'debug',     // 诊断
  };

  let saved = 0;
  let audio = null;

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
    // 兜底：读工具栏那个页码输入框
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

  // ── 三个动作 ────────────────────────────────────────────────────────
  async function actWrong(item) {
    try {
      const res = await fetch('/api/wrong', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(item),
      });
      if (!res.ok) throw new Error((await res.text()).trim());
      const data = await res.json();
      saved++;
      toast(`✅ 已存第 ${item.page} 页（本次 ${saved} 条 · 共 ${data.total} 条）`);
    } catch (e) {
      toast('❌ 存失败：' + e.message);
      console.error('[错题本]', e);
    }
  }

  async function actTranslate(item) {
    panel(`🔄 翻译中…`, item.text);
    try {
      const res = await fetch('/api/translate?text=' + encodeURIComponent(item.text));
      if (!res.ok) throw new Error((await res.text()).trim());
      const data = await res.json();
      panel(data.text, item.text, data.from);
    } catch (e) {
      panel('❌ ' + e.message, item.text);
      console.error('[翻译]', e);
    }
  }

  function actSpeak(item) {
    if (audio) audio.pause(); // 连按就换成新的，不要叠着放
    audio = new Audio('/api/tts?text=' + encodeURIComponent(item.text));
    audio.play().catch((e) => {
      toast('❌ 放不出来：' + e.message);
      console.error('[朗读]', e);
    });
    audio.addEventListener('error', () => toast('❌ 朗读失败 —— 按 d 看控制台'));
    toast('🔊 ' + item.text.slice(0, 40));
  }

  // ── 按键 ────────────────────────────────────────────────────────────
  function typingIn(el) {
    if (!el) return false;
    const tag = el.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable;
  }

  function onKey(e) {
    // 规矩一：带修饰键的一律放行（Ctrl+F 查找、Ctrl+S 保存都得留着）
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    // 规矩二：焦点在输入框里（页码框、查找框）就当没看见
    if (typingIn(e.target)) return;

    const action = KEYS[e.key.toLowerCase()];
    if (!action) return;

    if (action === 'debug') {
      e.preventDefault();
      e.stopPropagation();
      const item = grab();
      console.log('[pdf_reader] 当前文件:', currentFile());
      console.log('[pdf_reader] 这次抓到的:', item);
      console.log('[pdf_reader] 本次已存错题:', saved);
      console.log('[pdf_reader] 文字层数量:', document.querySelectorAll('.textLayer').length);
      toast('🔍 已打印到控制台（F12）');
      return;
    }

    const item = grab();
    // 没选中东西就不拦 —— 这样 r 还是 pdf.js 的「旋转页面」，s / h 也不受影响
    if (!item) return;

    // 选中了才吃掉这个键。pdf.js 是在 window 冒泡阶段绑的 keydown，
    // 我们在 document 捕获阶段 stopPropagation，它就收不到了（否则 r 会把页面转 90°）
    e.preventDefault();
    e.stopPropagation();

    if (action === 'wrong') actWrong(item);
    else if (action === 'translate') actTranslate(item);
    else if (action === 'speak') actSpeak(item);
  }

  document.addEventListener('keydown', onKey, true);

  // ── 翻译浮层 ────────────────────────────────────────────────────────
  let panelEl;
  function panel(result, source, from) {
    if (!panelEl) {
      panelEl = document.createElement('div');
      panelEl.style.cssText = [
        'position:fixed', 'right:16px', 'bottom:56px', 'z-index:2147483647',
        'background:rgba(28,28,30,.96)', 'color:#f2f2f7', 'max-width:min(520px,70vw)',
        'font:14px/1.6 system-ui,sans-serif', 'padding:12px 14px', 'border-radius:10px',
        'box-shadow:0 6px 24px rgba(0,0,0,.4)', 'white-space:pre-wrap',
      ].join(';');
      document.body.appendChild(panelEl);
    }
    panelEl.textContent = '';

    const res = document.createElement('div');
    res.style.cssText = 'font-size:16px;margin-bottom:6px';
    res.textContent = result;

    const src = document.createElement('div');
    src.style.cssText = 'font-size:12px;opacity:.6';
    src.textContent = (from ? `[${from}] ` : '') + source;

    const hint = document.createElement('div');
    hint.style.cssText = 'font-size:11px;opacity:.4;margin-top:8px';
    hint.textContent = 'Esc 关 · r 朗读 · a 存错题本';

    panelEl.append(res, src, hint);
    panelEl.style.display = 'block';
  }

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && panelEl) panelEl.style.display = 'none';
  }, true);

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

  toast('📖 选中文字后：a 存错题 · f 翻译 · r 朗读 · d 诊断');
})();
