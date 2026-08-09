// capture.js —— 注入到 pdf.js viewer 里：划词存单词 / 高亮 / 翻译 / 朗读。
// 由 pdf_reader 在伺服 viewer.html 时塞进去，不是油猴脚本，改完重启工具就生效。
//
// 快捷键是单独字母（a / s / f / r / d），不带修饰键——阅读时不打字，按着顺手。
// 代价是要跟 pdf.js 自己的单键抢：r 是「旋转页面」、s 是选择工具、h 是抓手。
// 处理办法见下面 onKey 里的两条规矩。
(function () {
  'use strict';

  // ── 配置 ────────────────────────────────────────────────────────────
  const CONTEXT_CHARS = 80;                       // 上下文往前后各抓多少字
  const HL_COLOR = 'rgba(255, 214, 0, .45)';      // 高亮颜色，想标红就换成 rgba(255,0,0,.3)
  const HL_OVERLAP = 0.3;                         // 重叠超过这个比例就当成「再按一次 = 取消高亮」

  const KEYS = {
    a: 'word',      // 存单词本
    s: 'highlight', // 高亮
    f: 'translate', // 翻译（备用，正常靠谷歌翻译插件自动弹）
    r: 'speak',     // 朗读
    d: 'debug',     // 诊断
  };

  let saved = 0;
  let hls = [];      // 当前文件的全部高亮
  let audio = null;

  const file = currentFile();

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
  // 里面全是 <span>。所以从选区往上爬就能拿到页码和页面容器。
  function pageDivOf(node) {
    const el = node && (node.nodeType === 1 ? node : node.parentElement);
    return (el && el.closest && el.closest('.page[data-page-number]')) || null;
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
    if (!text || !sel.rangeCount) return null;
    const node = sel.anchorNode;
    const pageDiv = pageDivOf(node);
    const input = document.getElementById('pageNumber');
    return {
      text,
      file,
      pageDiv,
      range: sel.getRangeAt(0),
      page: pageDiv ? pageDiv.getAttribute('data-page-number') : (input ? input.value : ''),
      context: contextOf(node, text),
    };
  }

  // ── a：存单词本 ────────────────────────────────────────────────────
  // 只送选中的文字，翻译和分天归档都在服务端做
  async function actWord(item) {
    try {
      const res = await fetch('/api/words', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text: item.text }),
      });
      if (!res.ok) throw new Error((await res.text()).trim());
      const data = await res.json();
      if (data.dup) {
        toast(`🔁 day${String(data.day).padStart(2, '0')} 里已经有了：${data.en} — ${data.zh}`);
        return;
      }
      saved++;
      const zh = data.zh || '（没翻出来，手工补）';
      toast(`✅ ${data.en} — ${zh}（${data.file} · 共 ${data.total} 个）`);
      if (data.warn) console.warn('[单词本]', data.warn);
    } catch (e) {
      toast('❌ 存失败：' + e.message);
      console.error('[单词本]', e);
    }
  }

  // ── s：高亮 ─────────────────────────────────────────────────────────
  // 坐标一律换算成「占页面宽高的百分比」再存。存像素的话换个缩放级别就全错位了，
  // 百分比跟缩放无关，重画时乘回当前页面尺寸即可。
  function rectsOf(item) {
    const pr = item.pageDiv.getBoundingClientRect();
    if (!pr.width || !pr.height) return [];
    return [...item.range.getClientRects()]
      .filter((r) => r.width > 1 && r.height > 1)
      .map((r) => ({
        x: (r.left - pr.left) / pr.width,
        y: (r.top - pr.top) / pr.height,
        w: r.width / pr.width,
        h: r.height / pr.height,
      }))
      // 跨页选中时另一页的矩形会落到 0..1 之外，丢掉——只高亮锚点所在这一页
      .filter((r) => r.y > -0.02 && r.y < 1.02);
  }

  function overlaps(a, b) {
    const ox = Math.max(0, Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x));
    const oy = Math.max(0, Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y));
    const inter = ox * oy;
    const small = Math.min(a.w * a.h, b.w * b.h) || 1;
    return inter / small > HL_OVERLAP;
  }

  async function actHighlight(item) {
    if (!item.pageDiv) return toast('⚠️ 定位不到页面，翻一下页再试');
    const rects = rectsOf(item);
    if (!rects.length) return toast('⚠️ 这段选区画不出高亮');
    const page = parseInt(item.page, 10);

    // 压在已有高亮上再按一次 = 取消掉它
    const hit = hls.find((h) => h.page === page && h.rects.some((r) => rects.some((n) => overlaps(r, n))));
    if (hit) {
      try {
        const res = await fetch('/api/highlights?id=' + encodeURIComponent(hit.id), { method: 'DELETE' });
        if (!res.ok) throw new Error((await res.text()).trim());
        hls = hls.filter((h) => h.id !== hit.id);
        drawPage(item.pageDiv);
        window.getSelection().removeAllRanges();
        toast('🧽 高亮去掉了');
      } catch (e) {
        toast('❌ 删不掉：' + e.message);
      }
      return;
    }

    const h = { file: item.file, page, text: item.text, color: HL_COLOR, rects };
    try {
      const res = await fetch('/api/highlights', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(h),
      });
      if (!res.ok) throw new Error((await res.text()).trim());
      const data = await res.json();
      hls.push({ ...h, id: data.id });
      drawPage(item.pageDiv);
      window.getSelection().removeAllRanges();
      toast(`🖍️ 高亮了（本文件共 ${hls.length} 处）`);
    } catch (e) {
      toast('❌ 高亮存不上：' + e.message);
      console.error('[高亮]', e);
    }
  }

  // 每个 .page 里挂一层自己的覆盖层。pointer-events:none，所以不挡选中；
  // .page 本身是 position:relative，百分比定位直接就对得上。
  function layerOf(pageDiv) {
    let l = pageDiv.querySelector(':scope > .pr-hl-layer');
    if (!l) {
      l = document.createElement('div');
      l.className = 'pr-hl-layer';
      l.style.cssText = 'position:absolute;inset:0;pointer-events:none;z-index:1';
      pageDiv.appendChild(l);
    }
    return l;
  }

  function drawPage(pageDiv) {
    if (!pageDiv) return;
    const page = parseInt(pageDiv.getAttribute('data-page-number'), 10);
    const l = layerOf(pageDiv);
    l.textContent = '';
    for (const h of hls) {
      if (h.page !== page) continue;
      for (const r of h.rects) {
        const d = document.createElement('div');
        d.style.cssText = [
          'position:absolute',
          `left:${r.x * 100}%`, `top:${r.y * 100}%`,
          `width:${r.w * 100}%`, `height:${r.h * 100}%`,
          `background:${h.color || HL_COLOR}`,
          'mix-blend-mode:multiply', 'border-radius:2px',
        ].join(';');
        l.appendChild(d);
      }
    }
  }

  function drawAll() {
    document.querySelectorAll('.page[data-page-number]').forEach(drawPage);
  }

  // ── f：翻译（备用）──────────────────────────────────────────────────
  async function actTranslate(item) {
    panel('🔄 翻译中…', item.text);
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

  // ── r：朗读 ─────────────────────────────────────────────────────────
  // 直接走自己的 GCP TTS。服务端有磁盘缓存，同一个词念第二遍不花钱。
  //
  // 特意用 fetch + blob 而不是 new Audio(url)：<audio> 拿到非音频响应时
  // 只会甩一句「no supported source found」，服务端真正的报错全被吞掉。
  // 先 fetch 就能把那句人话读出来。
  async function actSpeak(item) {
    if (audio) { audio.pause(); URL.revokeObjectURL(audio.src); audio = null; }
    try {
      const res = await fetch('/api/tts?text=' + encodeURIComponent(item.text));
      if (!res.ok) throw new Error(`${res.status} ${(await res.text()).trim()}`);
      const blob = await res.blob();
      if (!blob.size) throw new Error('服务端回了个空音频');
      audio = new Audio(URL.createObjectURL(blob));
      audio.addEventListener('error', () =>
        toast(`❌ 解不了码（${blob.type} ${blob.size}B）—— 按 d 看控制台`));
      await audio.play();
      toast('🔊 ' + item.text.slice(0, 40));
    } catch (e) {
      toast('❌ 朗读失败：' + e.message);
      console.error('[朗读]', e);
    }
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
      console.log('[pdf_reader] 文件:', file);
      console.log('[pdf_reader] 这次抓到的:', grab());
      console.log('[pdf_reader] 本次已存单词:', saved, '· 本文件高亮:', hls.length);
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

    if (action === 'word') actWord(item);
    else if (action === 'highlight') actHighlight(item);
    else if (action === 'translate') actTranslate(item);
    else if (action === 'speak') actSpeak(item);
  }

  document.addEventListener('keydown', onKey, true);
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && panelEl) panelEl.style.display = 'none';
  }, true);

  // ── 跟 pdf.js 挂钩：页面重绘时把高亮补回去 ──────────────────────────
  // pdf.js 会把滚出视野的页面卸载掉，回来时重新渲染，高亮得跟着重画。
  // viewer.mjs 把自己挂在 window.PDFViewerApplication 上，等它出现就行。
  (function hook() {
    const app = window.PDFViewerApplication;
    if (!app || !app.eventBus) return setTimeout(hook, 100);
    app.eventBus.on('pagerendered', (e) => drawPage(e.source && e.source.div));
    app.eventBus.on('scalechanging', () => setTimeout(drawAll, 60));
    app.eventBus.on('rotationchanging', () => setTimeout(drawAll, 60));
  })();

  fetch('/api/highlights?file=' + encodeURIComponent(file))
    .then((r) => r.json())
    .then((d) => { hls = d || []; drawAll(); })
    .catch((e) => console.error('[高亮] 读不到:', e));

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
    hint.textContent = 'Esc 关 · r 朗读 · a 存单词本 · s 高亮';

    panelEl.append(res, src, hint);
    panelEl.style.display = 'block';
  }

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

  toast('📖 选中文字后：a 存单词 · s 高亮 · r 朗读 · f 翻译 · d 诊断');
})();
