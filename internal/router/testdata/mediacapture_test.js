// Checks the pure helpers in ui/mediacapture.js: WAV encoding, downmix and
// resample, frame picking, chat content parts and one-off clone fields.
// Usage: node mediacapture_test.js <mediacapture.js> [wav-out]
// With wav-out, writes a 1 s 16 kHz tone so the Go side can parse it.
const assert = require('assert');
const fs = require('fs');
const c = require(require('path').resolve(process.argv[2]));

// encodeWAV: 44-byte RIFF header, mono 16-bit PCM at the given rate.
{
  const w = c.encodeWAV(Float32Array.from([0, 1, -1, 2, -2, 0.5]), 16000);
  const v = new DataView(w.buffer);
  const str = (o, n) => String.fromCharCode(...w.subarray(o, o + n));
  assert.strictEqual(w.length, 44 + 6 * 2);
  assert.strictEqual(str(0, 4), 'RIFF');
  assert.strictEqual(v.getUint32(4, true), 36 + 12);
  assert.strictEqual(str(8, 8), 'WAVEfmt ');
  assert.strictEqual(v.getUint16(20, true), 1, 'PCM');
  assert.strictEqual(v.getUint16(22, true), 1, 'mono');
  assert.strictEqual(v.getUint32(24, true), 16000, 'sample rate');
  assert.strictEqual(v.getUint32(28, true), 32000, 'byte rate');
  assert.strictEqual(v.getUint16(32, true), 2, 'block align');
  assert.strictEqual(v.getUint16(34, true), 16, 'bits');
  assert.strictEqual(str(36, 4), 'data');
  assert.strictEqual(v.getUint32(40, true), 12);
  const s = i => v.getInt16(44 + i * 2, true);
  assert.deepStrictEqual([0, 1, 2, 3, 4].map(s), [0, 32767, -32768, 32767, -32768], 'clips to full scale');
  assert.strictEqual(s(5), 16384);
}

// downmix averages channels; one channel passes through.
assert.deepStrictEqual(Array.from(c.downmix([Float32Array.from([1, 0.5]), Float32Array.from([0, -0.5])])), [0.5, 0]);
assert.deepStrictEqual(Array.from(c.downmix([Float32Array.from([0.25])])), [0.25]);
assert.strictEqual(c.downmix([]).length, 0);

// resampleLinear: 48 kHz to 16 kHz keeps a third of the samples and a ramp stays a ramp.
{
  const ramp = Float32Array.from({ length: 48 }, (_, i) => i / 48);
  const out = c.resampleLinear(ramp, 48000, 16000);
  assert.strictEqual(out.length, 16);
  for (let i = 0; i < out.length; i++) assert.ok(Math.abs(out[i] - (i * 3) / 48) < 1e-6);
  assert.strictEqual(c.resampleLinear(ramp, 16000, 16000).length, 48);
  assert.strictEqual(c.resampleLinear(Float32Array.from([0, 1]), 8000, 16000).length, 4);
}

// pickEven keeps the first and last frame and never repeats one.
assert.deepStrictEqual(c.pickEven(10, 3), [0, 5, 9]);
assert.deepStrictEqual(c.pickEven(21, 6), [0, 4, 8, 12, 16, 20]);
assert.deepStrictEqual(c.pickEven(3, 6), [0, 1, 2]);
assert.deepStrictEqual(c.pickEven(5, 1), [2]);
assert.deepStrictEqual(c.pickEven(0, 4), []);

// fitSize caps the longest side and never scales up.
assert.deepStrictEqual(c.fitSize(1920, 1080, 1280), { w: 1280, h: 720 });
assert.deepStrictEqual(c.fitSize(720, 1280, 640), { w: 360, h: 640 });
assert.deepStrictEqual(c.fitSize(640, 480, 1280), { w: 640, h: 480 });

