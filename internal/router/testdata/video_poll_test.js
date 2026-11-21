// Drives the embedded UI's video job polling against stubs: a transient poll
// failure must keep the job and retry, and a rejected DELETE must not clear it.
// Usage: node video_poll_test.js <path-to-app.js>
const assert = require('node:assert');
const fs = require('node:fs');

const leaf = () => ({
  style: {}, dataset: {}, children: [], options: [], files: [], classList: {
    add() {}, remove() {}, toggle() {}, contains() { return false; },
  },
  addEventListener() {}, removeEventListener() {}, setAttribute() {}, removeAttribute() {},
  appendChild() {}, remove() {}, focus() {}, click() {}, scrollIntoView() {},
});
const els = {};
function el() {
  return {
    style: {}, dataset: {}, classList: {
      add() {}, remove() {}, toggle() {}, contains() { return false; },
    },
    children: [leaf(), leaf(), leaf(), leaf()], options: [], files: [],
    hidden: false, value: '', disabled: false, checked: false, textContent: '', innerHTML: '',
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

const storage = {};
global.sessionStorage = {
  getItem(k) { return Object.prototype.hasOwnProperty.call(storage, k) ? storage[k] : null; },
  setItem(k, v) { storage[k] = String(v); },
  removeItem(k) { delete storage[k]; },
};
global.URL = { createObjectURL: () => 'blob:test', revokeObjectURL() {} };
global.requestAnimationFrame = (fn) => { fn(); return 1; };
global.cancelAnimationFrame = () => {};
global.setInterval = () => 0;

let nextTimer = 1;
const timers = new Map();
global.setTimeout = (fn) => { const id = nextTimer++; timers.set(id, fn); return id; };
global.clearTimeout = (id) => { timers.delete(id); };
async function tick() {
  assert.ok(timers.size > 0, 'expected a pending poll');
  const [id, fn] = timers.entries().next().value;
  timers.delete(id);
  await fn();
}

function reply(status, body) {
  return {
    ok: status >= 200 && status < 300,
    status,
    text: async () => (typeof body === 'string' ? body : JSON.stringify(body)),
    blob: async () => new Blob([body || ''], { type: 'video/mp4' }),
  };
}
const key = 'router.video.job';
const queue = [];
global.fetch = async (url, opts = {}) => {
  if (url === '/_router/status') return reply(200, { models: [] });
  if (url === '/_router/activity') return reply(200, { entries: [] });
  const step = queue.shift();
  assert.ok(step, 'unexpected request ' + (opts.method || 'GET') + ' ' + url);
  assert.strictEqual(opts.method || 'GET', step.method || 'GET', 'method for ' + url);
  assert.strictEqual(url, step.url);
  if (step.bodyCheck) step.bodyCheck(JSON.parse(opts.body));
  if (step.gate) { if (step.started) step.started(); await step.gate; }
  if (step.network) throw new TypeError('fetch failed');
  return reply(step.status, step.body);
};

(0, eval)(fs.readFileSync(process.argv[2], 'utf8'));

(async () => {
  await new Promise((resolve) => setImmediate(resolve));
  document.getElementById('video-model').value = 'fake-video';
  document.getElementById('video-prompt').value = 'a cat on a skateboard';
  document.getElementById('video-opt-seed').hidden = false;

  // Submit: the poll survives a 5xx and a network error, then plays the result.
  queue.push({ method: 'POST', url: '/v1/videos', status: 200, body: { id: 'job1', status: 'queued' }, bodyCheck(b) { assert.ok(!('seed' in b), 'an empty seed is omitted'); } });
  await window.sendVideo();
  assert.strictEqual(sessionStorage.getItem(key), 'job1', 'submit stores the job id');

  queue.push({ url: '/v1/videos/job1', status: 503, body: { error: 'backend busy' } });
  await tick();
  assert.strictEqual(sessionStorage.getItem(key), 'job1', 'a 5xx poll keeps the job id');
  assert.ok(timers.size > 0, 'a 5xx poll schedules the next check');
  assert.strictEqual(document.getElementById('video-status').textContent, 'Retrying: backend busy', 'a 5xx poll shows the retry reason');
  assert.notStrictEqual(document.getElementById('video-status').textContent, 'Failed');

  queue.push({ url: '/v1/videos/job1', network: true });
  await tick();
  assert.strictEqual(sessionStorage.getItem(key), 'job1', 'a network error keeps the job id');
  assert.ok(timers.size > 0, 'a network error schedules the next check');

  queue.push({ url: '/v1/videos/job1', status: 200, body: { id: 'job1', status: 'completed' } });
  queue.push({ url: '/v1/videos/job1/content', status: 200, body: 'fake mp4' });
  await tick();
  assert.strictEqual(sessionStorage.getItem(key), null, 'completion clears the stored job id');
  assert.match(document.getElementById('video-output').innerHTML, /<video controls/, 'completion plays the result');

  // Cancel: a rejected DELETE keeps the job and restarts polling.
  queue.push({ method: 'POST', url: '/v1/videos', status: 200, body: { id: 'job2', status: 'queued' } });
  await window.sendVideo();
  assert.strictEqual(sessionStorage.getItem(key), 'job2');

  queue.push({ method: 'DELETE', url: '/v1/videos/job2', status: 500, body: { error: 'cancel unsupported' } });
  await window.cancelVideo();
  assert.strictEqual(sessionStorage.getItem(key), 'job2', 'a rejected DELETE keeps the job id');
  assert.strictEqual(document.getElementById('video-status').textContent, 'Cancel failed');
  assert.ok(timers.size > 0, 'a rejected DELETE restarts polling');

  queue.push({ url: '/v1/videos/job2', status: 200, body: { id: 'job2', status: 'queued', progress: 10 } });
  await tick();
  assert.strictEqual(sessionStorage.getItem(key), 'job2', 'the later status check still owns the job');
  assert.ok(timers.size > 0, 'the later status check keeps polling');

  queue.push({ method: 'DELETE', url: '/v1/videos/job2', status: 200, body: {} });
  await window.cancelVideo();
  assert.strictEqual(sessionStorage.getItem(key), null, 'a successful DELETE clears the job');
  assert.strictEqual(document.getElementById('video-status').textContent, 'Cancelled');

  // A failed DELETE while a status check is still awaiting its reply must leave
  // one poll loop and must not let the late check repaint the cancel error.
  document.getElementById('video-seed').value = '900719925474099312345';
  queue.push({ method: 'POST', url: '/v1/videos', status: 200, body: { id: 'job3', status: 'queued' }, bodyCheck(b) { assert.strictEqual(b.seed, '900719925474099312345', 'a long seed keeps its digits'); } });
  await window.sendVideo();
  let releaseStatus;
  const pendingStatus = new Promise((resolve) => { releaseStatus = resolve; });
  queue.push({ url: '/v1/videos/job3', status: 200, body: { id: 'job3', status: 'queued', progress: 5 }, gate: pendingStatus });
  const pendingCheck = tick();
  queue.push({ method: 'DELETE', url: '/v1/videos/job3', status: 500, body: { error: 'cancel unsupported' } });
  await window.cancelVideo();
  releaseStatus();
  await pendingCheck;
  assert.strictEqual(timers.size, 1, 'a failed cancel during a pending check leaves one poll loop');
  assert.strictEqual(document.getElementById('video-status').textContent, 'Cancel failed', 'the late check does not repaint over the cancel error');

  queue.push({ method: 'DELETE', url: '/v1/videos/job3', status: 200, body: {} });
  await window.cancelVideo();
  assert.strictEqual(sessionStorage.getItem(key), null, 'a successful DELETE clears the job');

  // A completed job whose content keeps failing gives up after ten retries.
  queue.push({ method: 'POST', url: '/v1/videos', status: 200, body: { id: 'job4', status: 'queued' } });
  await window.sendVideo();
  for (let i = 0; i < 10; i++) {
    queue.push({ url: '/v1/videos/job4', status: 200, body: { id: 'job4', status: 'completed' } });
    queue.push({ url: '/v1/videos/job4/content', status: 500, body: 'backend down' });
    await tick();
    if (i < 9) assert.strictEqual(document.getElementById('video-status').textContent, 'Retrying: HTTP 500: backend down', 'content failures show the retry reason');
  }
  assert.strictEqual(sessionStorage.getItem(key), null, 'ten content failures end the job');
  assert.strictEqual(document.getElementById('video-status').textContent, 'Failed');

  // More than ten status-poll errors keep the job: only a final state or a 404
  // ends it.
  queue.push({ method: 'POST', url: '/v1/videos', status: 200, body: { id: 'job5', status: 'queued' } });
  await window.sendVideo();
  for (let i = 0; i < 12; i++) {
    queue.push({ url: '/v1/videos/job5', status: 503, body: { error: 'backend busy' } });
    await tick();
  }
  assert.strictEqual(sessionStorage.getItem(key), 'job5', 'status-poll errors never drop the job');
  assert.ok(timers.size > 0, 'status-poll errors keep polling');
  queue.push({ method: 'DELETE', url: '/v1/videos/job5', status: 200, body: {} });
  await window.cancelVideo();

  // A failed job shows the message the router sent.
  queue.push({ method: 'POST', url: '/v1/videos', status: 200, body: { id: 'job6', status: 'queued' } });
  await window.sendVideo();
  queue.push({ url: '/v1/videos/job6', status: 200, body: { id: 'job6', status: 'failed', error: { message: 'backend exploded' } } });
  await tick();
  assert.strictEqual(document.getElementById('video-status').textContent, 'Failed');
  assert.strictEqual(document.getElementById('video-output').textContent, 'backend exploded');

  // A job cancelled from elsewhere reports Cancelled, not Failed.
  queue.push({ method: 'POST', url: '/v1/videos', status: 200, body: { id: 'job7', status: 'queued' } });
  await window.sendVideo();
  queue.push({ url: '/v1/videos/job7', status: 200, body: { id: 'job7', status: 'cancelled' } });
  await tick();
  assert.strictEqual(document.getElementById('video-status').textContent, 'Cancelled');
  assert.strictEqual(sessionStorage.getItem(key), null, 'a job cancelled elsewhere clears its id');

  // Cancel while the completed video is downloading must not replace the player.
  queue.push({ method: 'POST', url: '/v1/videos', status: 200, body: { id: 'job8', status: 'queued' } });
  await window.sendVideo();
  let releaseContent, contentStarted, releaseDelete, deleteStarted;
  const contentGate = new Promise((resolve) => { releaseContent = resolve; });
  const contentReached = new Promise((resolve) => { contentStarted = resolve; });
  const deleteGate = new Promise((resolve) => { releaseDelete = resolve; });
  const deleteReached = new Promise((resolve) => { deleteStarted = resolve; });
  queue.push({ url: '/v1/videos/job8', status: 200, body: { id: 'job8', status: 'completed' } });
  queue.push({ url: '/v1/videos/job8/content', status: 200, body: 'fake mp4', gate: contentGate, started: contentStarted });
  const downloading = tick();
  await contentReached;
  queue.push({ method: 'DELETE', url: '/v1/videos/job8', status: 200, body: {}, gate: deleteGate, started: deleteStarted });
  const cancelling = window.cancelVideo();
  await deleteReached;
  releaseContent();
  await downloading;
  releaseDelete();
  await cancelling;
  assert.match(document.getElementById('video-output').innerHTML, /<video controls/, 'a late DELETE keeps the finished video');
  assert.strictEqual(document.getElementById('video-status').textContent, 'Completed');
})().catch((err) => { console.error(err); process.exit(1); });
