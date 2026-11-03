// Mutual authentication with the oko bridge (mirror of internal/bridge/crypto.go).
// Pairing: ephemeral P-256 ECDH, each side proves the one-time code with an
// HMAC keyed by PBKDF2(code) over the transcript; the bridge proves first. The
// link key is HMAC(ECDH secret, transcript) and never crosses the socket.
// Every connection: HMAC challenge-response with the link key, bridge first.
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
