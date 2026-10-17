// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 依存の逆転：次の interface と hook は本ブロックが定め、上のブロックが実装・登録する（ページの「依存」、B-067、B-070）。

// DeviceKeyReader は端末と構成員の公開鍵を読む（srv-keys が実装する）。
type DeviceKeyReader interface {
	// VerifyProof は端末の署名鍵（fdev/1）で message への sig を確かめる。message は第12章 5.3 の連結。
	// 公開鍵が無ければ false。
	// q は ctx のトランザクションか Pool（トランザクションの中ではそれを渡す。全体実装設計書 2.4。4 つとも同じ）。
	VerifyProof(ctx context.Context, q store.Querier, deviceId uuid.UUID, message, sig []byte) (bool, error)
	// SigningKeys は構成員ごとの fusr/1（/users の user_sig_pub。srv-keys UserSigningKeys.Get）。
	SigningKeys(ctx context.Context, q store.Querier, userIds []uuid.UUID) (map[uuid.UUID]gen.Fusr1, error)
	// DevicesOf は構成員の承認済みの端末の fdev/1（/users/{id}。srv-keys DeviceKeys.ListOfUser）。
	DevicesOf(ctx context.Context, q store.Querier, userId uuid.UUID) ([]gen.Fdev1, error)
	// KeysOf は構成員の登録のある全端末（未承認と休眠を含む）の fdev/1 を端末の識別子から引く（GET /me/devices の keys。
	// 既存の端末が承認の前に新しい端末の鍵の指紋を求める。第3章 4.4、第12章 5.3）。
	KeysOf(ctx context.Context, q store.Querier, userId uuid.UUID) (map[uuid.UUID]gen.Fdev1, error)
}

// PushReader は端末の通知の購読の有無を読む（srv-push が Subscriptions.OfUser で実装する。GET /me/devices の push）。
type PushReader interface {
	// SubscribedDevices は構成員の端末のうち購読を持つ物の識別子。q は ctx のトランザクションか Pool。
	SubscribedDevices(ctx context.Context, q store.Querier, userId uuid.UUID) ([]uuid.UUID, error)
}

// Audit は管理者の操作の記録（srv-admin が実装する）。
type Audit interface {
	Record(ctx context.Context, tx *store.Tx, actor uuid.UUID, action string, target string, params map[string]any) error
}

// AudienceKind は知らせの宛先の種類（srv-push Notices.Emit の audience）。
type AudienceKind string

// 宛先の 5 つ（第7章 9）。
const (
	AudienceSelf      AudienceKind = "self"
	AudienceAll       AudienceKind = "all"
	AudienceAdmins    AudienceKind = "admins"
	AudienceGuardians AudienceKind = "guardians"
	AudienceRoom      AudienceKind = "room"
)

// Audience は知らせの宛先。self は UserId の本人、guardians は UserId の子の保護者、room は RoomId のトークの構成員（active。
// srv-push が srv-rooms Members.Of で解く）。all と admins はどちらも使わない。
type Audience struct {
	Kind   AudienceKind
	UserId *uuid.UUID
	RoomId *uuid.UUID
}

// Notice は OnNotice に渡す 1 件の知らせ（第7章 9 の表の kind と埋め込みの値）。Key は事象の鍵（第7章 9。srv-push が (kind, key, 日)
// で同じ事象の重複を抑える）。Audiences は宛先の列（1 つ以上。同じ種類を何度でも並べてよい）。1 つの事象は 1 回の発行で宛先の全部を
// 渡す（宛先ごとに分けて 2 回発行すると 2 回目以降は重複の抑えで何も出ない。第7章 9、全体実装設計書 I-17）。srv-push が宛先を
// 構成員へ解いて和を取り、同じ人を 1 回にする。
type Notice struct {
	Kind      string
	Key       string
	Params    map[string]any
	Audiences []Audience
}

