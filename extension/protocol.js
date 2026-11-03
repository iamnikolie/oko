// Mutual authentication with the oko bridge (mirror of internal/bridge/crypto.go).
// Pairing: ephemeral P-256 ECDH, each side proves the one-time code with an
// HMAC keyed by PBKDF2(code) over the transcript; the bridge proves first. The
// link key is HMAC(ECDH secret, transcript) and never crosses the socket.
// Every connection: HMAC challenge-response with the link key, bridge first,
// then every frame both ways is authenticated (channel below).
const enc = new TextEncoder();

export const ORIGIN = chrome.runtime.getURL('').replace(/\/$/, '');

export const hex = (buf) => [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, '0')).join('');
export const unhex = (s) => new Uint8Array((s.match(/../g) || []).map((h) => parseInt(h, 16)));
export const randomHex = (n) => hex(crypto.getRandomValues(new Uint8Array(n)));
export const validNonce = (s) => typeof s === 'string' && /^[0-9a-f]{32}$/.test(s);
export const normCode = (s) => String(s || '').toUpperCase().replace(/[-\t ]/g, '');

export async function hmac(keyBytes, msg) {
  const k = await crypto.subtle.importKey('raw', keyBytes, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  return new Uint8Array(await crypto.subtle.sign('HMAC', k, enc.encode(msg)));
}

export const hmacHex = async (key, msg) => hex(await hmac(key, msg));

// same compares two hex proofs without an early exit.
export function same(a, b) {
  if (typeof a !== 'string' || typeof b !== 'string' || a.length !== b.length) return false;
  let d = 0;
  for (let i = 0; i < a.length; i++) d |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return d === 0;
}

export async function codeKey(code, salt) {
  const k = await crypto.subtle.importKey('raw', enc.encode(normCode(code)), 'PBKDF2', false, ['deriveBits']);
  return new Uint8Array(await crypto.subtle.deriveBits(
    { name: 'PBKDF2', salt: enc.encode(salt), iterations: 100000, hash: 'SHA-256' }, k, 256));
}

export const pairTranscript = (pubExt, pubBridge, nonceExt, nonceBridge) =>
  ['oko-pair-v2', ORIGIN, pubExt, pubBridge, nonceExt, nonceBridge].join('|');
export const helloBridgeMsg = (nonceExt, nonceBridge) => `oko-bridge|${ORIGIN}|${nonceExt}|${nonceBridge}`;
export const helloExtMsg = (nonceExt, nonceBridge) => `oko-ext|${ORIGIN}|${nonceExt}|${nonceBridge}`;

export async function ecdhKeys() {
  const kp = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, false, ['deriveBits']);
  return { kp, pub: hex(await crypto.subtle.exportKey('raw', kp.publicKey)) };
}

export async function ecdhShared(kp, pubHex) {
  const peer = await crypto.subtle.importKey('raw', unhex(pubHex), { name: 'ECDH', namedCurve: 'P-256' }, false, []);
  return new Uint8Array(await crypto.subtle.deriveBits({ name: 'ECDH', public: peer }, kp.privateKey, 256));
}

// channel authenticates every frame after the handshake (mirror of
// internal/bridge/channel.go): one HMAC key per direction from the link key
// and this connection's nonces; a frame is "<n> <mac> <body>", n counts that
// direction from 1 with no gaps, mac = HMAC(key, n + "|" + body). open throws
// on a frame that does not check out or comes out of order; the caller then
// drops the session. side is 'ext' (the worker) or 'bridge' (tests only).
export async function channel(linkKey, nonceExt, nonceBridge, side = 'ext') {
  const dirKey = async (dir) => crypto.subtle.importKey('raw',
    await hmac(linkKey, ['oko-frames', dir, ORIGIN, nonceExt, nonceBridge].join('|')),
    { name: 'HMAC', hash: 'SHA-256' }, false, ['sign', 'verify']);
  const out = await dirKey(side === 'bridge' ? 'bridge' : 'ext');
  const inn = await dirKey(side === 'bridge' ? 'ext' : 'bridge');
  let sent = 0;
  let recvd = 0;
  let broken = false; // one bad frame ends the channel
  return {
    // seal numbers the frame now, so frames must be written in call order.
    async seal(body) {
      const n = String(++sent);
      return `${n} ${hex(await crypto.subtle.sign('HMAC', out, enc.encode(n + '|' + body)))} ${body}`;
    },
    async open(frame) {
      const fail = () => { broken = true; return new Error('a message failed authentication'); };
      if (broken || typeof frame !== 'string') throw fail();
      const a = frame.indexOf(' ');
      const b = a < 0 ? -1 : frame.indexOf(' ', a + 1);
      if (b < 0) throw fail();
      const n = frame.slice(0, a);
      const mac = frame.slice(a + 1, b);
      const body = frame.slice(b + 1);
      if (n !== String(recvd + 1) || !/^[0-9a-f]{64}$/.test(mac)) throw fail();
      recvd++; // claim the number before awaiting
      if (!await crypto.subtle.verify('HMAC', inn, unhex(mac), enc.encode(n + '|' + body))) throw fail();
      return body;
    },
  };
}
