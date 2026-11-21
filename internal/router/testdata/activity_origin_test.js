// Drives the Activity view's peer filter against rows the Go API built (see
// TestEmbeddedUIActivityOriginFilter): a snapshot, a progress event and a done
// event, then each selector option. The path-group chips stay on their
// default (chat and SD API), so an embeddings row is hidden throughout.
// Usage: node activity_origin_test.js <path-to-app.js> <fixtures.json>
const assert = require('node:assert');
const fs = require('node:fs');

const fx = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));

// A small DOM: enough tree for syncRows to insert, reorder and remove rows.
function node() {
  const n = {
    children: [], parent: null, dataset: {}, style: {}, className: '', innerHTML: '', textContent: '', value: '',
    classList: {
      s: new Set(),
      add(c) { this.s.add(c); }, remove(c) { this.s.delete(c); },
      toggle(c, on) { if (on ?? !this.s.has(c)) this.s.add(c); else this.s.delete(c); },
      contains(c) { return this.s.has(c); },
    },
    appendChild(c) { c.parent = n; n.children.push(c); return c; },
    insertBefore(c, ref) {
      if (c.parent) c.parent.children.splice(c.parent.children.indexOf(c), 1);
      c.parent = n;
      const i = ref ? n.children.indexOf(ref) : -1;
      if (i < 0) n.children.push(c); else n.children.splice(i, 0, c);
      return c;
    },
    remove() { if (n.parent) n.parent.children.splice(n.parent.children.indexOf(n), 1); n.parent = null; },
    get cells() { return n.children; },
    get firstElementChild() { return n.children[0] || null; },
    get nextElementSibling() { const s = n.parent && n.parent.children; return (s && s[s.indexOf(n) + 1]) || null; },
    get options() { return [...n.innerHTML.matchAll(/<option value="([^"]*)"/g)].map((m) => ({ value: m[1] })); },
    querySelector() { return node(); }, querySelectorAll() { return []; },
    setAttribute() {}, removeAttribute() {}, toggleAttribute() {}, addEventListener() {},
    insertAdjacentHTML() {}, focus() {}, getBoundingClientRect() { return { top: 0 }; },
  };
  return n;
}
const els = {};
for (const id of ['chart-gen', 'chart-prompt', 'chart-dur', 'chart-ttft']) {
  els[id] = node();
  els[id].children = [node(), node(), node(), node()];
}
const navActivity = node();
navActivity.dataset.view = 'activity';
global.document = {
  getElementById(id) { return els[id] || (els[id] = node()); },
  querySelector() { return null; },
  querySelectorAll(sel) { return sel === '.nav button' ? [navActivity] : []; },
  createElement() { return node(); }, createElementNS() { return node(); }, addEventListener() {},
};
global.window = global;
global.location = { hash: '', protocol: 'http:', host: 'example.test' };
global.history = { replaceState() {} };
global.sessionStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
global.setInterval = () => 0;
global.requestAnimationFrame = (fn) => { fn(); return 1; };
global.cancelAnimationFrame = () => {};
global.EventSource = class {
  constructor(url) { this.url = url; this.on = {}; global.EventSource.last = this; }
  addEventListener(type, fn) { this.on[type] = fn; }
  close() {}
};
global.EventSource.CLOSED = 2;
global.fetch = async (url) => {
  const body = url === '/_router/status' ? fx.status : {};
  return { ok: true, status: 200, headers: { get: () => null }, text: async () => JSON.stringify(body) };
};

(0, eval)(fs.readFileSync(process.argv[2], 'utf8'));

const $ = (id) => document.getElementById(id);
const send = (type, data) => global.EventSource.last.on[type]({ data: JSON.stringify(data) });
// Shown rows as "model origin", top to bottom; rows fading out are not counted.
const shown = () => $('activity-rows').children.filter((tr) => !tr.classList.contains('leaving'))
  .map((tr) => /class="model-id">([^<]*)</.exec(tr.cells[1].innerHTML)[1] + ' ' + tr.cells[2].innerHTML).sort();
const pick = (v) => { $('act-peer').value = v; $('act-peer').onchange(); return shown(); };

(async () => {
  await window.refresh();
  navActivity.onclick();
  send('snapshot', fx.snapshot);
  send('progress', fx.progress);
  send('done', fx.done);

  assert.deepStrictEqual($('act-peer').options.map((o) => o.value),
    ['', 'local', 'p:bravo', 'p:charlie', 'p:outsider'],
    'options are All, Local, the configured peers and the unlisted sender');

  assert.deepStrictEqual(pick(''), [
    'client-chat local', 'forwarded-in bravo', 'forwarded-out local', 'peer-done charlie', 'peer-run charlie',
    'running-in outsider', 'unlisted-image outsider', 'unlisted-image outsider',
  ], 'All shows every row in the ticked path groups, with its origin');
  assert.deepStrictEqual(pick('local'), ['client-chat local', 'forwarded-out local'],
    'Local is the clients of this node, including a request it forwarded out');
  assert.deepStrictEqual(pick('p:bravo'), ['forwarded-in bravo'], 'a peer shows what it sent here');
  assert.deepStrictEqual(pick('p:charlie'), ['peer-done charlie', 'peer-run charlie'], 'a peer shows its own requests from telemetry');
  assert.deepStrictEqual(pick('p:outsider'), ['running-in outsider', 'unlisted-image outsider', 'unlisted-image outsider'],
    'a sender outside the config is listed and filters');

  // The pick survives a status refresh even when no row or peer names it any more.
  send('snapshot', { entries: [], in_progress: [] });
  await window.refresh();
  assert.strictEqual($('act-peer').value, 'p:outsider');
  assert.ok($('act-peer').options.some((o) => o.value === 'p:outsider'), 'the current pick stays an option');
  process.exit(0);
})().catch((err) => { console.error(err); process.exit(1); });
