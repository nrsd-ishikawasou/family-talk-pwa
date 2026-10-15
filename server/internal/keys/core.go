// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// reauthWindow は再認証の有効の時間（第3章 4.5。srv-identity と同じ値）。
const reauthWindow = 5 * time.Minute

// 有効期間の決まった値（第12章 5.2「leaf の有効期間」、DD-12-18）。秒。
const (
	day               = 24 * 60 * 60
	lifetimeMaxTotal  = 120 * day // 全長の上限
	lifetimeSkew      = 1 * day   // 置く時の not_before と、Add の時の not_before の余裕
	lifetimeMinRemain = 7 * day   // 置く時の not_after の残り
)

// wrap は DB と部品の誤りを Fault にする。既に Event か Fault ならそのまま返す。
func wrap(err error) error {
	if err == nil {
		return nil
	}
	var e *fault.Event
	var f *fault.Fault
	if errors.As(err, &e) || errors.As(err, &f) {
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

// isEvent は予期した失敗（エラーコード表の番号）か。
func isEvent(err error, code string) bool {
	var e *fault.Event
	return errors.As(err, &e) && e.Code == code
}

// inTx は ctx のトランザクション（冪等の middleware）があればそれに乗り、無ければ開いて fn を走らせる。
func (d *Deps) inTx(ctx context.Context, fn func(tx *store.Tx) error) error {
	return d.Pool.InTx(ctx, fn)
}

// separateTx は要求のトランザクションの外の新しいトランザクションで fn を確定する（誤りの応答の前に残す記録。要求の
// トランザクションは誤りで戻る）。fn の中では、要求のトランザクションが錠を持つ行を書かず、外部キーで指さない。
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

// reauthed は主体の再認証が 5 分以内か（判定は業務。srv-identity と同じ）。
func (d *Deps) reauthed(p boundary.Principal) bool {
	return p.ReauthAt != nil && d.now().Sub(*p.ReauthAt) <= reauthWindow
}

// emit は 1 件のイベントを宛先の構成員ごとの feed に同じトランザクションで溜める（全体実装設計書 2.4）。宛先の重複を除き
// 識別子の順に並べる。room は room.* の種類のトーク、device は device.inbox の宛先の端末（他は uuid.Nil）。
func (d *Deps) emit(tx *store.Tx, users []uuid.UUID, kind string, room, device uuid.UUID, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return wrap(err)
	}
	list := sortedIds(users)
	id := d.Ids.NewV7()
	ts := d.now()
	for _, u := range list {
		d.Feed.Append(tx, u, store.FeedRef{Kind: kind, Id: id, RoomId: room, DeviceId: device, Payload: body, Ts: ts})
	}
	return nil
}

// sortedIds は識別子の重複を除き、バイトの順（UUID の文字列の順と同じ）に並べた写し。
func sortedIds(ids []uuid.UUID) []uuid.UUID {
	list := slices.Clone(ids)
	slices.SortFunc(list, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(list)
}

// b64 は base64url（RFC 4648 の詰め物なし。末尾のビットが 0 でない形は拒む）。
var b64 = base64.RawURLEncoding.Strict()

// decodeB64 は base64url を解く。解けなければ偽。
func decodeB64(s string) ([]byte, bool) {
	v, err := b64.DecodeString(s)
	return v, err == nil
}

// jwkPoint は JWK の P-256 の公開鍵（crv P-256、kty EC、x と y が 32 バイト）を SEC 1 の非圧縮の 65 バイトと公開鍵にする。
// 曲線の上の点で無限遠点でないことは crypto/ecdsa が確かめる。どれかが違えば偽。
func jwkPoint(j gen.CommonJwkP2561) ([]byte, *ecdsa.PublicKey, bool) {
	if crv, _ := j.Crv.(string); crv != "P-256" {
		return nil, nil, false
	}
	if kty, _ := j.Kty.(string); kty != "EC" {
		return nil, nil, false
	}
	x, okx := decodeB64(j.X)
	y, oky := decodeB64(j.Y)
	if !okx || !oky || len(x) != 32 || len(y) != 32 {
		return nil, nil, false
	}
	sec1 := append(append([]byte{4}, x...), y...)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), sec1)
	if err != nil {
		return nil, nil, false
	}
	return sec1, pub, true
}

// canonicalJwk は点から組み直した正規の形の JWK（同じ鍵が同じ文字列になり、比べと一意の索引が効く）。
func canonicalJwk(sec1 []byte) gen.CommonJwkP2561 {
	return gen.CommonJwkP2561{Crv: "P-256", Kty: "EC", X: b64.EncodeToString(sec1[1:33]), Y: b64.EncodeToString(sec1[33:65])}
}

// verifyRS は本システムの署名（device_sig、user_sig、メールボックス）：ECDSA P-256、SHA-256、r と s の 64 バイト（第12章 5.3）。
func verifyRS(pub *ecdsa.PublicKey, message, sig []byte) bool {
	if pub == nil || len(sig) != 64 {
		return false
	}
	h := sha256.Sum256(message)
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, h[:], r, s)
}

// credentialFor は第12章 5.3 の MLS の credential（basic、identity は本人と端末の識別子の 16 バイトずつ。35 バイト）。
func credentialFor(userId, deviceId uuid.UUID) []byte {
	out := []byte{0x00, 0x01, 0x20}
	out = append(out, userId.Bytes()...)
	return append(out, deviceId.Bytes()...)
}

