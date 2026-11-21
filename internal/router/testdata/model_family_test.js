// Drives the embedded UI's Model catalog against stubs: a known family draws
// its bundled icon, an unknown row keeps the generic model glyph, and a row
// with both a family and peers draws the icon and the peer chips.
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
  );
  await window.refresh();
  const html = els.models.innerHTML;
  assert.match(html, /fill="#6950EF"/, 'the qwen family icon is drawn');
  assert.match(html, /M6 4\.5 12 2l6 2\.5v6L12 13l-6-2\.5v-6Z/, 'an unknown family keeps the generic model glyph');
  assert.match(html, /nodeb/, 'the peer chip is drawn beside the family icon');
  assert.match(html, /aria-label="Qwen \(Alibaba\)"/, 'the family icon is labelled');
  assert.strictEqual((html.match(/class="model-glyph family"/g) || []).length, 1, 'only the known family gets the family glyph');
  assert.ok(!html.includes('function Object'), 'a family id naming an object property falls back to the generic glyph');
})().catch((err) => { console.error(err); process.exit(1); });
