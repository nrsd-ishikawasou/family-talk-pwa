// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// InboxKind は端末宛メールボックスの種類（第12章 5.2 の表）。
type InboxKind string

// 種類の 8 つ。welcome は Commit の受け付けでサーバーだけが置く（PutWelcome。端末からの要求の kind の列挙に無く、境界の形の検査で
// NET-001）。
const (
	InboxWelcome              InboxKind = "welcome"
	InboxKeybundle            InboxKind = "keybundle"
	InboxKeyBackup            InboxKind = "key_backup"
	InboxKeyRequest           InboxKind = "key_request"
	InboxShare                InboxKind = "share"
	InboxCallKeyRequest       InboxKind = "call_key_request"
	InboxCallKey              InboxKind = "call_key"
	InboxStickerRemoveRequest InboxKind = "sticker_remove_request"
)

// InboxItem は端末宛メールボックスの 1 件（GET /me/inbox の要素）。
type InboxItem struct {
	Id         uuid.UUID
	Kind       InboxKind
	FromDevice uuid.UUID
	// Ciphertext は HPKE の暗号文（welcome は Welcome の MLSMessage そのもの）。
	Ciphertext []byte
	// Sig は送り手の端末の署名（r と s の 64 バイト）。welcome だけ nil。
	Sig       []byte
	Pos       int64
	CreatedAt time.Time
}

// Inbox は端末宛メールボックス（to-device）を持つ（B-085。第12章 5.2、第2章 5.4）。
type Inbox struct {
	d         *Deps
	devices   *DeviceKeys
	retention *store.Retention
}

// Put は POST /devices/{did}/inbox（と srv-rooms の中から）。kind welcome と share を渡すのはプログラムの誤り（Fault。welcome は
// PutWelcome、share は Shares.Approve だけが置く。どちらも要求の kind の列挙に無い）。sig を fromDevice の登録した署名鍵で第12章 5.2 の
// 対象に対して確かめ、通らなければ KEY-009。暗号文が 64 KiB を超えれば KEY-009、宛先の端末の期限の内の件数が 1,000 件で KEY-010。
// kind keybundle の宛先は送り手と同じ構成員の端末だけ（KEY-009）。宛先の端末に鍵の登録が無いか、取り消し済み・期限切れなら KEY-001。
// device.inbox{count} を toDevice の持ち主の feed に、宛先の端末 = toDevice で書く。期限 30 日。
func (i *Inbox) Put(ctx context.Context, tx *store.Tx, fromDevice, toDevice uuid.UUID, kind InboxKind, ciphertext, sig []byte) (uuid.UUID, error) {
	if kind == InboxWelcome || kind == InboxShare {
		return uuid.Nil, fault.Wrap(errors.New("keys: 端末からは置けない種類: " + string(kind)))
	}
	return i.put(ctx, tx, fromDevice, toDevice, kind, ciphertext, sig, true)
}

// put は Put と Shares.Approve の本体。cap は 1 端末 1,000 件の上限を掛けるか（POST /devices/{did}/inbox の経路だけ）。
func (i *Inbox) put(ctx context.Context, tx *store.Tx, from, to uuid.UUID, kind InboxKind, ciphertext, sig []byte, cap bool) (uuid.UUID, error) {
	if len(ciphertext) > inboxItemMax || len(sig) != 64 {
		return uuid.Nil, ev(cgen.KEY009)
	}
	toReg, err := i.reachable(ctx, tx, to)
	if err != nil {
		return uuid.Nil, err
	}
	fromReg, ok, err := readRegistration(ctx, tx, from, false)
	if err != nil {
		return uuid.Nil, err
	}
	if !ok {
		return uuid.Nil, ev(cgen.KEY001)
	}
	if kind == InboxKeybundle && fromReg.userId != toReg.userId {
		return uuid.Nil, ev(cgen.KEY009)
	}
	if !verifyRS(fromReg.sigPub, inboxMessage(kind, from, to, ciphertext), sig) {
		return uuid.Nil, ev(cgen.KEY009)
	}
	now := i.d.now()
	if cap {
		n, err := i.countOf(ctx, tx, to, now)
		if err != nil {
			return uuid.Nil, err
		}
		if n >= inboxDeviceMax {
			return uuid.Nil, ev(cgen.KEY010)
		}
	}
	return i.insert(ctx, tx, from, to, toReg.userId, kind, ciphertext, sig, now)
}

// reachable は宛先の端末の鍵の登録と状態を確かめる（無いか取り消し済み・期限切れなら KEY-001）。
func (i *Inbox) reachable(ctx context.Context, tx *store.Tx, to uuid.UUID) (registration, error) {
	reg, ok, err := readRegistration(ctx, tx, to, false)
	if err != nil {
		return registration{}, err
	}
	if !ok {
		return registration{}, ev(cgen.KEY001)
	}
	dv, err := i.d.Identity.Devices.Get(ctx, tx, to)
	if err != nil {
		return registration{}, err
	}
	if dv.State == identity.DeviceRevoked || dv.State == identity.DeviceExpired {
		return registration{}, ev(cgen.KEY001)
	}
	return reg, nil
}

