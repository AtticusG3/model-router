// Drives the embedded UI's Model catalog against stubs: a known family draws
// its bundled icon, an unknown row keeps the generic model glyph, and a row
// with both a family and peers draws the icon and the peer chips. A metadata
// icon_url is drawn as an <img> only when safeUrl passes it, and info shows as chips.
// Usage: node model_family_test.js <path-to-app.js>
const assert = require('node:assert');
const fs = require('node:fs');

const els = {};
function el() {
  return {
    style: {}, dataset: {}, classList: {
      add() {}, remove() {}, toggle() {}, contains() { return false; },
    },
    children: [], options: [], files: [], hidden: false, value: '', disabled: false,
    checked: false, textContent: '', innerHTML: '',
    addEventListener() {}, removeEventListener() {}, setAttribute() {}, removeAttribute() {},
    querySelector() { return el(); }, querySelectorAll() { return []; },
    appendChild() {}, insertAdjacentHTML() {}, remove() {}, focus() {}, click() {},
    getBoundingClientRect() { return {top: 0}; }, scrollIntoView() {},
  };
}
global.document = {
  getElementById(id) { return els[id] || (els[id] = el()); },
  querySelector() { return el(); }, querySelectorAll() { return []; },
  createElement() { return el(); }, createElementNS() { return el(); },
  addEventListener() {}, activeElement: null,
};
global.window = global;
global.location = { hash: '', protocol: 'http:', host: 'localhost' };
global.history = { replaceState() {} };
global.scrollTo = () => {};
global.alert = () => {};
global.sessionStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
global.URL = { createObjectURL: () => 'blob:test', revokeObjectURL() {} };
global.requestAnimationFrame = (fn) => { fn(); return 1; };
global.cancelAnimationFrame = () => {};
global.setInterval = () => 0;
global.setTimeout = () => 0;
global.clearTimeout = () => {};

let models = [];
global.fetch = async (url) => {
  const body = url === '/_router/activity'
    ? { entries: [] }
    : { node: 'test', models, telemetry: {}, peers: {} };
  return { ok: true, status: 200, text: async () => JSON.stringify(body) };
};

(0, eval)(fs.readFileSync(process.argv[2], 'utf8'));

(async () => {
  await new Promise((resolve) => setImmediate(resolve));
  models.length = 0;
  models.push(
    { id: 'qwen38-flash-next', family: { id: 'qwen', name: 'Qwen', owner: 'Alibaba' }, peers: [{ name: 'nodeb', state: 'loaded', gpus: [1], in_flight: 1, slots: 2 }] },
    { id: 'mystery-model' },
    { id: 'custom-model', family: { id: 'constructor' } },
    { id: 'meta-model', family: { id: 'qwen', name: 'Qwen' }, info: { icon_url: 'https://icons.example.test/qwen.svg', owner: 'Example Tune Co', params: '27B', context_length: 32768, quant: 'Q4_K_M' } },
    { id: 'evil-model', family: { id: 'evil', name: 'Evil' }, info: { icon_url: 'javascript:alert(1)' } },
    { id: 'image-model', info: { size: '1024x768', steps: 8 } },
  );
  await window.refresh();
  const html = els.models.innerHTML;
  assert.match(html, /fill="#6950EF"/, 'the qwen family icon is drawn');
  assert.match(html, /M6 4\.5 12 2l6 2\.5v6L12 13l-6-2\.5v-6Z/, 'an unknown family keeps the generic model glyph');
  assert.match(html, /nodeb/, 'the peer chip is drawn beside the family icon');
  assert.match(html, /aria-label="Qwen \(Alibaba\)"/, 'the family icon is labelled');
  assert.strictEqual((html.match(/class="model-glyph family"/g) || []).length, 2, 'only a bundled icon or a safe icon_url gets the family glyph');
  assert.match(html, /<img src="https:\/\/icons\.example\.test\/qwen\.svg" alt=""/, 'a metadata icon_url is drawn as an img');
  assert.ok(!html.includes('javascript:'), 'a javascript: icon_url is not rendered');
  for (const chip of ['Example Tune Co', '27B params', '32k ctx', 'Q4_K_M', '1024x768', '8 steps']) {
    assert.ok(html.includes('<span class="chip">' + chip + '</span>'), 'info chip ' + chip);
  }
  assert.ok(!html.includes('function Object'), 'a family id naming an object property falls back to the generic glyph');
})().catch((err) => { console.error(err); process.exit(1); });