// Hooks は本ブロックの出来事を他のブロックへ渡す（B-070）。fn は呼び出しと同じトランザクションで、
// 登録の順に走る。最初の誤りで止まり、その誤りでトランザクションを rollback させる（I-8）。
// 登録は起動の時だけ（上のブロックの組み立ての中）。Identity.Ready の後の登録はプログラムの誤り。
type Hooks struct {
	beforeUserChange   []func(ctx context.Context, tx *store.Tx, userId uuid.UUID, deviceIds []uuid.UUID) error
	beforeDeviceRevoke []func(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error
	deviceRevoked      []func(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error
	userSuspended      []func(ctx context.Context, tx *store.Tx, userId uuid.UUID, reason RevokeReason) error
	userUnsuspended    []func(ctx context.Context, tx *store.Tx, userId uuid.UUID) error
	userDeleted        []func(ctx context.Context, tx *store.Tx, userId uuid.UUID, eraseContent bool) error
	userErased         []func(ctx context.Context, tx *store.Tx, userId uuid.UUID) error
	userCreated        []func(ctx context.Context, tx *store.Tx, userId, firstDeviceId uuid.UUID) error
	notice             []func(ctx context.Context, tx *store.Tx, n Notice) error
	passkeyDeleted     []func(ctx context.Context, tx *store.Tx, userId uuid.UUID, credId []byte) error
	passkeyReset       []func(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID) error
	sealed             bool
}

// open は登録できるかを確かめる。
func (h *Hooks) open() {
	if h.sealed {
		panic("identity: 起動の後に hook を登録した")
	}
}

// RegisterOnBeforeUserChange は停止・削除・抹消の始めの hook を足す（srv-rooms だけが登録する）。Users が users、devices、sessions
// の行の錠の後、他の hook の前に呼ぶ。deviceIds は本人の全端末（全状態。識別子の順）。srv-rooms がそのトランザクションが触れ得る
// 全部のトークの錠を識別子の順に取る（全体実装設計書 2.4、I-18）。
func (h *Hooks) RegisterOnBeforeUserChange(fn func(ctx context.Context, tx *store.Tx, userId uuid.UUID, deviceIds []uuid.UUID) error) {
	h.open()
	h.beforeUserChange = append(h.beforeUserChange, fn)
}

// RegisterOnBeforeDeviceRevoke は端末の取り消しの始めの hook を足す（srv-rooms だけが登録する）。Devices.Revoke が devices と
// sessions の行の錠の後、OnDeviceRevoked の前に呼ぶ。srv-rooms がその端末のトークと家族全員のトークの錠を識別子の順に取る
// （全体実装設計書 2.4、I-18）。
func (h *Hooks) RegisterOnBeforeDeviceRevoke(fn func(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error) {
	h.open()
	h.beforeDeviceRevoke = append(h.beforeDeviceRevoke, fn)
}

// RegisterOnDeviceRevoked は端末の取り消しの hook を足す（srv-keys の鍵の失効、srv-push の購読とメールボックスの削除）。
func (h *Hooks) RegisterOnDeviceRevoked(fn func(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error) {
	h.open()
	h.deviceRevoked = append(h.deviceRevoked, fn)
}

// RegisterOnUserSuspended は構成員の停止の hook を足す。reason は RevokeSuspended（Users.Suspend）か RevokeDeleted（Users.Delete が
// 停止の処理を通る時）。srv-rooms は suspended の時だけ家族全員のトークに知らせを流す。
func (h *Hooks) RegisterOnUserSuspended(fn func(ctx context.Context, tx *store.Tx, userId uuid.UUID, reason RevokeReason) error) {
	h.open()
	h.userSuspended = append(h.userSuspended, fn)
}

// RegisterOnUserUnsuspended は構成員の再開の hook を足す（Users.Unsuspend が状態を normal にした後。srv-rooms が家族全員のトークに
// 再開の知らせを流す）。
func (h *Hooks) RegisterOnUserUnsuspended(fn func(ctx context.Context, tx *store.Tx, userId uuid.UUID) error) {
	h.open()
	h.userUnsuspended = append(h.userUnsuspended, fn)
}

// RegisterOnUserDeleted は構成員の削除の hook を足す（srv-rooms の削除済みの印と leaf の削除待ち）。
func (h *Hooks) RegisterOnUserDeleted(fn func(ctx context.Context, tx *store.Tx, userId uuid.UUID, eraseContent bool) error) {
	h.open()
	h.userDeleted = append(h.userDeleted, fn)
}

// RegisterOnUserErased は抹消の hook を足す（srv-keys の鍵の破棄）。
func (h *Hooks) RegisterOnUserErased(fn func(ctx context.Context, tx *store.Tx, userId uuid.UUID) error) {
	h.open()
	h.userErased = append(h.userErased, fn)
}

// RegisterOnUserCreated は構成員の作成の hook を足す。最初の端末の登録の後に呼ぶ（srv-rooms が家族全員のトークを作るか、追加待ちに置く）。
func (h *Hooks) RegisterOnUserCreated(fn func(ctx context.Context, tx *store.Tx, userId, firstDeviceId uuid.UUID) error) {
	h.open()
	h.userCreated = append(h.userCreated, fn)
}

// RegisterOnNotice は知らせの hook を足す（srv-push Notices.Emit）。
func (h *Hooks) RegisterOnNotice(fn func(ctx context.Context, tx *store.Tx, n Notice) error) {
	h.open()
	h.notice = append(h.notice, fn)
}

// RegisterOnPasskeyDeleted はパスキーの削除の hook を足す（srv-keys がその資格情報のコピー (e) を消す。第12章 5.4 (e)）。
func (h *Hooks) RegisterOnPasskeyDeleted(fn func(ctx context.Context, tx *store.Tx, userId uuid.UUID, credId []byte) error) {
	h.open()
	h.passkeyDeleted = append(h.passkeyDeleted, fn)
}

// RegisterOnPasskeyReset は管理者の再設定のリンクの使用の hook を足す（srv-keys が recovery.used（method reset）を
// 本人の全端末と管理者へ書く。第3章 4.6）。
func (h *Hooks) RegisterOnPasskeyReset(fn func(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID) error) {
	h.open()
	h.passkeyReset = append(h.passkeyReset, fn)
}

// Notify は知らせを OnNotice の登録へ渡す公開の口（srv-push より下のブロックが使う。srv-keys の recovery_used、
// family_key_used、recovery_requested、mls_reinit、mls_reinit_limit、sticker_remove_requested）。登録が無ければ Fault（知らせを黙って捨てない。Identity.Ready が起動の時に確かめる）。
func (h *Hooks) Notify(ctx context.Context, tx *store.Tx, n Notice) error {
	if len(h.notice) == 0 {
		return fault.Wrap(errNoNoticeHook)
	}
	return h.onNotice(ctx, tx, n)
}

// errNoNoticeHook は OnNotice の登録が無い（起動の組み立ての誤り）。
var errNoNoticeHook = errors.New("identity: OnNotice の登録が無い")

// errNoBeforeHook は OnBeforeUserChange か OnBeforeDeviceRevoke の登録が無い（トークの錠の順が守れない組み立て。I-18）。
var errNoBeforeHook = errors.New("identity: OnBeforeUserChange か OnBeforeDeviceRevoke の登録が無い")

// fire は登録の順に呼び、最初の誤りを返す。
func fire[F any](fns []F, call func(F) error) error {
	for _, fn := range fns {
		if err := call(fn); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hooks) onBeforeUserChange(ctx context.Context, tx *store.Tx, userId uuid.UUID, deviceIds []uuid.UUID) error {
	return fire(h.beforeUserChange, func(fn func(context.Context, *store.Tx, uuid.UUID, []uuid.UUID) error) error {
		return fn(ctx, tx, userId, deviceIds)
	})
}

func (h *Hooks) onBeforeDeviceRevoke(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	return fire(h.beforeDeviceRevoke, func(fn func(context.Context, *store.Tx, uuid.UUID) error) error { return fn(ctx, tx, deviceId) })
}

func (h *Hooks) onDeviceRevoked(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	return fire(h.deviceRevoked, func(fn func(context.Context, *store.Tx, uuid.UUID) error) error { return fn(ctx, tx, deviceId) })
}

func (h *Hooks) onUserSuspended(ctx context.Context, tx *store.Tx, userId uuid.UUID, reason RevokeReason) error {
	return fire(h.userSuspended, func(fn func(context.Context, *store.Tx, uuid.UUID, RevokeReason) error) error {
		return fn(ctx, tx, userId, reason)
	})
}

func (h *Hooks) onUserUnsuspended(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	return fire(h.userUnsuspended, func(fn func(context.Context, *store.Tx, uuid.UUID) error) error { return fn(ctx, tx, userId) })
}

func (h *Hooks) onUserDeleted(ctx context.Context, tx *store.Tx, userId uuid.UUID, eraseContent bool) error {
	return fire(h.userDeleted, func(fn func(context.Context, *store.Tx, uuid.UUID, bool) error) error {
		return fn(ctx, tx, userId, eraseContent)
	})
}

func (h *Hooks) onUserErased(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	return fire(h.userErased, func(fn func(context.Context, *store.Tx, uuid.UUID) error) error { return fn(ctx, tx, userId) })
}

func (h *Hooks) onUserCreated(ctx context.Context, tx *store.Tx, userId, firstDeviceId uuid.UUID) error {
	return fire(h.userCreated, func(fn func(context.Context, *store.Tx, uuid.UUID, uuid.UUID) error) error {
		return fn(ctx, tx, userId, firstDeviceId)
	})
}

func (h *Hooks) onNotice(ctx context.Context, tx *store.Tx, n Notice) error {
	return fire(h.notice, func(fn func(context.Context, *store.Tx, Notice) error) error { return fn(ctx, tx, n) })
}

func (h *Hooks) onPasskeyReset(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID) error {
	return fire(h.passkeyReset, func(fn func(context.Context, *store.Tx, uuid.UUID, uuid.UUID) error) error {
		return fn(ctx, tx, userId, deviceId)
	})
}

func (h *Hooks) onPasskeyDeleted(ctx context.Context, tx *store.Tx, userId uuid.UUID, credId []byte) error {
	return fire(h.passkeyDeleted, func(fn func(context.Context, *store.Tx, uuid.UUID, []byte) error) error {
		return fn(ctx, tx, userId, credId)
	})
}
