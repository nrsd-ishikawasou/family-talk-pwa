// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0
// 確かめのページの Service Worker（試験設計書 T-L6-09 の (b)、(d)、(e)）。
// push を受けるたびに、ページが IndexedDB に置いた取り出せない鍵（ft-probe-device）を読んで復号と署名を試み、結果を本文と記録に出す。
// 着信は第7章 DD-7-7 の形（20 秒ごとの 3 回は 1 回ごとに別の通知。call_end は最初の表示から 30 秒を待ってから閉じる）。
'use strict';
const CLOSE_AFTER_MS = 30000;

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

function idbOpen(name, stores) {
  return new Promise((res, rej) => {
    const q = indexedDB.open(name, 1);
    q.onupgradeneeded = () => { for (const s of stores) q.result.createObjectStore(s); };
    q.onsuccess = () => res(q.result);
    q.onerror = () => rej(q.error);
  });
}
function idbDo(db, store, mode, fn) {
  return new Promise((res, rej) => {
    const tx = db.transaction(store, mode);
    const r = fn(tx.objectStore(store));
    let value;
    if (r) r.onsuccess = () => { value = r.result; };
    tx.oncomplete = () => res(value);
    tx.onerror = () => rej(tx.error);
  });
}
const errText = (e) => (e && e.name ? `${e.name}: ${e.message}` : String(e));

async function appendLog(entry) {
  const db = await idbOpen('ft-probe-sw', ['log']);
  await idbDo(db, 'log', 'readwrite', (s) => s.add(entry, `${entry.at}-${Math.random()}`));
  db.close();
  for (const c of await self.clients.matchAll({ includeUncontrolled: true })) c.postMessage({ kind: 'sw_log' });
}

// ページの makeKeySet が置いた鍵を読み、AES で暗号文を復号し、署名鍵で署名する
async function useDeviceKey() {
  let db;
  try { db = await idbOpen('ft-probe-device', ['keys']); } catch (e) { return { key: `ng: 開けない ${errText(e)}` }; }
  try {
    const k = await idbDo(db, 'keys', 'readonly', (s) => s.get('device'));
    if (k === undefined) return { key: 'none' };
    if (!(k.aes instanceof CryptoKey) || !(k.sig && k.sig.privateKey instanceof CryptoKey)) return { key: 'ng: CryptoKey が得られない' };
    const text = new TextDecoder().decode(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: k.iv }, k.aes, k.ct));
    await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, k.sig.privateKey, new TextEncoder().encode('ft.probe/sw'));
    return { key: 'ok', text };
  } catch (e) {
    return { key: `ng: ${errText(e)}` };
  } finally { db.close(); }
}

// 中身の形を 3 つに分ける。旧来の形（ft_probe と ft.push/1）、宣言型（OS が解析し event.notification を渡す）、
// 宣言型の JSON を SW が data として受けた形（宣言型に対応しない 18.4 より前の版）
function parse(event) {
  let j = null;
  try { if (event.data) j = event.data.json(); } catch { j = null; }
  if (event.notification) {
    const n = event.notification;
    const m = /確かめ (\w) (\d+)\/(\d+)/.exec(n.title || '');
    return { form: 'declarative', push: n.data || {}, tag: n.tag, item: m ? m[1] : null, seq: m ? Number(m[2]) : null, of: m ? Number(m[3]) : null, badge: null };
  }
  if (j && j.web_push === 8030 && j.notification) {
    const n = j.notification;
    const m = /確かめ (\w) (\d+)\/(\d+)/.exec(n.title || '');
    return { form: 'declarative-as-data', push: n.data || {}, tag: n.tag, item: m ? m[1] : null, seq: m ? Number(m[2]) : null, of: m ? Number(m[3]) : null, badge: n.app_badge ? Number(n.app_badge) : null };
  }
  if (j && j.ft_probe) return { form: 'legacy', push: j.push || {}, tag: j.ft_probe.tag, item: j.ft_probe.item, seq: j.ft_probe.seq, of: j.ft_probe.of, badge: j.ft_probe.badge || null };
  return { form: 'unknown', push: {}, tag: 'ft-probe', item: null, seq: null, of: null, badge: null };
}

async function setBadge(n) {
  if (!n) return 'none';
  try {
    if (self.navigator && self.navigator.setAppBadge) { await self.navigator.setAppBadge(n); return `set ${n}`; }
    return 'absent';
  } catch (e) { return errText(e); }
}

async function callShownAt(callId, now) {
  const db = await idbOpen('ft-probe-calls', ['calls']);
  try {
    const prev = await idbDo(db, 'calls', 'readonly', (s) => s.get(callId));
    if (prev) return prev;
    await idbDo(db, 'calls', 'readwrite', (s) => s.put(now, callId));
    return now;
  } finally { db.close(); }
}

self.addEventListener('push', (event) => {
  const at = Date.now();
  event.waitUntil((async () => {
    const p = parse(event);
    const kind = p.push.kind || 'message';
    const k = await useDeviceKey();
    const entry = { at, item: p.item, seq: p.seq, of: p.of, form: p.form, kind, key: k.key };
    try {
      if (kind === 'call') {
        await callShownAt(p.push.call_id, Date.now());
        await self.registration.showNotification(`確かめさんから着信 ${p.seq || ''}/${p.of || ''}`, { body: '応答するには開いてください', tag: `call:${p.push.call_id}`, data: { url: './' } });
        entry.shown = 'call';
      } else if (kind === 'call_end') {
        const first = await callShownAt(p.push.call_id, Date.now());
        const wait = Math.max(0, first + CLOSE_AFTER_MS - Date.now());
        if (wait) await new Promise((r) => setTimeout(r, wait));
        const open = await self.registration.getNotifications({ tag: `call:${p.push.call_id}` });
        open.forEach((n) => n.close());
        await self.registration.showNotification(p.push.reason === 'missed' ? '不在着信（確かめ）' : '通話が終わりました（確かめ）', { body: '確かめさん', tag: `call_end:${p.push.call_id}`, silent: p.push.reason !== 'missed', data: { url: './' } });
        entry.shown = 'call_end';
        entry.note = `待ち ${wait} ms、閉じた通知 ${open.length}`;
      } else {
        const body = k.key === 'ok' ? k.text : `汎用の本文（鍵：${k.key}）`;
        await self.registration.showNotification(`確かめ ${p.item || '?'} ${p.seq || ''}/${p.of || ''}`, { body, tag: p.tag || 'ft-probe', data: { url: './' } });
        entry.shown = 'message';
        // 宣言型ではバッジに触れない（app_badge が OS の側でバッジを動かすかを見るため。T-L6-09 の (d)）
        entry.note = p.form === 'declarative' ? 'バッジは app_badge に任せる' : `バッジ ${await setBadge(p.badge || p.seq)}`;
      }
    } catch (e) {
      entry.shown = `ng: ${errText(e)}`;
    }
    entry.ms = Date.now() - at;
    await appendLog(entry);
  })());
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  event.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    if (all.length) return all[0].focus();
    return self.clients.openWindow('./');
  })());
});

self.addEventListener('pushsubscriptionchange', (event) => {
  event.waitUntil(appendLog({ at: Date.now(), item: null, form: 'pushsubscriptionchange', kind: 'pushsubscriptionchange', key: '', shown: '' }));
});
