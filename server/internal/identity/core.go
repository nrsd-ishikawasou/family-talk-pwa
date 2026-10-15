// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rivo/uniseg"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/envelope"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/secret"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 期間と上限（設計書の値。設定の項目がある物は Config から読む）。
const (
	reauthWindow      = 5 * time.Minute     // 第3章 4.5
	challengeTtl      = 5 * time.Minute     // 第3章 4（login_challenges）
	qrTtl             = 10 * time.Minute    // 第3章 4.4
	qrInterval        = 5 * time.Second     // 第3章 4.4（RFC 8628 の interval）
	wsTokenTtl        = 60 * time.Second    // 第2章 4.4
	shellTokenTtl     = 24 * time.Hour      // 第2章 4.4
	setupLinkTtl      = 7 * 24 * time.Hour  // 第3章 3.1
	resetLinkTtl      = 24 * time.Hour      // 第3章 4.6
	transferTtl       = 7 * 24 * time.Hour  // 第3章 8.4
	relaxAge          = 16                  // 第3章 9.1（時間帯とリンクの確認の緩和）
	childAge          = 18                  // 第3章 9.1（子ども区分は 18 歳未満）
	approvalEraseTtl  = 7 * 24 * time.Hour  // 第3章 8.3
	approvalDeviceTtl = 10 * time.Minute    // 第3章 9.2（端末のコードと同じ）
	eraseGrace        = 24 * time.Hour      // 第3章 8.3
	touchEvery        = time.Minute         // 無操作の期限の延長と最終の利用の書き込みの間引き
	sessionKeepAfter  = 90 * 24 * time.Hour // 失効したセッションの行を残す期間
	peekDelay         = 250 * time.Millisecond
	nameMax           = 20 // 表示名、家族の名前、別名（書記素。第3章 6）
	deviceNameMax     = 40 // 端末の名前（書記素。第3章 5.2）
	expireBatch       = 1000
	pageDefault       = 50 // 第2章 3
)

// querier は Pool と Tx の読み書きの共通の口（srv-store の Querier）。
type querier = store.Querier

// wrap は DB と部品の誤りを Fault にする。既に Event か Fault ならそのまま返す。
func wrap(err error) error {
	if err == nil {
		return nil
	}
	var ev *fault.Event
	var f *fault.Fault
	if errors.As(err, &ev) || errors.As(err, &f) {
		return err
	}
	return fault.Wrap(err)
}

// noRows は 1 行の読みで行が無いこと。
func noRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// uniqueViolation は一意の索引の違反（SQLSTATE 23505）。
func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// ev は番号の Event（params なし）。
func ev(code string) *fault.Event {
	return fault.New(code, nil)
}

// now は記録の時刻（Deps.Clock）。
func (d *Deps) now() time.Time {
	return d.Clock.Now()
}

// inTx は ctx のトランザクション（冪等の middleware）があればそれに乗り、無ければ開いて fn を走らせる。
func (d *Deps) inTx(ctx context.Context, fn func(tx *store.Tx) error) error {
	return d.Pool.InTx(ctx, fn)
}

// separateTx は要求のトランザクションの外の新しいトランザクションで fn を確定する（誤りの応答を返す前に残す記録。要求のトランザクションは誤りで戻る）。
func (d *Deps) separateTx(ctx context.Context, fn func(tx *store.Tx) error) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return wrap(err)
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return wrap(tx.Commit(ctx))
}

// graphemes は書記素の数（第1章 8.6）。
func graphemes(s string) int {
	return uniseg.GraphemeClusterCount(s)
}

// checkName は 1〜max 書記素の名前か（空白だけは不可）。違えば MEM-005。
func checkName(s string, max int) *fault.Event {
	if strings.TrimSpace(s) == "" || graphemes(s) > max {
		return ev(gen.MEM005)
	}
	return nil
}

// reauthed は主体の再認証が 5 分以内か（第3章 4.5。判定は業務。srv-boundary のページ）。
func (d *Deps) reauthed(p boundary.Principal) bool {
	return p.ReauthAt != nil && d.now().Sub(*p.ReauthAt) <= reauthWindow
}

// requireReauth は x-reauth の経路の判定。足りなければ AUTH-006。
func (d *Deps) requireReauth(p boundary.Principal) error {
	if !d.reauthed(p) {
		return ev(gen.AUTH006)
	}
	return nil
}

// self は本人の経路の主体の構成員と端末（session の認可を通った後なので必ずある）。
func self(p boundary.Principal) (uuid.UUID, uuid.UUID, error) {
	if p.Kind != boundary.KindUser || p.UserId == nil || p.DeviceId == nil {
		return uuid.Nil, uuid.Nil, ev(gen.AUTH001)
	}
	return *p.UserId, *p.DeviceId, nil
}

// feed のイベントの payload（asyncapi の各 message の payload。additionalProperties は false）。

type deviceChanged struct {
	UserId   uuid.UUID  `json:"user_id"`
	DeviceId uuid.UUID  `json:"device_id"`
	Change   string     `json:"change"`
	By       *uuid.UUID `json:"by,omitempty"`
}