// countOf は宛先の端末の期限の内の件数（行の錠を取らない。ブロックのページの錠の順の 9）。
func (i *Inbox) countOf(ctx context.Context, q store.Querier, to uuid.UUID, now time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM device_inbox WHERE to_device = $1 AND expires_at > $2`, to, now).Scan(&n)
	return n, wrap(err)
}

// insert は 1 件を置き、device.inbox{count} を宛先の持ち主へ（宛先の端末だけに届く）。
func (i *Inbox) insert(ctx context.Context, tx *store.Tx, from, to, owner uuid.UUID, kind InboxKind, ciphertext, sig []byte, now time.Time) (uuid.UUID, error) {
	id := i.d.Ids.NewV7()
	if _, err := tx.Exec(ctx, `INSERT INTO device_inbox (id, to_device, from_device, kind, ciphertext, sig, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, to, from, string(kind), ciphertext, sig, now, now.Add(inboxTtl)); err != nil {
		return uuid.Nil, wrap(err)
	}
	n, err := i.countOf(ctx, tx, to, now)
	if err != nil {
		return uuid.Nil, err
	}
	if err := i.d.emit(tx, []uuid.UUID{owner}, "device.inbox", uuid.Nil, to, deviceInbox{Count: n}); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// PutWelcome は Groups.Commit から。kind welcome、署名は無し（Welcome の GroupInfo の署名で守られる。第12章 5.2）。件数の上限は掛けない。
// 宛先の端末の鍵の登録が無ければ Fault（Commit の確かめで Add の端末は鍵があることを確かめ済み）。device.inbox は Put と同じ。
func (i *Inbox) PutWelcome(ctx context.Context, tx *store.Tx, fromDevice, toDevice uuid.UUID, welcome []byte) (uuid.UUID, error) {
	reg, ok, err := readRegistration(ctx, tx, toDevice, false)
	if err != nil {
		return uuid.Nil, err
	}
	if !ok {
		return uuid.Nil, fault.Wrap(errors.New("keys: Welcome の宛先の端末に鍵の登録が無い"))
	}
	return i.insert(ctx, tx, fromDevice, toDevice, reg.userId, InboxWelcome, welcome, nil, i.d.now())
}

// ListSince は GET /me/inbox?since=&limit=。その端末宛ての期限の内の項目を pos の順に limit 件と has_more（limit + 1 件を読む）。
func (i *Inbox) ListSince(ctx context.Context, deviceId uuid.UUID, pos int64, limit int) ([]InboxItem, bool, error) {
	rows, err := i.d.Pool.Query(ctx, `SELECT id, kind, from_device, ciphertext, sig, pos, created_at FROM device_inbox
WHERE to_device = $1 AND pos > $2 AND expires_at > $3 ORDER BY pos LIMIT $4`, deviceId, pos, i.d.now(), limit+1)
	if err != nil {
		return nil, false, wrap(err)
	}
	defer rows.Close()
	out := []InboxItem{}
	for rows.Next() {
		var it InboxItem
		var kind string
		if err := rows.Scan(&it.Id, &kind, &it.FromDevice, &it.Ciphertext, &it.Sig, &it.Pos, &it.CreatedAt); err != nil {
			return nil, false, wrap(err)
		}
		it.Kind, it.CreatedAt = InboxKind(kind), it.CreatedAt.UTC()
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, false, wrap(err)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// Delete は DELETE /me/inbox/{id}（受け取ったら消す）。その端末宛ての物だけ。無ければ何もしない（DELETE の冪等）。
func (i *Inbox) Delete(ctx context.Context, tx *store.Tx, deviceId, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM device_inbox WHERE id = $1 AND to_device = $2`, id, deviceId)
	return wrap(err)
}

// Prune は期限を過ぎた項目を消す（srv-worker PruneHandler。srv-store Retention で 1 万行ずつ）。戻りは消した数。
func (i *Inbox) Prune(ctx context.Context) (int64, error) {
	return i.retention.DeleteOlder(ctx, "device_inbox", "expires_at", i.d.now())
}

// DeleteAllOfDevice はその端末宛ての項目と、その端末が送った項目を全部消す（hook OnDeviceRevoked。送り手の鍵が消えるので、受け手は
// 署名を確かめられない）。
func (i *Inbox) DeleteAllOfDevice(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM device_inbox WHERE to_device = $1 OR from_device = $1`, deviceId)
	return wrap(err)
}
