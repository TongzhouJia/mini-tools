// index.html 里那几个下标函数的测试。跑法：
//
//	node cmd/jp_reader/logic_test.js
//
// 直接从 index.html 里抠函数源码来跑，不重抄一份 —— 重抄的迟早跟真代码漂移。
// 钉的是「下标」：段落的 start/end 是 JS 的 UTF-16 下标，日语全是多字节，
// 这里错一位就是满屏乱码，而且要划到一半才看得出来。
const fs = require('fs');
const path = require('path');
const html = fs.readFileSync(process.argv[2] || path.join(__dirname, 'index.html'), 'utf8');
const src = html.match(/<script>\n?'use strict';([\s\S]*?)<\/script>/)[1];

function grab(name){
  const re = new RegExp('^function ' + name + '\\([\\s\\S]*?\\n\\}', 'm');
  const m = src.match(re);
  if(!m) throw new Error('抠不出函数 ' + name);
  return m[0];
}

let cur = null;                        // pieces() 用的全局
let $ = () => ({});                    // offsetOf 里的 $('#text')，测 offsetOf 时替换掉
eval(grab('autoSplit') + '\n' + grab('relocate') + '\n' + grab('pieces'));

let fail = 0;
function ok(cond, name, extra){
  console.log((cond ? 'ok   ' : 'FAIL ') + name + (cond ? '' : '  ' + (extra ?? '')));
  if(!cond) fail++;
}

// ── autoSplit ────────────────────────────────────────────────────────
const T1 = "私は毎朝六時に起きます。朝ご飯を食べてから、電車で会社へ行きます。\n" +
           "「もう行くの？」と母が聞きました。\n本当に大丈夫ですか？はい！";
const segs = autoSplit(T1);
console.log('\n[autoSplit] 切成', segs.length, '段');
segs.forEach((s, i) => console.log('   ' + i + ' [' + s.start + ',' + s.end + ') ' + JSON.stringify(s.text)));
ok(segs.every(s => s.text === T1.slice(s.start, s.end)), 'text 跟下标对得上');
ok(segs.every(s => s.text.trim() === s.text && s.text.length > 0), '段两端没有空白、也没有空段');
ok(segs.every((s, i) => i === 0 || s.start >= segs[i-1].end), '段之间不重叠且有序');
ok(segs.map(s => s.text).join('') === T1.replace(/\s/g, ''), '所有段拼起来 == 原文去掉空白', JSON.stringify(segs.map(s=>s.text).join('')));
ok(segs.some(s => s.text === '「もう行くの？」'), '闭引号跟着问号一起收进来');
ok(segs.some(s => s.text === 'はい！'), '结尾没有换行也收得住');

// ── relocate ─────────────────────────────────────────────────────────
console.log('\n[relocate]');
const old = [
  {start: 0,  end: 12, text: '私は毎朝六時に起きます。', note: '我每天早上六点起床。'},
  {start: 13, end: 21, text: '本当に大丈夫ですか？'},
];
const newText = "【第一課】\n私は毎朝六時に起きます。\nこれは新しい行です。\n本当に大丈夫ですか？";
const r1 = relocate(old, newText);
ok(r1.lost === 0, '两段都还在');
ok(r1.kept.every(s => s.text === newText.slice(s.start, s.end)), '重新对位后下标正确',
   JSON.stringify(r1.kept.map(s => [s.start, s.end, newText.slice(s.start, s.end)])));
ok(r1.kept[0].note === '我每天早上六点起床。', '译文跟着一起保住');

const r2 = relocate(old, "私は毎朝六時に起きます。だけ残った。");
ok(r2.lost === 1 && r2.kept.length === 1, '原文里删掉的那段被丢掉并计数');

// 同一句话出现两次时，第二段不该倒退回第一处
const dup = [{start:0,end:5,text:'おはよう'},{start:6,end:11,text:'おはよう'}];
const r3 = relocate(dup, 'おはよう、おはよう');
ok(r3.lost === 0 && r3.kept[0].start === 0 && r3.kept[1].start === 5, '重复句子按顺序往后找，不会撞在一起',
   JSON.stringify(r3.kept));

// ── pieces ───────────────────────────────────────────────────────────
console.log('\n[pieces]');
cur = {text: T1, segments: segs};
let ps = pieces();
ok(ps.map(p => cur.text.slice(p.start, p.end)).join('') === T1, '片段拼起来 == 原文（不丢字不重复）');
ok(ps.every((p, i) => i === 0 || p.start === ps[i-1].end), '片段首尾相接没有空洞');

// 只划中间一段：前后应该各留一块未划分的
cur = {text: 'あいうえお', segments: [{start: 1, end: 3, text: 'いう'}]};
ps = pieces();
ok(ps.length === 3 && ps[0].seg === null && ps[1].seg === 0 && ps[2].seg === null, '中间一段 → 三块',
   JSON.stringify(ps));

// 整篇正好被段铺满：不该冒出长度为 0 的空片段
cur = {text: 'あい', segments: [{start: 0, end: 2, text: 'あい'}]};
ps = pieces();
ok(ps.length === 1 && ps[0].seg === 0, '铺满时不产生空片段', JSON.stringify(ps));

// ── offsetOf ─────────────────────────────────────────────────────────
// 手搓一个够用的 DOM 替身：验证「span 被浏览器切成多个文本节点」时下标还对不对
console.log('\n[offsetOf]');
function textNode(s){ return {nodeType: 3, textContent: s, childNodes: []}; }
function spanOf(off, parts){
  const el = {nodeType: 1, dataset: {off: String(off)}, childNodes: parts.map(textNode)};
  el.childNodes.forEach(n => n.parentElement = el);
  el.textContent = parts.join('');
  return el;
}
const box = {nodeType: 1, dataset: {}, childNodes: []};
const spanA = spanOf(0, ['私は毎朝']);                 // 单个文本节点
const spanB = spanOf(4, ['六時に', '起きます。']);      // 被切成两个文本节点
box.childNodes.push(spanA, spanB);
box.textContent = '私は毎朝六時に起きます。';
[spanA, spanB].forEach(s => s.parentElement = box);

$ = (sel) => sel === '#text' ? box : null;
eval(grab('offsetOf'));

ok(offsetOf(spanA.childNodes[0], 2) === 2, 'span 内第一个文本节点：0 + 2');
ok(offsetOf(spanB.childNodes[0], 1) === 5, '第二个 span 的第一个文本节点：4 + 0 + 1');
ok(offsetOf(spanB.childNodes[1], 2) === 9, '第二个 span 的第二个文本节点：4 + 3 + 2（关键用例）',
   '得到 ' + offsetOf(spanB.childNodes[1], 2));
ok(offsetOf(box, 0) === 0, '端点落在容器上：全选起点 = 0');
ok(offsetOf(box, 2) === box.textContent.length, '端点落在容器上：全选终点 = 全文长度',
   '得到 ' + offsetOf(box, 2));
ok(offsetOf(spanB, 0) === 4, '端点落在 span 元素本身：退到它的起点');

console.log('\n' + (fail ? fail + ' 项没过' : '全部通过'));
process.exit(fail ? 1 : 0);
