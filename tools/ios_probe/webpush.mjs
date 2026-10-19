// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0
// iPhone の実機の測り（試験設計書 T-L6-09）で、開発機から購読へ 1 通の Web Push を送る。
// RFC 8291（aes128gcm の暗号化）と RFC 8292（VAPID）を Node の組み込みの暗号（node:crypto）と fetch だけで行い、部品を足さない。
// 何をいつ送るかは tools/ios_probe の push の副命令が決め、本ファイルは 1 通の暗号化と送信だけを持つ。
//
//   node webpush.mjs keys                VAPID の鍵対を作り JSON を標準出力へ（{public, jwk}）
//   node webpush.mjs send < req.json     1 通を送る。req は {subscription, payload, ttl, urgency, topic?, vapid:{public, jwk}, subject}
//   node webpush.mjs self-test           RFC 8291 の付録 A の例で暗号化を確かめる
import { pathToFileURL } from 'node:url';
import { createECDH, createHmac, createCipheriv, createPrivateKey, randomBytes, sign } from 'node:crypto';

const b64u = (buf) => Buffer.from(buf).toString('base64url');
const unb64u = (s) => Buffer.from(s.replace(/\s+/g, ''), 'base64url');
const hmac = (key, data) => createHmac('sha256', key).update(data).digest();
const RECORD_SIZE = 4096;

// RFC 8291 3.4 と 4。salt と送り手の鍵を渡せるのは self-test のためだけ
export function encrypt(plaintext, uaPublic, authSecret, salt = randomBytes(16), sender = null) {
  const ecdh = createECDH('prime256v1');
  if (sender) ecdh.setPrivateKey(sender); else ecdh.generateKeys();
  const asPublic = ecdh.getPublicKey();
  const ecdhSecret = ecdh.computeSecret(uaPublic);
  const prkKey = hmac(authSecret, ecdhSecret);
  const keyInfo = Buffer.concat([Buffer.from('WebPush: info\0'), uaPublic, asPublic]);
  const ikm = hmac(prkKey, Buffer.concat([keyInfo, Buffer.from([1])]));
  const prk = hmac(salt, ikm);
  const cek = hmac(prk, Buffer.from('Content-Encoding: aes128gcm\0\x01')).subarray(0, 16);
  const nonce = hmac(prk, Buffer.from('Content-Encoding: nonce\0\x01')).subarray(0, 12);
  const record = Buffer.concat([Buffer.from(plaintext), Buffer.from([2])]); // 最後の記録の区切り。詰め物なし
  if (record.length + 16 > RECORD_SIZE) throw new Error('中身が 1 つの記録に収まらない');
  const cipher = createCipheriv('aes-128-gcm', cek, nonce);
  const body = Buffer.concat([cipher.update(record), cipher.final(), cipher.getAuthTag()]);
  const header = Buffer.alloc(21);
  salt.copy(header, 0);
  header.writeUInt32BE(RECORD_SIZE, 16);
  header.writeUInt8(asPublic.length, 20);
  return { body: Buffer.concat([header, asPublic, body]), intermediate: { ecdhSecret, prkKey, ikm, prk, cek, nonce } };
}

// RFC 8292 2 と 3。署名は ES256（r と s の 64 バイト。RFC 7518 3.4）
export function vapidHeader(endpoint, vapid, subject, now = Math.floor(Date.now() / 1000)) {
  const head = b64u(JSON.stringify({ typ: 'JWT', alg: 'ES256' }));
  const claims = b64u(JSON.stringify({ aud: new URL(endpoint).origin, exp: now + 12 * 3600, sub: subject }));
  const key = createPrivateKey({ key: vapid.jwk, format: 'jwk' });
  const sig = sign('sha256', Buffer.from(`${head}.${claims}`), { key, dsaEncoding: 'ieee-p1363' });
  return `vapid t=${head}.${claims}.${b64u(sig)}, k=${vapid.public}`;
}

function keys() {
  const ecdh = createECDH('prime256v1');
  ecdh.generateKeys();
  const pub = ecdh.getPublicKey();
  const jwk = { kty: 'EC', crv: 'P-256', x: b64u(pub.subarray(1, 33)), y: b64u(pub.subarray(33, 65)), d: b64u(ecdh.getPrivateKey()) };
  return { public: b64u(pub), jwk };
}

async function send(req) {
  const sub = req.subscription;
  const { body } = encrypt(Buffer.from(req.payload, 'utf8'), unb64u(sub.keys.p256dh), unb64u(sub.keys.auth));
  const headers = {
    'Content-Encoding': 'aes128gcm',
    'Content-Type': 'application/octet-stream',
    TTL: String(req.ttl),
    Urgency: req.urgency,
    Authorization: vapidHeader(sub.endpoint, req.vapid, req.subject),
  };
  if (req.topic) headers.Topic = req.topic;
  const res = await fetch(sub.endpoint, { method: 'POST', headers, body });
  return { status: res.status, body: (await res.text()).slice(0, 500) };
}

function selfTest() {
  // RFC 8291 の 5 節と付録 A
  const v = {
    plaintext: 'V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24',
    asPrivate: 'yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw',
    uaPublic: 'BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4',
    salt: 'DGv6ra1nlYgDCS1FRnbzlw',
    auth: 'BTBZMqHH6r4Tts7J_aSIgg',
    ecdhSecret: 'kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs',
    prkKey: 'Snr3JMxaHVDXHWJn5wdC52WjpCtd2EIEGBykDcZW32k',
    ikm: 'S4lYMb_L0FxCeq0WhDx813KgSYqU26kOyzWUdsXYyrg',
    prk: '09_eUZGrsvxChDCGRCdkLiDXrReGOEVeSCdCcPBSJSc',
    cek: 'oIhVW04MRdy2XN9CiKLxTg',
    nonce: '4h_95klXJ5E_qnoN',
    body: 'DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN',
  };
  const out = encrypt(unb64u(v.plaintext), unb64u(v.uaPublic), unb64u(v.auth), unb64u(v.salt), unb64u(v.asPrivate));
  const got = { ...Object.fromEntries(Object.entries(out.intermediate).map(([k, x]) => [k, b64u(x)])), body: b64u(out.body) };
  const bad = Object.keys(got).filter((k) => got[k] !== v[k]);
  return bad.length ? { ok: false, mismatch: bad } : { ok: true };
}

async function main() {
  const cmd = process.argv[2];
  if (cmd === 'keys') { process.stdout.write(JSON.stringify(keys()) + '\n'); return 0; }
  if (cmd === 'self-test') { const r = selfTest(); process.stdout.write(JSON.stringify(r) + '\n'); return r.ok ? 0 : 1; }
  if (cmd === 'send') {
    const chunks = [];
    for await (const c of process.stdin) chunks.push(c);
    const r = await send(JSON.parse(Buffer.concat(chunks).toString('utf8')));
    process.stdout.write(JSON.stringify(r) + '\n');
    return r.status >= 200 && r.status < 300 ? 0 : 1;
  }
  process.stderr.write('使い方：node webpush.mjs keys | send < req.json | self-test\n');
  return 2;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) process.exitCode = await main();
