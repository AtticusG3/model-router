// Drives the Playground history against stubs: entries are newest first, a
// click restores inputs and output, Reuse fills inputs only, and the 50 entry
// and 20 media caps and Clear all revoke the blob URLs they drop. It also
// replays a speech restore across models whose voices differ, and covers the
// streamed speech, transcription, video and realtime history paths.
// Usage: node history_test.js <path-to-app.js>
const assert = require('node:assert');
const fs = require('node:fs');

const els = {};
function el() {
  return {
    style: {}, dataset: {}, classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    children: [el0(), el0(), el0(), el0()], files: [],
    get options() { return [...this.innerHTML.matchAll(/<option value="([^"]*)"/g)].map((m) => ({ value: m[1] })); },
    hidden: false, value: '', disabled: false, textContent: '', innerHTML: '', className: '',
    addEventListener() {}, setAttribute() {}, removeAttribute() {},
    querySelector() { return el(); }, querySelectorAll() { return []; },
    appendChild() {}, focus() {}, click() {}, getBoundingClientRect() { return { top: 0 }; }, scrollIntoView() {},
  };
}
function el0() { return { style: {}, setAttribute() {} }; }
global.document = {
  getElementById(id) { return els[id] || (els[id] = el()); },
  querySelector() { return el(); }, querySelectorAll() { return []; },
  createElement() { return el(); }, createElementNS() { return el(); }, addEventListener() {},
};
global.window = global;
global.location = { hash: '', protocol: 'http:', host: 'example.test' };
global.history = { replaceState() {} };
global.scrollTo = () => {};
global.sessionStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
let made = 0;
const revoked = [];
global.URL = { createObjectURL: () => 'blob:' + (++made), revokeObjectURL(u) { revoked.push(u); } };
global.requestAnimationFrame = (fn) => { fn(); return 1; };
global.cancelAnimationFrame = () => {};
global.setInterval = () => 0;
global.setTimeout = (fn) => { setImmediate(fn); return 0; };
global.clearTimeout = () => {};
global.MediaSource = class {
  static isTypeSupported() { return true; }
  addEventListener(type, fn) { if (type === 'sourceopen') setImmediate(fn); }
  addSourceBuffer() { return { updating: false, appendBuffer() {}, addEventListener() {} }; }
  endOfStream() {}
};
global.WebSocket = class {
  static OPEN = 1;
  constructor(url) { this.url = url; this.readyState = 1; global.WebSocket.last = this; }
  close(code) { if (this.onclose) this.onclose({ code: code || 1000, reason: '' }); }
  send() {}
};

const MODELS = [
  { id: 'fake-chat', api_type: 'chat', state: 'running', origin: 'local' },
  { id: 'fake-embed', api_type: 'embedding', state: 'running', origin: 'local' },
  { id: 'speech-A', api_type: 'speech', state: 'running', origin: 'local' },
  { id: 'speech-B', api_type: 'speech', state: 'running', origin: 'local' },
  { id: 'transcribe-1', api_type: 'transcription', state: 'running', origin: 'local' },
  { id: 'video-1', api_type: 'video', state: 'running', origin: 'local', capabilities: ['duration', 'resolution', 'seed'] },
  { id: 'rt-1', api_type: 'realtime', state: 'running', origin: 'local' },
];
let models = MODELS.slice();
const VOICES = { 'speech-A': ['v1', 'v2'], 'speech-B': ['v3', 'v4'] };

const sse = (text) => {
  const chunks = [new TextEncoder().encode('data: ' + JSON.stringify({ choices: [{ delta: { content: text } }] }) + '\n\ndata: [DONE]\n')];
  return { getReader: () => ({ read: async () => (chunks.length ? { done: false, value: chunks.shift() } : { done: true }) }) };
};
global.fetch = async (url, opts = {}) => {
  const json = (d) => ({ ok: true, status: 200, headers: { get: () => null }, text: async () => JSON.stringify(d), json: async () => d });
  const raw = (s) => ({ ok: true, status: 200, headers: { get: () => null }, text: async () => s });
  if (url === '/_router/status') return json({ models, telemetry: { gpus: [] }, peers: {} });
  if (url.startsWith('/v1/audio/voices?')) {
    const id = decodeURIComponent((/model=([^&]+)/.exec(url) || [])[1] || '');
    return json({ data: (VOICES[id] || []).map((v) => ({ id: v })) });
  }
  if (url === '/v1/chat/completions') return { ok: true, status: 200, body: sse('Reply to ' + JSON.parse(opts.body).messages[0].content) };
  if (url === '/sdapi/v1/txt2img') return json({ images: [Buffer.from('png').toString('base64')] });
  if (url === '/v1/embeddings') return json({ data: [{ embedding: [0.1, 0.2] }] });
  if (url === '/v1/audio/speech') {
    let sent = false;
    return {
      ok: true, status: 200,
      headers: { get: (h) => (String(h).toLowerCase() === 'content-type' ? 'audio/mpeg' : null) },
      body: { getReader: () => ({ read: async () => (sent ? { done: true } : ((sent = true), { done: false, value: new Uint8Array([1, 2, 3]) })) }) },
    };
  }
  if (url === '/v1/audio/transcriptions') {
    return raw(opts.body.get('response_format') === 'srt' ? '1\n00:00:00,000 --> 00:00:01,000\nHello\n' : JSON.stringify({ text: 'Plain transcript' }));
  }
  if (url === '/v1/videos' && opts.method === 'POST') return json({ id: 'job-1', status: 'queued' });
  if (url === '/v1/videos/job-1') return json({ status: 'completed', progress: 100 });
  if (url === '/v1/videos/job-1/content') return { ok: true, status: 200, blob: async () => new Blob(['vid'], { type: 'video/mp4' }) };
  return json({ models: [], entries: [] });
};

(0, eval)(fs.readFileSync(process.argv[2], 'utf8'));

const $ = (id) => document.getElementById(id);
const ids = (tab) => [...$('hist-' + tab).innerHTML.matchAll(/data-open="(\d+)"/g)].map((m) => m[1]);
const click = (tab, attr, id) => $('hist-' + tab).onclick({ target: { closest: () => ({ dataset: { [attr]: id } }) } });
const settle = async (n = 5) => { for (let i = 0; i < n; i++) await new Promise((resolve) => setImmediate(resolve)); };

(async () => {
  await new Promise((resolve) => setImmediate(resolve));
  // The static selects live in index.html, which this harness does not load.
  $('speech-format').innerHTML = '<option value="mp3">MP3</option><option value="opus">Opus</option><option value="flac">FLAC</option>';
  $('transcribe-format').innerHTML = '<option value="json">JSON</option><option value="verbose_json">Verbose</option><option value="text">Text</option><option value="srt">SRT</option><option value="vtt">VTT</option>';
  $('transcribe-language').innerHTML = '<option value="">Backend default</option><option value="ja">Japanese</option>';
  $('video-resolution').innerHTML = '<option value="">Default</option><option value="640x480">640x480</option>';
  await window.refresh();
  const emptyList = $('hist-chat').innerHTML;

  // Two chats: newest first, and a click on the first restores its prompt and reply.
  $('chat-model').value = 'fake-chat';
  $('chat-prompt').value = 'first';
  await window.sendChat();
  $('chat-prompt').value = 'second';
  await window.sendChat();
  const [second, first] = ids('chat');
  assert.ok(Number(second) > Number(first), 'newest entry is listed first');
  assert.match($('hist-chat').innerHTML, /fake-chat/);
  assert.match($('hist-chat').innerHTML, /Done/);
  assert.match($('chat-output').innerHTML, /Reply to second/);
  click('chat', 'open', first);
  assert.strictEqual($('chat-prompt').value, 'first', 'a click restores the inputs');
  assert.match($('chat-output').innerHTML, /Reply to first/, 'a click restores the output');
  assert.strictEqual($('hist-note-chat').textContent, '', 'available inputs produce no note');

  // Reuse fills the inputs and leaves the output alone.
  click('chat', 'reuse', second);
  assert.strictEqual($('chat-prompt').value, 'second');
  assert.match($('chat-output').innerHTML, /Reply to first/, 'Reuse keeps the output on show');

  // Images become blob URLs; only the newest 20 media entries are kept.
  for (let i = 0; i < 22; i++) await window.generateImage();
  assert.strictEqual(ids('image').length, 20, 'media entries are capped at 20');
  assert.deepStrictEqual(revoked, ['blob:1', 'blob:2'], 'the two oldest images are revoked');
  assert.match($('image-output').innerHTML, /src="blob:22"/);

  // Clear revokes every image and resets the panel that showed one of them.
  window.clearHistory('image');
  assert.strictEqual(revoked.length, 22, 'Clear revokes every remaining blob URL');
  assert.strictEqual($('hist-image').innerHTML, emptyList, 'Clear shows the empty list again');
  assert.doesNotMatch($('image-output').innerHTML, /blob:/, 'Clear removes a revoked image from the result panel');

  // Text entries are capped at 50.
  $('embed-model').value = 'fake-embed';
  for (let i = 0; i < 52; i++) await window.sendEmbedding();
  assert.strictEqual(ids('embeddings').length, 50, 'entries are capped at 50 per tab');
  assert.strictEqual(ids('chat').length, 2, 'other tabs keep their own history');

  // Streamed speech stores the finished blob in history, and a restore reloads
  // the entry's model voices before putting its voice back.
  $('speech-model').value = 'speech-A';
  await window.loadVoices();
  $('speech-voice').value = 'v1';
  $('speech-input').value = 'hello A';
  $('speech-format').value = 'mp3';
  await window.sendSpeech();
  await settle();
  const [speechEntry] = ids('speech');
  assert.ok(speechEntry, 'streamed speech makes a history entry');
  $('speech-model').value = 'speech-B';
  await window.loadVoices();
  assert.notStrictEqual($('speech-voice').value, 'v1', 'switching model loses the old voice');
  click('speech', 'open', speechEntry);
  await settle();
  assert.strictEqual($('speech-voice').value, 'v1', 'the restore reloads the entry model voices and keeps its voice');
  assert.match($('speech-output').innerHTML, /Download audio/, 'the restore shows the finished blob, not the live stream');
  assert.strictEqual($('hist-note-speech').textContent, '');

  // A router that unloads an idle model still keeps the entry's voice on restore,
  // so Generate sends the same voice and the backend accepts or rejects it.
  models = MODELS.map((m) => (m.id === 'speech-A' ? { ...m, state: 'stopped' } : m));
  await window.refresh();
  click('speech', 'open', speechEntry);
  await settle();
  assert.strictEqual($('speech-voice').value, 'v1', 'a stopped model keeps the restored voice');
  assert.doesNotMatch($('hist-note-speech').textContent, /v1/, 'a stopped model does not call the voice unavailable');

  // A restore while a streamed speech request is running leaves that request
  // alone, so it keeps its player and download link and is recorded as Done.
  models = MODELS.slice();
  await window.refresh();
  $('speech-model').value = 'speech-A';
  $('speech-voice').value = 'v1';
  $('speech-input').value = 'second';
  const running = window.sendSpeech();
  await new Promise((resolve) => setImmediate(resolve));
  assert.match($('speech-output').innerHTML, /Playing as it arrives/, 'the stream is in flight');
  click('speech', 'open', speechEntry);
  assert.match($('speech-output').innerHTML, /Playing as it arrives/, 'a restore keeps the running stream on show');
  assert.strictEqual($('speech-input').value, 'hello A', 'a restore still fills the inputs');
  await running;
  await settle();
  assert.strictEqual(ids('speech').length, 2, 'the running request records its own finished entry');
  assert.doesNotMatch($('hist-speech').innerHTML, /Failed/, 'the running request is not recorded as failed');
  assert.match($('speech-dl').innerHTML, /Download audio/, 'the stream still gets its download link');

  // A model that has left the catalogue leaves the select blank, so the restore says so.
  models = MODELS.filter((m) => m.id !== 'speech-A');
  await window.refresh();
  click('speech', 'open', speechEntry);
  await settle();
  assert.match($('hist-note-speech').textContent, /Not available/);
  assert.match($('hist-note-speech').textContent, /speech-A/);

  // Transcription stores the subtitle result with the entry, so a restore re-enables Copy and Download.
  $('transcribe-model').value = 'transcribe-1';
  $('transcribe-file').files = [{ name: 'clip.mp3' }];
  $('transcribe-format').value = 'srt';
  await window.sendTranscription();
  assert.strictEqual($('transcribe-actions').hidden, false);
  assert.strictEqual($('transcribe-copy').textContent, 'Copy SRT');
  $('transcribe-format').value = 'json';
  await window.sendTranscription();
  assert.strictEqual($('transcribe-actions').hidden, true);
  const subtitleEntry = ids('transcription')[1];
  click('transcription', 'open', subtitleEntry);
  assert.strictEqual($('transcribe-actions').hidden, false, 'the srt entry restores its subtitle actions');
  assert.strictEqual($('transcribe-copy').textContent, 'Copy SRT');

  // A finished video job records its completed playback in history.
  $('video-model').value = 'video-1';
  $('video-prompt').value = 'A lighthouse';
  await window.sendVideo();
  await settle(8);
  assert.strictEqual(ids('video').length, 1, 'the video job makes one history entry');
  assert.match($('hist-video').innerHTML, /Done/);
  assert.match($('video-output').innerHTML, /Download video/);

  // Closing a realtime session records the close in history.
  $('rt-model').value = 'rt-1';
  window.openRealtime();
  global.WebSocket.last.onopen();
  window.closeRealtime();
  assert.strictEqual(ids('realtime').length, 1, 'the realtime close makes one history entry');
  assert.match($('hist-realtime').innerHTML, /Done/);
  assert.strictEqual($('rt-status').textContent, 'Closed');
})().catch((err) => { console.error(err); process.exit(1); });
