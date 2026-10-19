// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0
// iPhone の Safari の確かめのページ（基本設計書 第14章 11.6、試験設計書 T-L3-12、T-L6-09）。
// OS の挙動（WebCrypto の原始的な操作、IndexedDB の取り出せない鍵、WebAuthn、Web Push、Service Worker）だけを見る。
// 本システムの鍵の形（第12章 5.3、5.4 のベクトル）は書かない。web-crypto の手順 9 で本物の鍵の操作のバンドルに置き換える。
// 結果は画面と window.__ft_probe（tools/ios_probe が WebDriver で読む）に出す。各項目は result が ok、ng、not_run のどれか。
'use strict';
(() => {
  const params = new URLSearchParams(location.search);
  const PK_TIMEOUT = Number(params.get('pk_timeout')) || 60000;
  const P = {
    format: 'ft.probe/1',
    page_rev: '2026-10-17',
    started_at: new Date().toISOString(),
    env: {},
    items: {},
    device: { a: [], c: [], f: [], g: [], notes: {} },
    sw_log: [],
    subscription: null,
    done: false,
    passkey_done: false,
  };
  window.__ft_probe = P;

  const $ = (id) => document.getElementById(id);
  const sc = crypto.subtle;
  const enc = new TextEncoder();
  const hex = (s) => Uint8Array.from(s.match(/../g), (b) => parseInt(b, 16));
  const toHex = (buf) => Array.from(new Uint8Array(buf), (b) => b.toString(16).padStart(2, '0')).join('');
  const b64u = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  const unb64u = (s) => Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0));
  const errText = (e) => (e && e.name ? `${e.name}: ${e.message}` : String(e));
  const sha8 = async (buf) => toHex((await sc.digest('SHA-256', buf)).slice(0, 8));

  // ---------- 表示と記録 ----------
  function show(id, item) {
    let tr = document.querySelector(`tr[data-id="${id}"]`);
    if (!tr) {
      const table = $(`t-${id.split('-').slice(0, 2).join('-')}`) || $('t-other');
      tr = document.createElement('tr');
      tr.dataset.id = id;
      tr.append(document.createElement('td'), document.createElement('td'), document.createElement('td'));
      table.append(tr);
    }
    const [c1, c2, c3] = tr.children;
    c1.textContent = id;
    c2.textContent = item.result;
    c2.className = item.result;
    c3.textContent = typeof item.detail === 'string' ? item.detail : JSON.stringify(item.detail);
  }
  function record(id, result, detail, ms) {
    const item = { result, detail: detail === undefined ? '' : detail };
    if (ms !== undefined) item.ms = ms;
    P.items[id] = item;
    show(id, item);
    return item;
  }
  // fn が真を返せば ok、偽なら ng、投げたら ng（理由つき）。detail を返したい時は {ok, detail} を返す
  async function step(id, fn) {
    const t0 = performance.now();
    try {
      const r = await fn();
      const ok = typeof r === 'object' && r !== null && 'ok' in r ? r.ok : r;
      const detail = typeof r === 'object' && r !== null && 'detail' in r ? r.detail : '';
      return record(id, ok ? 'ok' : 'ng', detail, Math.round(performance.now() - t0));
    } catch (e) {
      return record(id, 'ng', errText(e), Math.round(performance.now() - t0));
    }
  }

  // ---------- 0. 環境 ----------
  function displayMode() {
    for (const m of ['fullscreen', 'standalone', 'minimal-ui', 'browser']) if (matchMedia(`(display-mode: ${m})`).matches) return m;
    return 'unknown';
  }
  function env() {
    const m = navigator.userAgent.match(/OS (\d+)_(\d+)(?:_(\d+))?/);
    Object.assign(P.env, {
      user_agent: navigator.userAgent,
      ios_version: m ? `${m[1]}.${m[2]}${m[3] ? '.' + m[3] : ''}` : null,
      display_mode: displayMode(),
      navigator_standalone: navigator.standalone === true,
      secure_context: isSecureContext,
      origin: location.origin,
      push_manager: typeof PushManager !== 'undefined',
      window_push_manager: !!navigator.pushManager,
      notification: typeof Notification !== 'undefined' ? Notification.permission : 'absent',
      set_app_badge: typeof navigator.setAppBadge === 'function',
      service_worker: 'serviceWorker' in navigator,
      public_key_credential: typeof PublicKeyCredential !== 'undefined',
      rtc_rtp_script_transform: typeof RTCRtpScriptTransform !== 'undefined',
    });
    const t = $('t-env');
    for (const [k, v] of Object.entries(P.env)) {
      const tr = document.createElement('tr');
      const a = document.createElement('td');
      const b = document.createElement('td');
      a.textContent = k;
      b.textContent = String(v);
      tr.append(a, b);
      t.append(tr);
    }
  }

  // ---------- T-L3-12 (1) WebCrypto ----------
  // 既知の答えは公開の規格の値。RFC 6979 A.2.5（P-256、SHA-256、"sample"）、RFC 5869 A.1、GCM の仕様の試験の場合 3 と 15、P-256 の基点 G と 3G
  const V = {
    rfc6979Pub: '0460fed4ba255a9d31c961eb74c6356d68c049b8923b61fa6ce669622e60f29fb67903fe1008b8bc99a41ae9e95628bc64f2f1b20c2d7e9f5177a3c294d4462299',
    rfc6979Sig: 'efd48b2aacb6a8fd1140dd9cd45e81d69d2c877b56aaf991c34d0ea84eaf3716f7cb1c942d657c41d436c7a1b6e29f65f3e900dbb9aff4064dc4ab2f843acda8',
    hkdfOkm: '3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865',
    gcmKey: 'feffe9928665731c6d6a8f9467308308',
    gcmIv: 'cafebabefacedbaddecaf888',
    gcmPlain: 'd9313225f88406e5a55909c5aff5269a86a7a9531534f7da2e4c303d8a318a721c3c0c95956809532fcf0e2449a6b525b16aedf5aa0de657ba637b391aafd255',
    gcm128: '42831ec2217774244b7221b784d0d49ce3aa212f2c02a4e035c17e2329aca12e21d514b25466931c7d8f6a5aac84aa051ba30b396a0aac973d58e091473f59854d5c2af327cd64a62cf35abd2ba6fab4',
    gcm256: '522dc1f099567d07f47f37a32a84427d643a8cdcbfe5c0c97598a2bd2555d1aa8cb08e48590dbb3da7b08b1056828838c5f61e6393ba7a0abcc9f662898015adb094dac5d93471bdec1a502270e3cc6c',
    G: { x: 'axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY', y: 'T-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU' },
    G3: { x: 'Xsvk0aYzCkTI9--VHUvxZebGtyHvramF-0FmG8bn_Ww', y: 'hzRkDEmY_343SwbOGmSi7NgqsDY4T7g9mnmxJ6J9UDI' },
    gCompressed: '036b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c296',
  };
  const ECDSA = { name: 'ECDSA', namedCurve: 'P-256' };
  const ECDH = { name: 'ECDH', namedCurve: 'P-256' };
  const SIGN = { name: 'ECDSA', hash: 'SHA-256' };

  async function cryptoChecks() {
    let sigPair, kemA, kemB;
    await step('L3-1-ecdsa-generate', async () => {
      sigPair = await sc.generateKey(ECDSA, false, ['sign', 'verify']);
      return { ok: sigPair.privateKey.extractable === false, detail: `extractable=${sigPair.privateKey.extractable}` };
    });
    await step('L3-1-ecdsa-sign-verify', async () => {
      const m = enc.encode('ft.probe/sign');
      const sig = await sc.sign(SIGN, sigPair.privateKey, m);
      const good = await sc.verify(SIGN, sigPair.publicKey, sig, m);
      const bad = await sc.verify(SIGN, sigPair.publicKey, sig, enc.encode('ft.probe/sigm'));
      return { ok: sig.byteLength === 64 && good && !bad, detail: `${sig.byteLength} bytes, good=${good}, tampered=${bad}` };
    });
    await step('L3-1-ecdsa-known-answer', async () => {
      const pub = await sc.importKey('raw', hex(V.rfc6979Pub), ECDSA, false, ['verify']);
      const good = await sc.verify(SIGN, pub, hex(V.rfc6979Sig), enc.encode('sample'));
      const bad = await sc.verify(SIGN, pub, hex(V.rfc6979Sig), enc.encode('samplf'));
      return { ok: good && !bad, detail: `RFC 6979 A.2.5 good=${good}, tampered=${bad}` };
    });
    await step('L3-1-ecdh-generate', async () => {
      kemA = await sc.generateKey(ECDH, false, ['deriveBits']);
      kemB = await sc.generateKey(ECDH, false, ['deriveBits']);
      return { ok: kemA.privateKey.extractable === false, detail: `extractable=${kemA.privateKey.extractable}` };
    });
    await step('L3-1-ecdh-derive', async () => {
      const s1 = await sc.deriveBits({ name: 'ECDH', public: kemB.publicKey }, kemA.privateKey, 256);
      const s2 = await sc.deriveBits({ name: 'ECDH', public: kemA.publicKey }, kemB.privateKey, 256);
      return { ok: s1.byteLength === 32 && toHex(s1) === toHex(s2), detail: `${s1.byteLength} bytes` };
    });
    await step('L3-1-ecdh-known-answer', async () => {
      const d = new Uint8Array(32);
      d[31] = 3;
      const priv = await sc.importKey('jwk', { kty: 'EC', crv: 'P-256', ...V.G3, d: b64u(d) }, ECDH, false, ['deriveBits']);
      const g = await sc.importKey('jwk', { kty: 'EC', crv: 'P-256', ...V.G }, ECDH, true, []);
      const s = await sc.deriveBits({ name: 'ECDH', public: g }, priv, 256);
      return { ok: b64u(s) === V.G3.x, detail: '3 x G の x 座標' };
    });
    await step('L3-1-hkdf-sha256', async () => {
      const k = await sc.importKey('raw', new Uint8Array(22).fill(0x0b), 'HKDF', false, ['deriveBits']);
      const okm = await sc.deriveBits({ name: 'HKDF', hash: 'SHA-256', salt: hex('000102030405060708090a0b0c'), info: hex('f0f1f2f3f4f5f6f7f8f9') }, k, 336);
      return { ok: toHex(okm) === V.hkdfOkm, detail: 'RFC 5869 A.1' };
    });
    for (const [bits, keyHex, want] of [[128, V.gcmKey, V.gcm128], [256, V.gcmKey + V.gcmKey, V.gcm256]]) {
      await step(`L3-1-aes${bits}gcm`, async () => {
        const k = await sc.importKey('raw', hex(keyHex), 'AES-GCM', false, ['encrypt', 'decrypt']);
        const iv = hex(V.gcmIv);
        const c = await sc.encrypt({ name: 'AES-GCM', iv }, k, hex(V.gcmPlain));
        const p = await sc.decrypt({ name: 'AES-GCM', iv }, k, c);
        const t = new Uint8Array(c).slice(); // 写しを書き換える（c の比べに響かせない）
        t[0] ^= 1;
        let tamperedRejected = false;
        try { await sc.decrypt({ name: 'AES-GCM', iv }, k, t); } catch { tamperedRejected = true; }
        return { ok: toHex(c) === want && toHex(p) === V.gcmPlain && tamperedRejected, detail: `GCM の試験の場合 ${bits === 128 ? 3 : 15}、改竄の拒否=${tamperedRejected}` };
      });
    }
    await step('L3-1-aes256gcm-generate', async () => {
      const k = await sc.generateKey({ name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
      const iv = crypto.getRandomValues(new Uint8Array(12));
      const p = await sc.decrypt({ name: 'AES-GCM', iv }, k, await sc.encrypt({ name: 'AES-GCM', iv }, k, enc.encode('ft.probe')));
      return { ok: new TextDecoder().decode(p) === 'ft.probe' && k.extractable === false, detail: `extractable=${k.extractable}` };
    });
    let pubG;
    await step('L3-1-import-jwk', async () => {
      pubG = await sc.importKey('jwk', { kty: 'EC', crv: 'P-256', ...V.G }, ECDSA, true, ['verify']);
      return true;
    });
    await step('L3-1-export-raw', async () => {
      const raw = new Uint8Array(await sc.exportKey('raw', pubG));
      return { ok: raw.length === 65 && raw[0] === 4 && b64u(raw.slice(1, 33)) === V.G.x, detail: `${raw.length} bytes, 先頭 ${raw[0]}` };
    });
    await step('L3-1-export-spki', async () => {
      const spki = new Uint8Array(await sc.exportKey('spki', pubG));
      return { ok: spki.length === 91 && spki[26] === 4 && b64u(spki.slice(27, 59)) === V.G.x, detail: `${spki.length} bytes` };
    });
    await step('L3-1-export-jwk', async () => {
      const j = await sc.exportKey('jwk', sigPair.publicKey);
      return { ok: j.crv === 'P-256' && j.x.length === 43 && j.y.length === 43 && !('d' in j), detail: `crv=${j.crv}` };
    });
    await step('L3-1-import-raw-spki', async () => {
      const raw = await sc.exportKey('raw', sigPair.publicKey);
      const spki = await sc.exportKey('spki', sigPair.publicKey);
      const a = await sc.importKey('raw', raw, ECDSA, true, ['verify']);
      const b = await sc.importKey('spki', spki, ECDSA, true, ['verify']);
      const m = enc.encode('ft.probe/import');
      const sig = await sc.sign(SIGN, sigPair.privateKey, m);
      return (await sc.verify(SIGN, a, sig, m)) && (await sc.verify(SIGN, b, sig, m));
    });
    await step('L3-1-import-raw-compressed-rejected', async () => {
      try {
        await sc.importKey('raw', hex(V.gCompressed), ECDSA, true, ['verify']);
        return { ok: false, detail: '圧縮の点の raw を受けた' };
      } catch (e) {
        return { ok: true, detail: `拒んだ（${e.name}）` };
      }
    });
  }

  // ---------- IndexedDB ----------
  function idbOpen(name, stores) {
    return new Promise((res, rej) => {
      const q = indexedDB.open(name, 1);
      q.onupgradeneeded = () => { for (const s of stores) q.result.createObjectStore(s); };
      q.onsuccess = () => res(q.result);
      q.onerror = () => rej(q.error);
    });
  }
  function idbReq(db, store, mode, fn) {
    return new Promise((res, rej) => {
      const tx = db.transaction(store, mode);
      const r = fn(tx.objectStore(store));
      let value;
      if (r) r.onsuccess = () => { value = r.result; };
      tx.oncomplete = () => res(value);
      tx.onerror = () => rej(tx.error);
      tx.onabort = () => rej(tx.error);
    });
  }
  const idbGet = (db, store, key) => idbReq(db, store, 'readonly', (s) => s.get(key));
  const idbPut = (db, store, key, v) => idbReq(db, store, 'readwrite', (s) => s.put(v, key));

  // 取り出せない 3 つの鍵（署名、鍵合意、AES）と、AES で包んだ鍵 1 つと暗号文 1 つを作る
  async function makeKeySet() {
    const sig = await sc.generateKey(ECDSA, false, ['sign', 'verify']);
    const kem = await sc.generateKey(ECDH, false, ['deriveBits']);
    const aes = await sc.generateKey({ name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt', 'wrapKey', 'unwrapKey']);
    const inner = await sc.generateKey({ name: 'AES-GCM', length: 256 }, true, ['encrypt', 'decrypt']);
    const wrapIv = crypto.getRandomValues(new Uint8Array(12));
    const wrapped = await sc.wrapKey('raw', inner, aes, { name: 'AES-GCM', iv: wrapIv });
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const ct = await sc.encrypt({ name: 'AES-GCM', iv }, aes, enc.encode('本文を復号できた'));
    return { sig, kem, aes, wrapped, wrapIv, iv, ct, created_at: Date.now() };
  }
  // 読み戻した鍵が使えるかを確かめる。失敗した手順の名を返す（全部通れば null）
  async function useKeySet(k) {
    if (!k || !(k.sig && k.sig.privateKey instanceof CryptoKey) || !(k.kem && k.kem.privateKey instanceof CryptoKey) || !(k.aes instanceof CryptoKey)) return 'CryptoKey が得られない';
    try {
      const m = enc.encode('ft.probe/use');
      if (!(await sc.verify(SIGN, k.sig.publicKey, await sc.sign(SIGN, k.sig.privateKey, m), m))) return '署名が通らない';
    } catch (e) { return `署名：${errText(e)}`; }
    try {
      const other = await sc.generateKey(ECDH, false, ['deriveBits']);
      await sc.deriveBits({ name: 'ECDH', public: other.publicKey }, k.kem.privateKey, 256);
    } catch (e) { return `鍵合意：${errText(e)}`; }
    try {
      await sc.unwrapKey('raw', k.wrapped, k.aes, { name: 'AES-GCM', iv: k.wrapIv }, { name: 'AES-GCM' }, false, ['encrypt']);
    } catch (e) { return `包みの解除：${errText(e)}`; }
    try {
      const p = new TextDecoder().decode(await sc.decrypt({ name: 'AES-GCM', iv: k.iv }, k.aes, k.ct));
      if (p !== '本文を復号できた') return '復号の値が違う';
    } catch (e) { return `復号：${errText(e)}`; }
    return null;
  }

  // ---------- T-L3-12 (2) IndexedDB の鍵 ----------
  async function idbChecks() {
    let db;
    await step('L3-2-open', async () => { db = await idbOpen('ft-probe-l3', ['keys']); return true; });
    if (!db) return;
    // 開き直し：前に開いた時に置いた鍵を読む（初めて開いた時は not_run）
    try {
      const prev = await idbGet(db, 'keys', 'set');
      if (!prev) record('L3-2-reopen', 'not_run', 'まだ開き直していない（前に置いた鍵が無い）');
      else {
        const why = await useKeySet(prev);
        record('L3-2-reopen', why ? 'ng' : 'ok', why || `前に置いた鍵（${new Date(prev.created_at).toISOString()}）を読んで使えた`);
      }
    } catch (e) { record('L3-2-reopen', 'ng', errText(e)); }
    let set;
    await step('L3-2-put', async () => { set = await makeKeySet(); await idbPut(db, 'keys', 'set', set); return true; });
    await step('L3-2-get', async () => {
      const got = await idbGet(db, 'keys', 'set');
      const why = await useKeySet(got);
      return { ok: !why && got.sig.privateKey.extractable === false, detail: why || '読んだ鍵で署名、鍵合意、包みの解除、復号ができた' };
    });
    await step('L3-2-persist', async () => {
      const r = await navigator.storage.persist();
      return { ok: typeof r === 'boolean', detail: `persist()=${r}` };
    });
    db.close(); // 開いたままだと (a) の「消して作り直す」の削除が blocked になる
  }

  // ---------- T-L3-12 (3) パスキー、T-L6-09 (c) PRF ----------
  const PRF_SALT = enc.encode('ft.probe/prf-salt/0000000000000000').slice(0, 32);
  async function passkeyCreate() {
    const t0 = performance.now();
    try {
      const cred = await navigator.credentials.create({ publicKey: {
        rp: { id: location.hostname, name: 'ft.probe' },
        user: { id: crypto.getRandomValues(new Uint8Array(16)), name: `probe@${location.hostname}`, displayName: 'probe' },
        challenge: crypto.getRandomValues(new Uint8Array(32)),
        pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
        authenticatorSelection: { residentKey: 'required', userVerification: 'required' },
        timeout: PK_TIMEOUT,
        attestation: 'none',
        extensions: { prf: {}, credProps: true },
      } });
      const ext = cred.getClientExtensionResults();
      localStorage.setItem('ft-probe-cred', b64u(cred.rawId));
      record('L3-3-create', 'ok', { prf_enabled: ext.prf ? ext.prf.enabled === true : null, rk: ext.credProps ? ext.credProps.rk : null, attachment: cred.authenticatorAttachment }, Math.round(performance.now() - t0));
    } catch (e) {
      // NotAllowedError は利用者の操作が無かった（Simulator の生体認証を操作できなかった）。試験設計書 T-L3-12 の「未実施」
      record('L3-3-create', e && e.name === 'NotAllowedError' ? 'not_run' : 'ng', errText(e), Math.round(performance.now() - t0));
    }
  }
  async function passkeyGet(target) {
    const t0 = performance.now();
    try {
      const a = await navigator.credentials.get({ publicKey: {
        rpId: location.hostname,
        challenge: crypto.getRandomValues(new Uint8Array(32)),
        userVerification: 'required',
        allowCredentials: [],
        timeout: PK_TIMEOUT,
        extensions: { prf: { eval: { first: PRF_SALT } } },
      } });
      const ext = a.getClientExtensionResults();
      const first = ext.prf && ext.prf.results && ext.prf.results.first;
      const detail = { same_credential: b64u(a.rawId) === localStorage.getItem('ft-probe-cred'), prf_first_sha256_8: first ? await sha8(first) : null };
      if (target === 'L3') record('L3-3-get', 'ok', detail, Math.round(performance.now() - t0));
      return detail;
    } catch (e) {
      if (target === 'L3') record('L3-3-get', e && e.name === 'NotAllowedError' ? 'not_run' : 'ng', errText(e), Math.round(performance.now() - t0));
      throw e;
    }
  }

  // ---------- T-L6-09 (a) 端末の鍵の読み出し、(g) 保存の消去の後 ----------
  const DEVICE_DB = 'ft-probe-device';
  function history() {
    try { return JSON.parse(localStorage.getItem('ft-probe-history') || '{}'); } catch { return {}; }
  }
  function saveHistory() {
    try { localStorage.setItem('ft-probe-history', JSON.stringify({ a: P.device.a, c: P.device.c, f: P.device.f, g: P.device.g, notes: P.device.notes })); } catch { /* 保存できなくても画面と JSON には出る */ }
  }
  function deviceRow(list, entry, tableId) {
    list.push(entry);
    saveHistory();
    const tr = document.createElement('tr');
    const a = document.createElement('td');
    const b = document.createElement('td');
    a.textContent = new Date(entry.at).toISOString().slice(0, 19);
    b.textContent = JSON.stringify(entry);
    tr.append(a, b);
    $(tableId).append(tr);
  }
  async function deviceKeyState() {
    let db;
    try { db = await idbOpen(DEVICE_DB, ['keys']); } catch (e) { return { state: 'unreadable', why: `開けない：${errText(e)}` }; }
    let k;
    try { k = await idbGet(db, 'keys', 'device'); } catch (e) { db.close(); return { state: 'unreadable', why: `読み出し：${errText(e)}` }; }
    db.close();
    if (k === undefined) return { state: 'none' };
    const why = await useKeySet(k);
    return why ? { state: 'unreadable', why } : { state: 'ok', created_at: new Date(k.created_at).toISOString() };
  }
  async function deviceKeyCreate() {
    const db = await idbOpen(DEVICE_DB, ['keys']);
    try {
      await idbPut(db, 'keys', 'device', await makeKeySet());
      return { result: 'ok' };
    } catch (e) {
      // 保存の DataCloneError は Apple 版の WebKit でキーチェーンのマスター鍵が無い時の形（第12章 5.3 の「保存できない」）
      return { result: 'ng', why: errText(e) };
    } finally { db.close(); }
  }
  async function deviceReset() {
    // 第12章 5.3 の「読めない」の時の順のうち OS に関わる分：購読の解除、保存を全部消す、作り直す
    const steps = {};
    try {
      const reg = await navigator.serviceWorker.getRegistration();
      const sub = reg && (await reg.pushManager.getSubscription());
      steps.unsubscribe = sub ? String(await sub.unsubscribe()) : 'no subscription';
    } catch (e) { steps.unsubscribe = errText(e); }
    for (const name of [DEVICE_DB, 'ft-probe-l3']) {
      steps[`delete ${name}`] = await new Promise((res) => {
        const q = indexedDB.deleteDatabase(name);
        q.onsuccess = () => res('ok');
        q.onerror = () => res(errText(q.error));
        q.onblocked = () => res('blocked');
      });
    }
    steps.create = await deviceKeyCreate();
    steps.after = await deviceKeyState();
    return steps;
  }
  async function storageState() {
    // (g)：サイトのデータを消した後に開くと、localStorage の印も IndexedDB も無い（新しい端末として進む）
    const mark = localStorage.getItem('ft-probe-first-seen');
    const dbs = indexedDB.databases ? (await indexedDB.databases()).map((d) => d.name) : null;
    if (!mark) localStorage.setItem('ft-probe-first-seen', new Date().toISOString());
    return { first_seen: mark, fresh: !mark, databases: dbs, persisted: navigator.storage && navigator.storage.persisted ? await navigator.storage.persisted() : null };
  }

  // ---------- Web Push の購読と SW の記録（(b)、(d)、(e)） ----------
  async function swRegister() {
    if (!('serviceWorker' in navigator)) return null;
    const reg = await navigator.serviceWorker.register('./sw.js', { scope: './' });
    await navigator.serviceWorker.ready;
    return reg;
  }
  async function subscribe() {
    const vapid = $('vapid').value.trim();
    if (!vapid) throw new Error('VAPID の公開鍵を入れてください');
    localStorage.setItem('ft-probe-vapid', vapid);
    const perm = await Notification.requestPermission();
    if (perm !== 'granted') throw new Error(`通知の許可：${perm}`);
    const reg = await swRegister();
    let sub = await reg.pushManager.getSubscription();
    if (sub && b64u(sub.options.applicationServerKey || new ArrayBuffer(0)) !== vapid) { await sub.unsubscribe(); sub = null; }
    if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: unb64u(vapid) });
    P.subscription = { subscription: sub.toJSON(), page: location.href.split('?')[0] };
    $('sub-json').value = JSON.stringify(P.subscription);
    return P.subscription;
  }
  async function readSwLog() {
    try {
      const db = await idbOpen('ft-probe-sw', ['log']);
      P.sw_log = await idbReq(db, 'log', 'readonly', (s) => s.getAll());
      db.close();
    } catch (e) { P.sw_log = [{ error: errText(e) }]; }
    const t = $('t-swlog');
    t.textContent = '';
    for (const r of P.sw_log) {
      const tr = document.createElement('tr');
      for (const v of [r.at ? new Date(r.at).toISOString().slice(11, 19) : '', r.item, r.seq ? `${r.seq}/${r.of}` : '', r.form, r.key, r.shown, r.note || '']) {
        const td = document.createElement('td');
        td.textContent = v === undefined ? '' : String(v);
        tr.append(td);
      }
      t.append(tr);
    }
  }

  // ---------- T-L6-09 (f) カメラとマイク ----------
  async function mediaCheck() {
    const r = {};
    try {
      const s = await navigator.mediaDevices.getUserMedia({ audio: true, video: true });
      r.get_user_media = s.getTracks().map((t) => t.kind).join(',');
      s.getTracks().forEach((t) => t.stop());
    } catch (e) { r.get_user_media = errText(e); }
    try {
      const w = new Worker(URL.createObjectURL(new Blob(['self.onrtctransform = (e) => { e.transformer.readable.pipeTo(e.transformer.writable); };'], { type: 'text/javascript' })));
      const pc = new RTCPeerConnection();
      const t = pc.addTransceiver('audio');
      t.sender.transform = new RTCRtpScriptTransform(w, { side: 'send' });
      t.receiver.transform = new RTCRtpScriptTransform(w, { side: 'receive' });
      pc.close();
      w.terminate();
      r.rtc_rtp_script_transform = true;
    } catch (e) { r.rtc_rtp_script_transform = errText(e); }
    return r;
  }

  // ---------- ボタン ----------
  function bind(id, fn) {
    $(id).addEventListener('click', async () => {
      $(id).disabled = true;
      try { await fn(); } catch (e) { log(`${id}：${errText(e)}`); } finally { $(id).disabled = false; }
    });
  }
  function log(s) { $('log').textContent += `${s}\n`; }
  function result() {
    for (const k of Object.keys(P.device.notes)) delete P.device.notes[k];
    for (const el of document.querySelectorAll('textarea[data-note]')) if (el.value.trim()) P.device.notes[el.dataset.note] = el.value.trim();
    saveHistory();
    P.copied_at = new Date().toISOString();
    return JSON.stringify(P, null, 1);
  }

  bind('b-pk-create', async () => { await passkeyCreate(); });
  bind('b-pk-get', async () => {
    try { await passkeyGet('L3'); } catch { /* 記録は passkeyGet が書く */ }
    P.passkey_done = true;
  });
  bind('b-a-create', async () => { deviceRow(P.device.a, { at: Date.now(), action: 'create', ...(await deviceKeyCreate()), state: await deviceKeyState() }, 't-a'); });
  bind('b-a-reset', async () => { deviceRow(P.device.a, { at: Date.now(), action: 'reset', steps: await deviceReset() }, 't-a'); });
  bind('b-c-prf', async () => {
    let entry;
    try { entry = { at: Date.now(), ...(await passkeyGet('L6')) }; } catch (e) { entry = { at: Date.now(), error: errText(e) }; }
    deviceRow(P.device.c, entry, 't-c');
  });
  bind('b-sub', async () => { await subscribe(); log('購読した。「購読をコピー」で開発機へ送る'); });
  bind('b-sub-copy', async () => { await navigator.clipboard.writeText($('sub-json').value); log('購読をコピーした'); });
  bind('b-swlog', readSwLog);
  bind('b-swlog-clear', async () => {
    const db = await idbOpen('ft-probe-sw', ['log']);
    await idbReq(db, 'log', 'readwrite', (s) => s.clear());
    db.close();
    await readSwLog();
  });
  bind('b-f', async () => { deviceRow(P.device.f, { at: Date.now(), ...(await mediaCheck()) }, 't-f'); });
  bind('b-copy', async () => {
    const text = result();
    $('result').value = text;
    try { await navigator.clipboard.writeText(text); log(`結果をコピーした（${text.length} 文字）`); } catch (e) { log(`コピーできない（${errText(e)}）。下の欄を長押しで選んでください`); }
  });
  bind('b-save', async () => {
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([result()], { type: 'application/json' }));
    a.download = `ft-probe-${P.env.ios_version || 'x'}-${Date.now()}.json`;
    a.click();
  });

  // ---------- 開いた時に自動で走る分 ----------
  (async () => {
    const h = history();
    for (const k of ['a', 'c', 'f', 'g']) if (Array.isArray(h[k])) P.device[k] = h[k];
    Object.assign(P.device.notes, h.notes || {});
    for (const el of document.querySelectorAll('textarea[data-note]')) if (P.device.notes[el.dataset.note]) el.value = P.device.notes[el.dataset.note];
    $('vapid').value = localStorage.getItem('ft-probe-vapid') || '';
    env();
    record('L3-4-display-mode', 'ok', P.env.display_mode);
    record('L3-3-create', 'not_run', 'ボタンを押していない');
    record('L3-3-get', 'not_run', 'ボタンを押していない');
    await cryptoChecks();
    await idbChecks();
    try { deviceRow(P.device.g, { at: Date.now(), ...(await storageState()) }, 't-g'); } catch (e) { log(`(g)：${errText(e)}`); }
    try { deviceRow(P.device.a, { at: Date.now(), action: 'open', state: await deviceKeyState() }, 't-a'); } catch (e) { log(`(a)：${errText(e)}`); }
    try { await swRegister(); } catch (e) { log(`Service Worker：${errText(e)}`); }
    await readSwLog();
    if (navigator.serviceWorker) navigator.serviceWorker.addEventListener('message', (e) => { if (e.data && e.data.kind === 'sw_log') readSwLog(); });
    P.done = true;
    document.body.dataset.done = 'true';
    log('開いた時の確かめが終わった');
  })();
})();