type userChanged struct {
	UserId uuid.UUID `json:"user_id"`
	Fields []string  `json:"fields"`
}

type sessionRevoked struct {
	Reason RevokeReason `json:"reason"`
}

// device.changed の change の 5 つ（guardian_approved は保護者が子の端末の鍵の承認を通した時。第3章 4.4）。
const (
	changeAdded            = "added"
	changeApproved         = "approved"
	changeGuardianApproved = "guardian_approved"
	changeRevoked          = "revoked"
	changeRenamed          = "renamed"
)

// user.changed の fields の値（User の欄の名）。
const (
	fieldDisplayName = "display_name"
	fieldRole        = "role"
	fieldState       = "state"
	fieldChild       = "child"
)

// emit は 1 件のイベントを宛先の構成員ごとの feed に同じトランザクションで溜める（全体実装設計書 2.4）。
// device は宛先の端末（session.revoked だけが持つ）。採番と書き込みと pg_notify はコミットの直前に srv-store の Flush が
// 宛先の識別子の順に行うので、本ブロックは宛先の重複を除くだけでよい。
func (d *Deps) emit(_ context.Context, tx *store.Tx, users []uuid.UUID, kind string, device uuid.UUID, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return wrap(err)
	}
	list := slices.Clone(users)
	slices.SortFunc(list, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	list = slices.Compact(list)
	id := d.Ids.NewV7()
	ts := d.now()
	for _, u := range list {
		d.Feed.Append(tx, u, store.FeedRef{Kind: kind, Id: id, DeviceId: device, Payload: body, Ts: ts})
	}
	return nil
}

// emitDeviceChanged は device.changed を家族の全員へ（本人を含む。第2章 6.2）。
func (d *Deps) emitDeviceChanged(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID, change string, by *uuid.UUID) error {
	to, err := familyMembers(ctx, tx)
	if err != nil {
		return err
	}
	return d.emit(ctx, tx, to, "device.changed", uuid.Nil, deviceChanged{UserId: userId, DeviceId: deviceId, Change: change, By: by})
}

// emitUserChanged は user.changed を家族の全員へ。
func (d *Deps) emitUserChanged(ctx context.Context, tx *store.Tx, userId uuid.UUID, fields ...string) error {
	to, err := familyMembers(ctx, tx)
	if err != nil {
		return err
	}
	return d.emit(ctx, tx, to, "user.changed", uuid.Nil, userChanged{UserId: userId, Fields: fields})
}

// 宛先の解き方（feed の宛先の構成員）。

// familyMembers は家族の全員（削除と抹消を除く）。
func familyMembers(ctx context.Context, q querier) ([]uuid.UUID, error) {
	return uuids(ctx, q, `SELECT id FROM users WHERE state IN ('normal', 'suspended', 'erase_pending') ORDER BY id`)
}

// uuids は 1 列の識別子の読み。
func uuids(ctx context.Context, q querier, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, wrap(err)
		}
		out = append(out, id)
	}
	return out, wrap(rows.Err())
}

// aad はエンベロープの認証の追加データの文脈 ft/<表>/<列>/<行の識別子>（第12章 12）。
func aad(table, column string, row ...string) []byte {
	return []byte("ft/" + table + "/" + column + "/" + strings.Join(row, "/"))
}

// userKeys は構成員の DEK の作成と開け閉め（第12章 6.2。DEK は使い終えたら 0 で埋める）。
type userKeys struct {
	d *Deps
}

// errNoDek は DEK が無い（抹消済みの構成員。crypto-shredding の後）。
var errNoDek = errors.New("identity: DEK が無い")