// userSigMessage は user_sig の署名の対象（第12章 5.3。179 バイト）。
func userSigMessage(userId, deviceId uuid.UUID, sigPub, kemPub []byte) []byte {
	out := append([]byte("ft.device_cert/1"), 0)
	out = append(out, userId.Bytes()...)
	out = append(out, deviceId.Bytes()...)
	out = append(out, sigPub...)
	return append(out, kemPub...)
}

// inboxMessage はメールボックスの署名の対象（第12章 5.2）。
func inboxMessage(kind InboxKind, from, to uuid.UUID, ciphertext []byte) []byte {
	out := append([]byte("ft.inbox/1"), 0)
	out = append(out, kind...)
	out = append(out, 0)
	out = append(out, from.Bytes()...)
	out = append(out, to.Bytes()...)
	return append(out, ciphertext...)
}

// groupIdOf はグループの識別子（トークの識別子の 16 バイトと世代の 4 バイト。ビッグエンディアン。第12章 5.2）。
func groupIdOf(roomId uuid.UUID, generation int) []byte {
	return binary.BigEndian.AppendUint32(append([]byte(nil), roomId.Bytes()...), uint32(generation))
}

// unixSeconds は時刻の秒（有効期間と比べる。今の時刻は 1970 年より後）。
func unixSeconds(t time.Time) uint64 {
	return uint64(t.Unix())
}

// lifetimeOk は leaf の有効期間（第12章 5.2「leaf の有効期間」、DD-12-18）。全長が 120 日以下（not_after >= not_before）。forPush は
// 置く時で、not_before が今 + 1 日より前、not_after が今 + 7 日より後。偽は Commit の Add で、not_before − 1 日 <= 今 <= not_after。
func lifetimeOk(l ParsedLeafNode, now time.Time, forPush bool) bool {
	if l.NotAfter < l.NotBefore || l.NotAfter-l.NotBefore > lifetimeMaxTotal {
		return false
	}
	n := unixSeconds(now)
	if forPush {
		return l.NotBefore < n+lifetimeSkew && l.NotAfter > n+lifetimeMinRemain
	}
	return l.NotBefore <= n+lifetimeSkew && n <= l.NotAfter
}

// defaultExtension は RFC 9420 7.2 の既定の拡張（application_id、ratchet_tree、required_capabilities、external_pub、external_senders）。
func defaultExtension(t int) bool {
	return t >= 1 && t <= 5
}

// capabilitiesOk は第12章 5.2「KeyPackage の保管」の 8：capabilities に版 mls10、スイート 0x0002、credential の basic があり、
// leaf の extensions のうち既定でない種類が全部 capabilities の extensions にある（RFC 9420 7.2）。
func capabilitiesOk(l ParsedLeafNode) bool {
	c := l.Capabilities
	if !slices.Contains(c.Versions, mlsVersion10) || !slices.Contains(c.Suites, mlsSuite) || !slices.Contains(c.Credentials, credentialBasic) {
		return false
	}
	for _, t := range l.extTypes {
		if !defaultExtension(t) && !slices.Contains(c.Extensions, t) {
			return false
		}
	}
	return true
}

// registration は端末の鍵の登録（device_keys の 1 行の、確かめに使う欄）。
type registration struct {
	userId     uuid.UUID
	credential []byte
	sigSec1    []byte
	sigPub     *ecdsa.PublicKey
}

// checkKeyPackage は第12章 5.2「KeyPackage の保管」の 9 つ（forPush でない時は Commit の Add の確かめ 4 で、有効期間は今の時刻の
// 確かめになる）。KeyPackages.Push と Groups.Commit が使う。
func checkKeyPackage(kp ParsedKeyPackage, reg registration, now time.Time, forPush bool) bool {
	l := kp.Leaf
	return kp.Version == mlsVersion10 && kp.Suite == mlsSuite && // 1
		bytes.Equal(l.Credential, reg.credential) && // 2
		bytes.Equal(l.SigPub, reg.sigSec1) && // 3
		l.Source == leafSourceKeyPackage && // 4
		kp.VerifySignature(reg.sigPub) && // 5
		l.VerifySignature(reg.sigPub, nil, 0) && // 6
		!bytes.Equal(l.EncryptionKey, kp.InitKey) && // 7
		capabilitiesOk(l) && // 8
		lifetimeOk(l, now, forPush) // 9
}

// readRegistration は device_keys の 1 行を読む（lock なら FOR UPDATE）。無ければ ok は偽。
func readRegistration(ctx context.Context, q store.Querier, deviceId uuid.UUID, lock bool) (registration, bool, error) {
	sql := `SELECT user_id, sig_pub, credential FROM device_keys WHERE device_id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var reg registration
	var sig gen.CommonJwkP2561
	err := q.QueryRow(ctx, sql, deviceId).Scan(&reg.userId, &sig, &reg.credential)
	if noRows(err) {
		return registration{}, false, nil
	}
	if err != nil {
		return registration{}, false, wrap(err)
	}
	sec1, pub, ok := jwkPoint(sig)
	if !ok {
		return registration{}, false, fault.Wrap(errors.New("keys: 登録した署名鍵が P-256 の点でない"))
	}
	reg.sigSec1, reg.sigPub = sec1, pub
	return reg, true, nil
}

// errNet001 は要求の形の誤り（NET-001。scope global）。path は欄の位置。
func errNet001(path, message string) error {
	return fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": path, "message": message}}})
}
