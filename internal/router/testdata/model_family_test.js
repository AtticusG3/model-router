// Drives the embedded UI's Model catalog against stubs: a known family draws
// its bundled icon, an unknown row keeps the generic model glyph. A metadata
// icon_url is drawn as an <img> only when safeUrl passes it (with or without a
// family), info shows as one inline text line, and a hub link only when
// safeUrl passes it. Then each row's title, Origin, Load/Unload toggle and
// Open UI state are checked against its process state and origin.
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
    { id: 'hub-model', info: { icon_url: 'https://avatars.example.test/org.png', licence: 'apache-2.0', modality: 'text-generation', hub: 'Hugging Face', hub_url: 'https://huggingface.co/Example-Org/Hub-Model' } },
    { id: 'hub-evil', info: { hub: 'Hugging Face', hub_url: 'javascript:alert(2)' } },
    // Process states drive the Load/Unload toggle and Open UI; freshness only shows in State.
    { id: 'stopped-chat', name: 'Example Chat 8B Instruct', state: 'stopped', info: { context_length: 8192 } },
    { id: 'running-chat', state: 'running', freshness: 'stale' },
    { id: 'starting-chat', state: 'starting' },
    { id: 'stopping-chat', state: 'stopping', freshness: 'fresh' },
    { id: 'remote-chat', origin: 'remote', state: 'available', peers: [{ name: 'nodeb', state: 'loaded', gpus: [0], in_flight: 0, slots: 4 }, { name: 'nodec', state: 'stale' }] },
  );
  await window.refresh();
  const html = els.models.innerHTML;
  assert.match(html, /fill="#6950EF"/, 'the qwen family icon is drawn');
  assert.match(html, /M6 4\.5 12 2l6 2\.5v6L12 13l-6-2\.5v-6Z/, 'an unknown family keeps the generic model glyph');
  assert.match(html, /aria-label="Qwen \(Alibaba\)"/, 'the family icon is labelled');
  assert.strictEqual((html.match(/class="model-glyph family"/g) || []).length, 3, 'only a bundled icon or a safe icon_url gets the family glyph');
  assert.match(html, /<img src="https:\/\/avatars\.example\.test\/org\.png"/, 'a hub avatar is drawn for a row with no family');
  assert.match(html, /<a href="https:\/\/huggingface\.co\/Example-Org\/Hub-Model" target="_blank" rel="noopener noreferrer">from Hugging Face<\/a>/, 'the hub link is drawn');
  assert.ok(!html.includes('alert(2)'), 'an unsafe hub_url is not rendered');
  assert.match(html, /<img src="https:\/\/icons\.example\.test\/qwen\.svg" alt=""/, 'a metadata icon_url is drawn as an img');
  assert.ok(!html.includes('javascript:'), 'a javascript: icon_url is not rendered');
  assert.ok(!html.includes('function Object'), 'a family id naming an object property falls back to the generic glyph');

  // One <tr> per model, in order; cells are Model, Origin, State, VRAM, Actions.
  const rows = html.split('<tr>').slice(1);
  assert.strictEqual(rows.length, models.length);
  const row = (id) => rows[models.findIndex((m) => m.id === id)];
  const cells = (id) => row(id).split('<td').slice(1).map((c) => '<td' + c);
  assert.strictEqual(cells('mystery-model').length, 5, 'no Peers column');
  const text = (s) => s.replace(/<[^>]*>/g, '').trim();
  const meta = (id) => (/<span class="model-meta">(.*?)<\/span><\/div>/.exec(row(id)) || [])[1];

  // Details are one inline text line, not chips, with stanza details first and the hub link kept.
  assert.ok(!html.includes('class="chip'), 'no info pills');
  assert.strictEqual(text(meta('meta-model')), '27B params · 32k ctx · Q4_K_M · Example Tune Co');
  assert.strictEqual(text(meta('image-model')), '1024x768 · 8 steps');
  assert.strictEqual(text(meta('hub-model')), 'text-generation · apache-2.0from Hugging Face', 'hub link sits after the text (the CSS adds its separator)');
  assert.strictEqual(meta('mystery-model'), undefined, 'no details line without info');

  // Title: the name when there is one, else the id; the id stays on hover.
  const title = (id) => /<span class="model-id model-title" title="([^"]*)">([^<]*)<\/span>/.exec(row(id)).slice(1);
  assert.deepStrictEqual(title('stopped-chat'), ['Example Chat 8B Instruct\nstopped-chat', 'Example Chat 8B Instruct']);
  assert.ok(!text(cells('stopped-chat')[0]).includes('stopped-chat'), 'the id is not shown when a name exists');
  assert.deepStrictEqual(title('mystery-model'), ['mystery-model', 'mystery-model']);

  // Origin: local (peers as muted names with details on hover), or the peer names for a remote id.
  assert.strictEqual(text(cells('mystery-model')[1]), 'local');
  assert.strictEqual(text(cells('qwen38-flash-next')[1]), 'local + nodeb');
  assert.match(cells('qwen38-flash-next')[1], /title="Also on\nnodeb · loaded · gpu1 · 1\/2 · no telemetry received"/);
  assert.strictEqual(text(cells('remote-chat')[1]), 'nodeb, nodec');
  assert.match(cells('remote-chat')[1], /title="nodeb · loaded · gpu0 · 0\/4 · no telemetry received\nnodec · stale · no telemetry received"/);
  assert.ok(!models.some((m) => text(cells(m.id)[1]).includes('remote')), 'remote is never an Origin label');

  // State keeps freshness.
  assert.strictEqual(text(cells('running-chat')[2]), 'stale');

  // Actions: one valid toggle, keyed off process state; Open UI live only when starting or running.
  const acts = (id) => {
    const c = cells(id)[4];
    const open = /<a [^>]*>Open UI<\/a>/.exec(c)[0];
    return {
      open: /href="\/upstream\//.test(open) ? 'link' : (/aria-disabled="true"/.test(open) && !/href=/.test(open) ? 'off' : 'bad'),
      tip: (/title="([^"]*)"/.exec(open) || [])[1],
      toggle: [...c.matchAll(/<button([^>]*)>(.*?)<\/button>/g)].map(([, a, t]) => (/disabled/.test(a) ? 'disabled ' : '') + (/data-act="(\w+)"/.exec(a) || [, ''])[1] + ' ' + text(t)),
    };
  };
  const off = 'Available once this model is loading or loaded on this node';
  assert.deepStrictEqual(acts('stopped-chat'), { open: 'off', tip: off, toggle: ['load Load'] });
  assert.deepStrictEqual(acts('mystery-model'), { open: 'off', tip: off, toggle: ['load Load'] }, 'no state reads as stopped');
  assert.deepStrictEqual(acts('running-chat'), { open: 'link', tip: undefined, toggle: ['unload Unload'] }, 'stale freshness still unloads');
  assert.deepStrictEqual(acts('starting-chat'), { open: 'link', tip: undefined, toggle: ['disabled  Loading…'] });
  assert.match(cells('starting-chat')[4], /aria-busy="true"><span class="spinner"/);
  assert.deepStrictEqual(acts('stopping-chat'), { open: 'off', tip: off, toggle: ['disabled  Unloading…'] });
  assert.strictEqual(text(cells('remote-chat')[4]), '—', 'remote rows get no Open UI or Load/Unload control');
  assert.match(cells('running-chat')[4], /<a class="ghost" href="\/upstream\/running-chat\/" target="_blank" rel="noopener">Open UI<\/a>/);

  // The click handler loads or unloads the button's model.
  const posts = [];
  const realFetch = global.fetch;
  global.fetch = async (url, opts) => { if (opts && opts.method === 'POST') posts.push(url + ' ' + opts.body); return realFetch(url, opts); };
  const btn = (act, id) => ({ target: { closest: () => ({ dataset: { act, id } }) } });
  els.models.onclick(btn('load', 'stopped-chat'));
  els.models.onclick(btn('unload', 'running-chat'));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepStrictEqual(posts, ['/_router/load {"model_id":"stopped-chat"}', '/_router/unload {"model_id":"running-chat"}']);

  models.length = 0;
  await window.refresh();
  assert.match(els.models.innerHTML, /colspan="5"/, 'the empty row spans the five columns');
})().catch((err) => { console.error(err); process.exit(1); });