// chatPlan follows the capability flags.
assert.deepStrictEqual(c.chatPlan([]), { camera: false, still: false, clip: null, audio: false });
assert.deepStrictEqual(c.chatPlan(['vision']), { camera: true, still: true, clip: 'frames', audio: false });
assert.deepStrictEqual(c.chatPlan(['vision', 'video_in', 'audio_in']), { camera: true, still: true, clip: 'native', audio: true });

// chatContent: plain string with nothing attached, parts otherwise.
{
  const img = { kind: 'image', url: 'data:image/jpeg;base64,AAA' };
  const frames = Array.from({ length: 21 }, (_, i) => 'data:image/jpeg;base64,F' + i);
  const clip = { kind: 'clip', url: 'data:video/webm;base64,VVV', frames };
  const audio = { kind: 'audio', b64: 'UklGRg==', format: 'wav' };
  assert.strictEqual(c.chatContent('hi', [], ['vision']), 'hi');
  assert.strictEqual(c.chatContent('hi', [img, clip, audio], []), 'hi', 'a text-only model gets the plain prompt');
  assert.deepStrictEqual(c.chatContent('hi', [img], ['vision']), [
    { type: 'text', text: 'hi' },
    { type: 'image_url', image_url: { url: img.url } },
  ]);
  const sampled = c.chatContent('what moves?', [clip], ['vision'], 6);
  assert.strictEqual(sampled.length, 7);
  assert.deepStrictEqual(sampled.slice(1).map(p => p.image_url.url), [0, 4, 8, 12, 16, 20].map(i => frames[i]));
  assert.deepStrictEqual(c.chatContent('what moves?', [clip], ['vision', 'video_in']), [
    { type: 'text', text: 'what moves?' },
    { type: 'video_url', video_url: { url: clip.url } },
  ]);
  assert.deepStrictEqual(c.chatContent('listen', [audio], ['audio_in']), [
    { type: 'text', text: 'listen' },
    { type: 'input_audio', input_audio: { data: 'UklGRg==', format: 'wav' } },
  ]);
  // A clip on a video_in model without vision has no frames fallback to need.
  assert.strictEqual(c.chatContent('x', [img], ['video_in']), 'x');
}

// cloneFields uses the stanza's clone_mode, ref_audio by default.
assert.deepStrictEqual(c.cloneFields('ref_audio', 'QUJD', 'hello there', 'en'), {
  ref_audio: 'data:audio/wav;base64,QUJD', ref_text: 'hello there', language: 'en',
});
assert.deepStrictEqual(c.cloneFields(undefined, 'QUJD', '', ''), { ref_audio: 'data:audio/wav;base64,QUJD' });
assert.deepStrictEqual(c.cloneFields('speaker_ref_b64', 'QUJD', 'ignored', 'en'), { speaker_ref_b64: 'QUJD', lang: 'en' });

// mediaError names the fix; canCapture needs a secure context.
assert.match(c.mediaError({ name: 'NotAllowedError' }, 'microphone'), /refused.*site settings/);
assert.match(c.mediaError({ name: 'NotFoundError' }, 'camera'), /No camera was found/);
assert.match(c.mediaError({ name: 'NotReadableError' }, 'camera'), /busy/);
assert.match(c.mediaError({ name: 'SecurityError' }), /HTTPS or localhost/);
assert.strictEqual(c.mediaError({ message: 'boom' }), 'boom');
assert.strictEqual(c.canCapture({ mediaDevices: { getUserMedia() {} } }, true), true);
assert.strictEqual(c.canCapture({ mediaDevices: { getUserMedia() {} } }, false), false);
assert.strictEqual(c.canCapture({}, true), false);

assert.strictEqual(c.fmtClock(75.9), '1:15');
assert.strictEqual(c.bytesToBase64(Uint8Array.from([82, 73, 70, 70])), 'UklGRg==');

if (process.argv[3]) {
  const tone = Float32Array.from({ length: 16000 }, (_, i) => 0.3 * Math.sin((2 * Math.PI * 440 * i) / 16000));
  fs.writeFileSync(process.argv[3], c.encodeWAV(tone, 16000));
}
console.log('capture helpers ok');