// create は構成員の DEK を作り、今の KEK で包んで user_dek に置く。users の行が先に要る。
func (k userKeys) create(ctx context.Context, tx *store.Tx, userId, dekId uuid.UUID) error {
	keks, err := k.d.Secrets.Kek()
	if err != nil {
		return wrap(err)
	}
	defer zeroKeks(keks)
	dek := k.d.Envelope.NewDek()
	defer secret.Zero(dek)
	cur := keks[0]
	for _, kk := range keks[1:] {
		if kk.CreatedAt.After(cur.CreatedAt) {
			cur = kk
		}
	}
	w, err := k.d.Envelope.WrapDek(cur, dekId, dek)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO user_dek (user_id, wrapped) VALUES ($1, $2)`, userId, w.Encode())
	return wrap(err)
}

// with は構成員の DEK を開いて fn に渡し、終えたら 0 で埋める。
func (k userKeys) with(ctx context.Context, q querier, userId uuid.UUID, fn func(dek []byte, dekId uuid.UUID) error) error {
	var dekId uuid.UUID
	var wrapped []byte
	err := q.QueryRow(ctx, `SELECT u.dek_id, d.wrapped FROM users u JOIN user_dek d ON d.user_id = u.id WHERE u.id = $1`, userId).Scan(&dekId, &wrapped)
	if noRows(err) {
		return fault.Wrap(errNoDek)
	}
	if err != nil {
		return wrap(err)
	}
	f, err := envelope.DecodeFdek(wrapped)
	if err != nil {
		return wrap(err)
	}
	keks, err := k.d.Secrets.Kek()
	if err != nil {
		return wrap(err)
	}
	dek, err := k.d.Envelope.UnwrapDek(keks, dekId, f)
	zeroKeks(keks)
	if err != nil {
		return err
	}
	defer secret.Zero(dek)
	return fn(dek, dekId)
}

// seal は userId の DEK で plaintext を封じた fenv/1 のバイト列を返す。
func (k userKeys) seal(ctx context.Context, q querier, userId uuid.UUID, plaintext, adata []byte) ([]byte, error) {
	var out []byte
	err := k.with(ctx, q, userId, func(dek []byte, dekId uuid.UUID) error {
		f, err := k.d.Envelope.Seal(dek, dekId, plaintext, adata)
		if err != nil {
			return err
		}
		out = f.Encode()
		return nil
	})
	return out, err
}

// open は userId の DEK で fenv/1 のバイト列を開く。
func (k userKeys) open(ctx context.Context, q querier, userId uuid.UUID, blob, adata []byte) ([]byte, error) {
	var out []byte
	err := k.with(ctx, q, userId, func(dek []byte, dekId uuid.UUID) error {
		var err error
		out, err = openWith(k.d, dek, dekId, blob, adata)
		return err
	})
	return out, err
}

// openWith は開いた DEK で 1 つの fenv/1 を開く（同じ構成員の値を続けて開く時）。
func openWith(d *Deps, dek []byte, dekId uuid.UUID, blob, adata []byte) ([]byte, error) {
	f, err := envelope.DecodeFenv(blob)
	if err != nil {
		return nil, wrap(err)
	}
	return d.Envelope.Open(dek, dekId, f, adata)
}

// sealWith は開いた DEK で 1 つの値を封じる。
func sealWith(d *Deps, dek []byte, dekId uuid.UUID, plaintext, adata []byte) ([]byte, error) {
	f, err := d.Envelope.Seal(dek, dekId, plaintext, adata)
	if err != nil {
		return nil, err
	}
	return f.Encode(), nil
}

// zeroKeks は読んだ KEK を 0 で埋める。
func zeroKeks(keks []secret.KekWithId) {
	for _, k := range keks {
		secret.Zero(k.Key)
	}
}

// cursor は一覧の引数（第2章 3）。limit は 1〜100（範囲の外は srv-boundary の検証が NET-001 で先に返す）。
type cursor[K any] struct {
	before *K
	after  *K
	limit  int
}

// newCursor は引数から作る。limit が無ければ 50。
func newCursor[K any](limit *int, before, after *K) cursor[K] {
	c := cursor[K]{before: before, after: after, limit: pageDefault}
	if limit != nil {
		c.limit = *limit
	}
	return c
}

// ascending は after から進む向きか（after がある時。両方なら after の側から before へ）。
// 無い時は最新の側から古い側へ読み、返す前に古い順に並べ直す。
func (c cursor[K]) ascending() bool {
	return c.after != nil
}

// finish は limit + 1 件まで読んだ結果を limit に切り、has_more と古い順の並びにする。
// has_more は進む向きにまだ要素があること（after だけの時と両方の時は新しい側、それ以外は古い側）。
func finish[T any, K any](items []T, c cursor[K]) ([]T, bool) {
	more := len(items) > c.limit
	if more {
		items = items[:c.limit]
	}
	if !c.ascending() {
		slices.Reverse(items)
	}
	if items == nil {
		items = []T{}
	}
	return items, more
}

// uuids は Page の before と after を識別子にする（形は srv-boundary の検証が UUIDv7 で済ませている）。
func (p Page) uuids() (cursor[uuid.UUID], error) {
	var before, after *uuid.UUID
	for _, pair := range []struct {
		s   *string
		dst **uuid.UUID
	}{{p.Before, &before}, {p.After, &after}} {
		if pair.s == nil {
			continue
		}
		u, err := uuid.FromString(*pair.s)
		if err != nil {
			return cursor[uuid.UUID]{}, fault.New(gen.NET001, map[string]any{"errors": []map[string]any{{"path": "query", "message": "UUIDv7 でない"}}})
		}
		*pair.dst = &u
	}
	lim := p.Limit
	var lp *int
	if lim > 0 {
		lp = &lim
	}
	return newCursor(lp, before, after), nil
}

// whereUUID は識別子（UUIDv7 は作った順に並ぶ）の列 col で cursor の条件、並び、LIMIT（limit + 1）を sql に足す。
// sql は既に WHERE を持つこと。
func whereUUID(c cursor[uuid.UUID], sql, col string, args []any) (string, []any) {
	if c.after != nil {
		args = append(args, *c.after)
		sql += " AND " + col + " > $" + strconv.Itoa(len(args))
	}
	if c.before != nil {
		args = append(args, *c.before)
		sql += " AND " + col + " < $" + strconv.Itoa(len(args))
	}
	order := " DESC"
	if c.ascending() {
		order = " ASC"
	}
	args = append(args, c.limit+1)
	return sql + " ORDER BY " + col + order + " LIMIT $" + strconv.Itoa(len(args)), args
}
