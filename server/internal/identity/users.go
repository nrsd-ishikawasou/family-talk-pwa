// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/envelope"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// User は構成員の 1 行（users）。表示名は平文、誕生日などは UserPrivate のエンベロープ（第3章 6）。
type User struct {
	Id uuid.UUID
	// DisplayName は 20 文字（書記素）。
	DisplayName string
	Role        boundary.Role
	State       UserState
	IsChild     bool
	// ChildDeclared は申告（誕生日があればそちらが正）。
	ChildDeclared bool
	CreatedAt     time.Time
	// DekId は user_dek の鍵の識別子。
	DekId uuid.UUID
}

// UserPrivate は本人の DEK のエンベロープの欄（user_private）。
type UserPrivate struct {
	// Birthday は年を任意とする誕生日。
	Birthday *envelope.Fenv
	// Aliases は {user_id: 表示名} の対応。
	Aliases envelope.Fenv
	// MutedUsers は [user_id]。
	MutedUsers envelope.Fenv
	// Settings は ft.settings.user/1 のうちエンベロープに置く項目（プライバシー）。
	Settings envelope.Fenv
}

// ChildSpec は子ども区分の作成の指定（招待の child）。
type ChildSpec struct {
	IsChild   bool
	Guardians []uuid.UUID
}

// Transition は 16 歳・18 歳の解除の 1 件。
type Transition struct {
	UserId uuid.UUID
	Age    int
}

// Users は構成員を持つ（B-061）。
type Users struct {
	d         *Deps
	hooks     *Hooks
	families  *Families
	sessions  *Sessions
	devices   *Devices
	guardians *Guardians
	keys      userKeys
}

// userCols は User を読む列。
const userCols = `id, display_name, role, state, is_child, child_declared, created_at, dek_id`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var role, state string
	err := row.Scan(&u.Id, &u.DisplayName, &role, &state, &u.IsChild, &u.ChildDeclared, &u.CreatedAt, &u.DekId)
	u.Role, u.State, u.CreatedAt = boundary.Role(role), UserState(state), u.CreatedAt.UTC()
	return u, err
}

// Create は構成員を作る。DEK を生成し KEK で包む（Envelope）。WebAuthn の user handle は新しい乱数。
// hook OnUserCreated は最初の端末の登録の後に呼ぶ側が呼ぶ（Invites.Accept、SetupLink.Consume）。
func (u *Users) Create(ctx context.Context, tx *store.Tx, name string, role boundary.Role, child *ChildSpec) (User, error) {
	return u.create(ctx, tx, u.d.Ids.NewToken(), name, role, child, uuid.Nil)
}

// create は user handle（WebAuthn の登録の options の時に決めた値）と保護者の組を置いた人を受けて作る。
func (u *Users) create(ctx context.Context, tx *store.Tx, handle []byte, name string, role boundary.Role, child *ChildSpec, setBy uuid.UUID) (User, error) {
	if e := checkName(name, nameMax); e != nil {
		return User{}, e
	}
	usr := User{Id: u.d.Ids.NewV7(), DisplayName: name, Role: role, State: UserNormal, CreatedAt: u.d.now(), DekId: u.d.Ids.NewV7()}
	if child != nil && child.IsChild {
		usr.IsChild, usr.ChildDeclared = true, true
	}
	_, err := tx.Exec(ctx, `INSERT INTO users (id, webauthn_handle, display_name, role, state, is_child, child_declared, created_at, dek_id)
VALUES ($1, $2, $3, $4, 'normal', $5, $6, $7, $8)`, usr.Id, handle, usr.DisplayName, string(usr.Role), usr.IsChild, usr.ChildDeclared, usr.CreatedAt, usr.DekId)
	if err != nil {
		return User{}, wrap(err)
	}
	if err := u.keys.create(ctx, tx, usr.Id, usr.DekId); err != nil {
		return User{}, err
	}
	var aliases, muted, settings []byte
	err = u.keys.with(ctx, tx, usr.Id, func(dek []byte, dekId uuid.UUID) error {
		var err error
		if aliases, err = sealWith(u.d, dek, dekId, []byte(`{}`), aad("user_private", "aliases", usr.Id.String())); err != nil {
			return err
		}
		if muted, err = sealWith(u.d, dek, dekId, []byte(`[]`), aad("user_private", "muted_users", usr.Id.String())); err != nil {
			return err
		}
		settings, err = sealWith(u.d, dek, dekId, []byte(`{}`), aad("user_private", "settings", usr.Id.String()))
		return err
	})
	if err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_private (user_id, aliases, muted_users, settings) VALUES ($1, $2, $3, $4)`,
		usr.Id, aliases, muted, settings); err != nil {
		return User{}, wrap(err)
	}
	if usr.IsChild {
		for _, g := range child.Guardians {
			if _, err := tx.Exec(ctx, `INSERT INTO guardians (child_id, guardian_id, set_by, set_at) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
				usr.Id, g, setBy, usr.CreatedAt); err != nil {
				return User{}, wrap(err)
			}
		}
	}
	return usr, nil
}

// Get は構成員を返す。無ければ MEM-001。q は ctx のトランザクションか Pool（トランザクションの中ではそれを渡す。全体実装設計書 2.4）。
func (u *Users) Get(ctx context.Context, q store.Querier, id uuid.UUID) (User, error) {
	return u.get(ctx, q, id, false)
}

// get は 1 行を読む。lock なら FOR NO KEY UPDATE（キーの列は書かないので足り、外部キーで指す INSERT の FOR KEY SHARE を
// 止めない。別のトランザクションで構成員を指す行を書いても待ち合わない）。
func (u *Users) get(ctx context.Context, q querier, id uuid.UUID, lock bool) (User, error) {
	sql := `SELECT ` + userCols + ` FROM users WHERE id = $1`
	if lock {
		sql += ` FOR NO KEY UPDATE`
	}
	usr, err := scanUser(q.QueryRow(ctx, sql, id))
	if noRows(err) {
		return User{}, ev(cgen.MEM001)
	}
	return usr, wrap(err)
}

// inFamily は家族の構成員（削除と抹消を含めない。第2章 5.3）。
func (s UserState) inFamily() bool {
	return s == UserNormal || s == UserSuspended || s == UserErasePending
}

// viewer は一覧を見る人（子ども区分を見せる範囲の判定。第3章 6：本人と保護者と管理者）。
type viewer struct {
	id    uuid.UUID
	admin bool
	wards map[uuid.UUID]bool
}

// viewerOf は主体から作る。q は ctx のトランザクションか Pool。
func (u *Users) viewerOf(ctx context.Context, q querier, p boundary.Principal) (viewer, error) {
	me, _, err := self(p)
	if err != nil {
		return viewer{}, err
	}
	v := viewer{id: me, admin: p.Role == boundary.RoleAdmin, wards: map[uuid.UUID]bool{}}
	kids, err := uuids(ctx, q, `SELECT child_id FROM guardians WHERE guardian_id = $1`, me)
	for _, k := range kids {
		v.wards[k] = true
	}
	return v, err
}

// toGen は User を応答の形にする（子ども区分は見せてよい人にだけ）。
func (v viewer) toGen(usr User, sig *gen.Fusr1) gen.User {
	out := gen.User{Id: usr.Id.String(), DisplayName: usr.DisplayName, Role: gen.UserRole(usr.Role), State: gen.UserState(usr.State), UserSigPub: sig}
	if v.admin || v.id == usr.Id || v.wards[usr.Id] {
		c := usr.IsChild
		out.Child = &c
	}
	return out
}

// List は /users（識別子、表示名、役割、状態、子ども区分、本人の署名鍵の公開部）。写真はサーバーに無い（第3章 6）。
// q は ctx のトランザクションか Pool（/sync の初回の一覧は srv-store Pool.BeginSnapshot の中で読む）。
func (u *Users) List(ctx context.Context, q store.Querier, p boundary.Principal, page Page) ([]gen.User, bool, error) {
	v, err := u.viewerOf(ctx, q, p)
	if err != nil {
		return nil, false, err
	}
	c, err := page.uuids()
	if err != nil {
		return nil, false, err
	}
	sql := `SELECT ` + userCols + ` FROM users WHERE state IN ('normal', 'suspended', 'erase_pending')`
	args := []any{}
	sql, args = whereUUID(c, sql, "id", args)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, wrap(err)
	}
	defer rows.Close()
	var list []User
	for rows.Next() {
		usr, err := scanUser(rows)
		if err != nil {
			return nil, false, wrap(err)
		}
		list = append(list, usr)
	}
	if err := rows.Err(); err != nil {
		return nil, false, wrap(err)
	}
	list, more := finish(list, c)
	ids := make([]uuid.UUID, len(list))
	for i, usr := range list {
		ids[i] = usr.Id
	}
	sigs, err := u.d.Keys.SigningKeys(ctx, q, ids)
	if err != nil {
		return nil, false, wrap(err)
	}
	out := make([]gen.User, len(list))
	for i, usr := range list {
		var sig *gen.Fusr1
		if k, ok := sigs[usr.Id]; ok {
			sig = &k
		}
		out[i] = v.toGen(usr, sig)
	}
	return out, more, nil
}

// Detail は /users/{id}（構成員と承認済みの端末の公開鍵）。削除と抹消は MEM-001（家族の構成員でない）。
func (u *Users) Detail(ctx context.Context, p boundary.Principal, id uuid.UUID) (gen.UserDetail, error) {
	v, err := u.viewerOf(ctx, u.d.Pool, p)
	if err != nil {
		return gen.UserDetail{}, err
	}
	usr, err := u.Get(ctx, u.d.Pool, id)
	if err != nil {
		return gen.UserDetail{}, err
	}
	if !usr.State.inFamily() {
		return gen.UserDetail{}, ev(cgen.MEM001)
	}
	sigs, err := u.d.Keys.SigningKeys(ctx, u.d.Pool, []uuid.UUID{id})
	if err != nil {
		return gen.UserDetail{}, wrap(err)
	}
	var sig *gen.Fusr1
	if k, ok := sigs[id]; ok {
		sig = &k
	}
	devs, err := u.d.Keys.DevicesOf(ctx, u.d.Pool, id)
	if err != nil {
		return gen.UserDetail{}, wrap(err)
	}
	if devs == nil {
		devs = []gen.Fdev1{}
	}
	return gen.UserDetail{User: v.toGen(usr, sig), Devices: devs}, nil
}

// birthdayRe は誕生日の形（YYYY-MM-DD か MM-DD。第2章 5.3 の updateMe）。
var birthdayRe = regexp.MustCompile(`^(?:(\d{4})-)?(\d{2})-(\d{2})$`)

// birthday は誕生日（Year は 0 なら不明）。
type birthday struct {
	Year, Month, Day int
}

// parseBirthday は形と暦の確かめ（2 月 29 日は年が無いか閏年だけ）。違えば MEM-005。
func parseBirthday(s string) (birthday, *fault.Event) {
	m := birthdayRe.FindStringSubmatch(s)
	if m == nil {
		return birthday{}, ev(cgen.MEM005)
	}
	var b birthday
	if m[1] != "" {
		b.Year, _ = strconv.Atoi(m[1])
	}
	b.Month, _ = strconv.Atoi(m[2])
	b.Day, _ = strconv.Atoi(m[3])
	y := b.Year
	if y == 0 {
		y = 2000 // 閏年（2 月 29 日を許す）
	}
	t := time.Date(y, time.Month(b.Month), b.Day, 0, 0, 0, 0, time.UTC)
	if b.Month < 1 || b.Month > 12 || t.Month() != time.Month(b.Month) || t.Day() != b.Day || (b.Year != 0 && b.Year < 1900) {
		return birthday{}, ev(cgen.MEM005)
	}
	return b, nil
}

func (b birthday) String() string {
	md := twoDigits(b.Month) + "-" + twoDigits(b.Day)
	if b.Year == 0 {
		return md
	}
	return strconv.Itoa(b.Year) + "-" + md
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// ageOn は today の時点の満年齢（年が無ければ -1。2 月 29 日生まれは平年の 2 月 28 日に年を取る）。
func (b birthday) ageOn(today time.Time) int {
	if b.Year == 0 {
		return -1
	}
	m, d := b.Month, b.Day
	if m == 2 && d == 29 && !isLeap(today.Year()) {
		d = 28
	}
	age := today.Year() - b.Year
	if int(today.Month()) < m || (int(today.Month()) == m && today.Day() < d) {
		age--
	}
	return age
}

// isToday は今日が誕生日か（2 月 29 日生まれは平年の 2 月 28 日）。
func (b birthday) isToday(today time.Time) bool {
	m, d := b.Month, b.Day
	if m == 2 && d == 29 && !isLeap(today.Year()) {
		d = 28
	}
	return int(today.Month()) == m && today.Day() == d
}

// birthdayIn は year 年の誕生日の 0 時（2 月 29 日生まれは平年の 2 月 28 日。ageOn と同じ扱い）。
func (b birthday) birthdayIn(year int, loc *time.Location) time.Time {
	d := b.Day
	if b.Month == 2 && d == 29 && !isLeap(year) {
		d = 28
	}
	return time.Date(year, time.Month(b.Month), d, 0, 0, 0, 0, loc)
}

func isLeap(y int) bool {
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

// readBirthday は本人の誕生日を開く。無ければ nil。
func (u *Users) readBirthday(ctx context.Context, q querier, id uuid.UUID) (*birthday, error) {
	var blob []byte
	if err := q.QueryRow(ctx, `SELECT birthday FROM user_private WHERE user_id = $1`, id).Scan(&blob); err != nil {
		if noRows(err) {
			return nil, nil
		}
		return nil, wrap(err)
	}
	if blob == nil {
		return nil, nil
	}
	pt, err := u.keys.open(ctx, q, id, blob, aad("user_private", "birthday", id.String()))
	if err != nil {
		return nil, err
	}
	b, e := parseBirthday(string(pt))
	if e != nil {
		return nil, fault.Wrap(e)
	}
	return &b, nil
}

// Me は /me の応答（誕生日を開いて返す）。
func (u *Users) Me(ctx context.Context, id uuid.UUID) (gen.GetMeResponse, error) {
	usr, err := u.Get(ctx, u.d.Pool, id)
	if err != nil {
		return gen.GetMeResponse{}, err
	}
	sigs, err := u.d.Keys.SigningKeys(ctx, u.d.Pool, []uuid.UUID{id})
	if err != nil {
		return gen.GetMeResponse{}, wrap(err)
	}
	var sig *gen.Fusr1
	if k, ok := sigs[id]; ok {
		sig = &k
	}
	out := gen.GetMeResponse{User: viewer{id: id}.toGen(usr, sig), Lang: "ja"}
	b, err := u.readBirthday(ctx, u.d.Pool, id)
	if err != nil {
		return gen.GetMeResponse{}, err
	}
	if b != nil {
		s := b.String()
		out.Birthday = &s
	}
	return out, nil
}

// UpdateProfile は /me PATCH。欄は表示名（平文。user.changed）と誕生日（エンベロープ）だけ。
// 子ども区分の構成員は自分の誕生日を書けない（MEM-007。保護者が Guardians.SetBirthday で書く。第3章 9.1）。誕生日で区分は
// 変えない。区分の無い構成員の誕生日で今日 18 歳未満になる時は管理者に知らせる（child_flag_suggested）。
func (u *Users) UpdateProfile(ctx context.Context, tx *store.Tx, id uuid.UUID, patch gen.UpdateMeRequest) error {
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if patch.DisplayName != nil && *patch.DisplayName != usr.DisplayName {
		if e := checkName(*patch.DisplayName, nameMax); e != nil {
			return e
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET display_name = $1 WHERE id = $2`, *patch.DisplayName, id); err != nil {
			return wrap(err)
		}
		if err := u.d.emitUserChanged(ctx, tx, id, fieldDisplayName); err != nil {
			return err
		}
	}
	if patch.Birthday != nil {
		if usr.IsChild {
			return ev(cgen.MEM007)
		}
		b, e := parseBirthday(*patch.Birthday)
		if e != nil {
			return e
		}
		if b.Year != 0 && b.ageOn(u.d.now()) < 0 {
			return ev(cgen.MEM005)
		}
		if err := u.writeBirthday(ctx, tx, id, b); err != nil {
			return err
		}
		if age := b.ageOn(u.d.now()); age >= 0 && age < childAge {
			return u.hooks.notifyAdmins(ctx, tx, noticeChildFlagSuggested, noticeKey(id.String()), map[string]any{"user_id": id.String(), "name": usr.DisplayName})
		}
	}
	return nil
}

// writeBirthday は誕生日をエンベロープで user_private.birthday に置く。
func (u *Users) writeBirthday(ctx context.Context, tx *store.Tx, id uuid.UUID, b birthday) error {
	blob, err := u.keys.seal(ctx, tx, id, []byte(b.String()), aad("user_private", "birthday", id.String()))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE user_private SET birthday = $1 WHERE user_id = $2`, blob, id)
	return wrap(err)
}

// SetAlias は本人だけに見える別名を置く。name が nil なら消す（/me/aliases/{user_id}）。20 書記素（MEM-005）。
// 相手が家族にいなければ MEM-001（抹消済みは除く。削除された人の別名は残してよい）。
func (u *Users) SetAlias(ctx context.Context, tx *store.Tx, userId, targetId uuid.UUID, name *string) error {
	if name != nil {
		if e := checkName(*name, nameMax); e != nil {
			return e
		}
		var st string
		err := tx.QueryRow(ctx, `SELECT state FROM users WHERE id = $1`, targetId).Scan(&st)
		if noRows(err) || UserState(st) == UserErased {
			return ev(cgen.MEM001)
		}
		if err != nil {
			return wrap(err)
		}
	}
	return u.rewrite(ctx, tx, userId, "aliases", func(raw []byte) ([]byte, error) {
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, wrap(err)
		}
		if name == nil {
			delete(m, targetId.String())
		} else {
			m[targetId.String()] = *name
		}
		return json.Marshal(m)
	})
}

// rewrite は user_private の 1 列を開いて書き換えて封じ直す（行の錠の下）。
func (u *Users) rewrite(ctx context.Context, tx *store.Tx, userId uuid.UUID, column string, fn func(raw []byte) ([]byte, error)) error {
	var blob []byte
	if err := tx.QueryRow(ctx, `SELECT `+column+` FROM user_private WHERE user_id = $1 FOR UPDATE`, userId).Scan(&blob); err != nil {
		return wrap(err)
	}
	adata := aad("user_private", column, userId.String())
	return u.keys.with(ctx, tx, userId, func(dek []byte, dekId uuid.UUID) error {
		raw, err := openWith(u.d, dek, dekId, blob, adata)
		if err != nil {
			return err
		}
		next, err := fn(raw)
		if err != nil {
			return wrap(err)
		}
		sealed, err := sealWith(u.d, dek, dekId, next, adata)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE user_private SET `+column+` = $1 WHERE user_id = $2`, sealed, userId)
		return wrap(err)
	})
}

// settingsDefaults は ft.settings.user/1 の既定値（config.yaml の構成員ごとの項目。生成の gen.Items）。
func settingsDefaults(prefix string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, it := range cgen.Items {
		if name, ok := strings.CutPrefix(it.Key, prefix); ok {
			out[name] = json.RawMessage(it.DefaultJSON)
		}
	}
	return out
}

// Settings は /me/settings。エンベロープを開き、既定値に重ねる（srv-push の規則の判定もこれを呼ぶ）。
// muted_users は自分の列（user_private.muted_users）に置き、他の項目は settings の列に置く。
func (u *Users) Settings(ctx context.Context, userId uuid.UUID) (gen.FtSettingsUser1, error) {
	var sBlob, mBlob []byte
	if err := u.d.Pool.QueryRow(ctx, `SELECT settings, muted_users FROM user_private WHERE user_id = $1`, userId).Scan(&sBlob, &mBlob); err != nil {
		if noRows(err) {
			return gen.FtSettingsUser1{}, ev(cgen.MEM001)
		}
		return gen.FtSettingsUser1{}, wrap(err)
	}
	merged := settingsDefaults("user.")
	err := u.keys.with(ctx, u.d.Pool, userId, func(dek []byte, dekId uuid.UUID) error {
		raw, err := openWith(u.d, dek, dekId, sBlob, aad("user_private", "settings", userId.String()))
		if err != nil {
			return err
		}
		stored := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &stored); err != nil {
			return wrap(err)
		}
		for k, v := range stored {
			merged[k] = v
		}
		muted, err := openWith(u.d, dek, dekId, mBlob, aad("user_private", "muted_users", userId.String()))
		if err != nil {
			return err
		}
		merged["muted_users"] = muted
		return nil
	})
	if err != nil {
		return gen.FtSettingsUser1{}, err
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return gen.FtSettingsUser1{}, wrap(err)
	}
	var out gen.FtSettingsUser1
	return out, wrap(json.Unmarshal(b, &out))
}

// PutSettings は /me/settings PUT。形の検証は srv-boundary の要求の検証（ft.settings.user/1）で済んでいる。
// 与えた項目だけを置き換える（与えない項目は今の値のまま）。
func (u *Users) PutSettings(ctx context.Context, tx *store.Tx, userId uuid.UUID, s gen.FtSettingsUser1) error {
	b, err := json.Marshal(s)
	if err != nil {
		return wrap(err)
	}
	patch := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &patch); err != nil {
		return wrap(err)
	}
	if muted, ok := patch["muted_users"]; ok {
		delete(patch, "muted_users")
		if err := u.rewrite(ctx, tx, userId, "muted_users", func([]byte) ([]byte, error) { return muted, nil }); err != nil {
			return err
		}
	}
	if len(patch) == 0 {
		return nil
	}
	return u.rewrite(ctx, tx, userId, "settings", func(raw []byte) ([]byte, error) {
		cur := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &cur); err != nil {
			return nil, err
		}
		for k, v := range patch {
			cur[k] = v
		}
		return json.Marshal(cur)
	})
}

// lastAdmin は id が状態の通常の最後の管理者か（他に状態の通常の管理者がいない）。
func lastAdmin(ctx context.Context, q querier, usr User) (bool, error) {
	if usr.Role != boundary.RoleAdmin {
		return false, nil
	}
	var others int
	err := q.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND state = 'normal' AND id <> $1`, usr.Id).Scan(&others)
	return others == 0, wrap(err)
}

// Suspend は状態を suspended にする。Sessions.RevokeAllOfUser(suspended)、Devices.UnapproveAllOfUser、
// 本人の未使用の招待の取り消し、hook OnUserSuspended、user.changed（第3章 8.1）。最後の管理者は MEM-003。
// Families.HoldsCard なら管理者へ作り直しを勧める知らせ（第7章 9）。reason は記録しない（監査は srv-admin）。
func (u *Users) Suspend(ctx context.Context, tx *store.Tx, id, by uuid.UUID, reason string) error {
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if usr.State != UserNormal {
		return ev(cgen.MEM002)
	}
	return u.suspend(ctx, tx, usr, RevokeSuspended)
}

// suspend は停止の 2 と 3（Delete も同じ順を通る。セッションの失効の理由だけが違う）。
func (u *Users) suspend(ctx context.Context, tx *store.Tx, usr User, reason RevokeReason) error {
	last, err := lastAdmin(ctx, tx, usr)
	if err != nil {
		return err
	}
	if last {
		return ev(cgen.MEM003)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET state = 'suspended' WHERE id = $1`, usr.Id); err != nil {
		return wrap(err)
	}
	// 錠の順は構成員、端末、セッション（端末の取り消しと同じ順）。
	if err := u.devices.UnapproveAllOfUser(ctx, tx, usr.Id); err != nil {
		return err
	}
	if err := u.sessions.RevokeAllOfUser(ctx, tx, usr.Id, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET revoked_at = $2 WHERE issuer = $1 AND used_at IS NULL AND revoked_at IS NULL`, usr.Id, u.d.now()); err != nil {
		return wrap(err)
	}
	if err := u.hooks.onUserSuspended(ctx, tx, usr.Id, reason); err != nil {
		return err
	}
	if err := u.cardNotice(ctx, tx, usr); err != nil {
		return err
	}
	return u.d.emitUserChanged(ctx, tx, usr.Id, fieldState)
}

// cardNotice はカードの持ち主の停止・削除・抹消の時に管理者へ作り直しを勧める（第12章 5.4 の持ち主の変化、第7章 9）。
// params.cards は本人が持つ札の番号。
func (u *Users) cardNotice(ctx context.Context, tx *store.Tx, usr User) error {
	cards, err := u.families.held(ctx, tx, usr.Id)
	if err != nil || len(cards) == 0 {
		return err
	}
	return u.hooks.notifyAdmins(ctx, tx, noticeCardHolderChanged, noticeKey(usr.Id.String()), map[string]any{"user_id": usr.Id.String(), "name": usr.DisplayName, "cards": cards})
}

// Unsuspend は状態を normal にし、hook OnUserUnsuspended を呼ぶ。端末は未承認のまま（本人がコピー (a) で承認し直す。第12章 5.4）。
func (u *Users) Unsuspend(ctx context.Context, tx *store.Tx, id, by uuid.UUID) error {
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if usr.State != UserSuspended {
		return ev(cgen.MEM002)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET state = 'normal' WHERE id = $1`, id); err != nil {
		return wrap(err)
	}
	if err := u.hooks.onUserUnsuspended(ctx, tx, id); err != nil {
		return err
	}
	return u.d.emitUserChanged(ctx, tx, id, fieldState)
}

// Delete は停止 + hook OnUserDeleted + 状態 deleted。最後の管理者は MEM-003。状態が通常なら停止の順を先に通る
// （セッションの失効の理由は deleted。カードの持ち主への知らせは停止の中で 1 回だけ。既に停止中なら出さない）。
func (u *Users) Delete(ctx context.Context, tx *store.Tx, id, by uuid.UUID, eraseContent bool) error {
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return err
	}
	switch usr.State {
	case UserNormal:
		if err := u.suspend(ctx, tx, usr, RevokeDeleted); err != nil {
			return err
		}
	case UserSuspended:
		if last, err := lastAdmin(ctx, tx, usr); err != nil || last {
			if err != nil {
				return err
			}
			return ev(cgen.MEM003)
		}
	default:
		return ev(cgen.MEM002)
	}
	if err := u.hooks.onUserDeleted(ctx, tx, id, eraseContent); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET state = 'deleted', deleted_at = $2 WHERE id = $1`, id, u.d.now()); err != nil {
		return wrap(err)
	}
	return u.d.emitUserChanged(ctx, tx, id, fieldState)
}

// RequestErase は erase_pending にし、24 時間の仕事（erasure）を積み、猶予の終わりを返す。既に猶予の間なら今の時刻を返す（冪等）。
// 最後の管理者は MEM-003。子ども区分は保護者の承認の求めを別のトランザクションで確定してから MEM-006（誤りの応答で要求のトランザクションが戻っても求めを残す）。
func (u *Users) RequestErase(ctx context.Context, tx *store.Tx, id uuid.UUID) (time.Time, error) {
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return time.Time{}, err
	}
	if usr.State == UserErasePending {
		var at time.Time
		err := tx.QueryRow(ctx, `SELECT erase_at FROM users WHERE id = $1`, id).Scan(&at)
		return at.UTC(), wrap(err)
	}
	if usr.State != UserNormal {
		return time.Time{}, ev(cgen.MEM002)
	}
	if last, err := lastAdmin(ctx, tx, usr); err != nil || last {
		if err != nil {
			return time.Time{}, err
		}
		return time.Time{}, ev(cgen.MEM003)
	}
	if usr.IsChild {
		if err := u.d.separateTx(ctx, func(stx *store.Tx) error {
			_, err := u.guardians.RequestApproval(ctx, stx, id, ApprovalErase, ApprovalTarget{})
			return err
		}); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, ev(cgen.MEM006)
	}
	return u.startErase(ctx, tx, id)
}

// startErase は猶予を始める（RequestErase と、保護者の承認の Guardians.Approve の続き）。
func (u *Users) startErase(ctx context.Context, tx *store.Tx, id uuid.UUID) (time.Time, error) {
	at := u.d.now().Add(eraseGrace)
	if _, err := tx.Exec(ctx, `UPDATE users SET state = 'erase_pending', erase_at = $2 WHERE id = $1`, id, at); err != nil {
		return time.Time{}, wrap(err)
	}
	if _, err := u.d.Jobs.Enqueue(ctx, tx, "erasure", map[string]string{"user_id": id.String()}, at, "erasure:"+id.String()); err != nil {
		return time.Time{}, err
	}
	if err := u.d.emitUserChanged(ctx, tx, id, fieldState); err != nil {
		return time.Time{}, err
	}
	eraseAt := at.UTC().Format(time.RFC3339)
	return at, u.hooks.notifySelf(ctx, tx, id, noticeEraseRequested, noticeKey(id.String(), eraseAt), map[string]any{"erase_at": eraseAt})
}

// CancelErase は DELETE /me/erase。猶予の間なら erase_pending を外し、erasure の仕事を取り消す。全端末に知らせる。
// 求めが無ければ何もしない（DELETE の冪等。RFC 9110）。
func (u *Users) CancelErase(ctx context.Context, tx *store.Tx, id uuid.UUID) error {
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if usr.State != UserErasePending {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET state = 'normal', erase_at = NULL WHERE id = $1`, id); err != nil {
		return wrap(err)
	}
	if _, err := u.d.Jobs.Cancel(ctx, tx, "erasure:"+id.String()); err != nil {
		return err
	}
	if err := u.d.emitUserChanged(ctx, tx, id, fieldState); err != nil {
		return err
	}
	return u.hooks.notifySelf(ctx, tx, id, noticeEraseCancelled, noticeKey(id.String()), map[string]any{})
}

// Erase は srv-worker の erasure から。猶予の間で期限を過ぎた時だけ行う（2 回目と取り消しの後は何もしない。I-10）。
// セッションの失効（erased）、全端末の取り消し、hook OnUserErased、DEK と user_private の削除（crypto-shredding）、状態 erased。
// erasures の記録は srv-admin（OnUserErased の登録）。持ち主の記録は残す（第12章 5.4）。
func (u *Users) Erase(ctx context.Context, tx *store.Tx, id uuid.UUID) error {
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return err
	}
	var at *time.Time
	if err := tx.QueryRow(ctx, `SELECT erase_at FROM users WHERE id = $1`, id).Scan(&at); err != nil {
		return wrap(err)
	}
	if usr.State != UserErasePending || at == nil || at.After(u.d.now()) {
		return nil
	}
	// 錠の順は構成員、端末、セッション。端末の行を先に取り、セッションを erased で失効させてから端末を取り消す
	// （端末に届く session.revoked の理由を erased にする）。
	devs, err := uuids(ctx, tx, `SELECT id FROM devices WHERE user_id = $1 AND state NOT IN ('revoked', 'expired') ORDER BY id FOR UPDATE`, id)
	if err != nil {
		return err
	}
	if err := u.sessions.RevokeAllOfUser(ctx, tx, id, RevokeErased); err != nil {
		return err
	}
	for _, dv := range devs {
		if err := u.devices.Revoke(ctx, tx, dv, id, false); err != nil {
			return err
		}
	}
	if err := u.hooks.onUserErased(ctx, tx, id); err != nil {
		return err
	}
	if err := u.cardNotice(ctx, tx, usr); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_private WHERE user_id = $1`, id); err != nil {
		return wrap(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_dek WHERE user_id = $1`, id); err != nil {
		return wrap(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET state = 'erased', erase_at = NULL WHERE id = $1`, id); err != nil {
		return wrap(err)
	}
	return u.d.emitUserChanged(ctx, tx, id, fieldState)
}

// FamilyIds は家族の全員（状態が削除・抹消でない構成員。識別子の順）。srv-keys の feed の宛先（recovery.used の family、
// keybundle.changed の family、user.changed の signing_key）。q は ctx のトランザクションか Pool。
func (u *Users) FamilyIds(ctx context.Context, q store.Querier) ([]uuid.UUID, error) {
	return familyMembers(ctx, q)
}

// AdminIds は状態が通常の管理者（識別子の順）。srv-keys の recovery.used の宛先。
func (u *Users) AdminIds(ctx context.Context, q store.Querier) ([]uuid.UUID, error) {
	return uuids(ctx, q, `SELECT id FROM users WHERE role = 'admin' AND state = 'normal' ORDER BY id`)
}

// SetRole は役割を変える。同じ役割なら何もしない。最後の 1 人の管理者の解除は MEM-003。削除・抹消は MEM-002。
func (u *Users) SetRole(ctx context.Context, tx *store.Tx, id uuid.UUID, role boundary.Role, by uuid.UUID) error {
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if !usr.State.inFamily() {
		return ev(cgen.MEM002)
	}
	if usr.Role == role {
		return nil
	}
	if role == boundary.RoleMember {
		if last, err := lastAdmin(ctx, tx, usr); err != nil || last {
			if err != nil {
				return err
			}
			return ev(cgen.MEM003)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, id, string(role)); err != nil {
		return wrap(err)
	}
	return u.d.emitUserChanged(ctx, tx, id, fieldRole)
}

// Transfer は /admin/transfer。admin_transfers に求めを置き（7 日。前の求めは取り消す。第3章 8.4）、相手に
// transfer_requested を出す。相手が状態の通常の大人で本人と違う人でなければ MEM-002。
func (u *Users) Transfer(ctx context.Context, tx *store.Tx, from, to uuid.UUID) (uuid.UUID, error) {
	target, err := u.get(ctx, tx, to, false)
	if err != nil {
		return uuid.Nil, err
	}
	if target.State != UserNormal || target.IsChild || to == from {
		return uuid.Nil, ev(cgen.MEM002)
	}
	sender, err := u.get(ctx, tx, from, false)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_transfers SET state = 'cancelled' WHERE state = 'open'`); err != nil {
		return uuid.Nil, wrap(err)
	}
	id := u.d.Ids.NewV7()
	now := u.d.now()
	exp := now.Add(transferTtl)
	if _, err := tx.Exec(ctx, `INSERT INTO admin_transfers (id, from_user_id, to_user_id, state, created_at, expires_at) VALUES ($1, $2, $3, 'open', $4, $5)`,
		id, from, to, now, exp); err != nil {
		return uuid.Nil, wrap(err)
	}
	return id, u.hooks.notifySelf(ctx, tx, to, noticeTransferRequested, noticeKey(id.String()), map[string]any{"transfer_id": id.String(),
		"from_user_id": from.String(), "from_name": sender.DisplayName, "expires_at": exp.UTC().Format(time.RFC3339)})
}

// ExpireTransfers は srv-worker の PruneHandler から。open で期限を過ぎた引き継ぎの求めを自分のトランザクションで
// expired にする。知らせは出さない（第3章 8.4。期限は求めの画面に示してある）。戻りは数。
func (u *Users) ExpireTransfers(ctx context.Context) (int, error) {
	var n int64
	err := u.d.separateTx(ctx, func(tx *store.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE admin_transfers SET state = 'expired' WHERE state = 'open' AND expires_at <= $1`, u.d.now())
		n = tag.RowsAffected()
		return wrap(err)
	})
	return int(n), err
}

// AcceptTransfer は POST /me/transfer/{id}/accept（G-18）。指名された本人だけ。求めが無い・他人宛て・open でない・期限切れは
// AUTH-007（他人宛ての有無を漏らさない。期限切れの状態は ExpireTransfers が書く）。
// 役割を入れ替え、Audit（本ブロックのタグから入る管理者の効果のある操作）。
func (u *Users) AcceptTransfer(ctx context.Context, tx *store.Tx, p boundary.Principal, transferId uuid.UUID) error {
	me, _, err := self(p)
	if err != nil {
		return err
	}
	var from, to uuid.UUID
	var state string
	var exp time.Time
	err = tx.QueryRow(ctx, `SELECT from_user_id, to_user_id, state, expires_at FROM admin_transfers WHERE id = $1 FOR UPDATE`, transferId).
		Scan(&from, &to, &state, &exp)
	if noRows(err) {
		return ev(cgen.AUTH007)
	}
	if err != nil {
		return wrap(err)
	}
	if to != me || state != "open" {
		return ev(cgen.AUTH007)
	}
	if !exp.After(u.d.now()) {
		// 失効の書き込みは ExpireTransfers の 1 か所（この行の錠を持ったまま別のトランザクションで書くと待ち合って止まる）。
		return ev(cgen.AUTH007)
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_transfers SET state = 'accepted' WHERE id = $1`, transferId); err != nil {
		return wrap(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, to); err != nil {
		return wrap(err)
	}
	if from != to {
		if _, err := tx.Exec(ctx, `UPDATE users SET role = 'member' WHERE id = $1`, from); err != nil {
			return wrap(err)
		}
		if err := u.d.emitUserChanged(ctx, tx, from, fieldRole); err != nil {
			return err
		}
	}
	if err := u.d.emitUserChanged(ctx, tx, to, fieldRole); err != nil {
		return err
	}
	return wrap(u.d.Audit.Record(ctx, tx, to, auditTransferAccepted, from.String(), map[string]any{"transfer_id": transferId.String()}))
}

// 本ブロックが記録する監査の操作の名（srv-admin の audit の action。管理者の操作のうち本ブロックのタグから入る物）。
const (
	auditTransferAccepted = "admin_transfer_accepted"
	auditInviteIssued     = "invite_issued"
	auditInviteRevoked    = "invite_revoked"
)

// AckDisclosure は POST /me/disclosure-ack（G-09）。今の版でなければ MEM-008（params.version に今の版）。
func (u *Users) AckDisclosure(ctx context.Context, tx *store.Tx, userId uuid.UUID, version string) error {
	doc, err := u.families.Disclosure(ctx)
	if err != nil {
		return err
	}
	if version != doc.Version {
		return fault.New(cgen.MEM008, map[string]any{"version": doc.Version})
	}
	_, err = tx.Exec(ctx, `UPDATE users SET disclosure_ack_version = $2 WHERE id = $1`, userId, version)
	return wrap(err)
}

// birthdays は誕生日を持つ構成員（state の条件で絞る）の誕生日を開く。構成員の数だけ DEK を開く（家族の規模で足りる）。
func (u *Users) birthdays(ctx context.Context, stateCond string) (map[uuid.UUID]birthday, error) {
	ids, err := uuids(ctx, u.d.Pool, `SELECT u.id FROM users u JOIN user_private p ON p.user_id = u.id
WHERE p.birthday IS NOT NULL AND `+stateCond+` ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]birthday{}
	for _, id := range ids {
		b, err := u.readBirthday(ctx, u.d.Pool, id)
		if err != nil {
			return nil, err
		}
		if b != nil {
			out[id] = *b
		}
	}
	return out, nil
}

// BirthdayToday は srv-worker の誕生日の知らせ（月日が今日の、状態が通常の構成員。2 月 29 日生まれは平年の 2 月 28 日）。
func (u *Users) BirthdayToday(ctx context.Context, today time.Time) ([]uuid.UUID, error) {
	bs, err := u.birthdays(ctx, `u.state = 'normal'`)
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for id, b := range bs {
		if b.isToday(today) {
			out = append(out, id)
		}
	}
	return out, nil
}

// SetChild は PUT /admin/users/{id}/child（srv-admin の operation から。監査は srv-admin。第3章 9.1）。区分を付けるのは
// 招待とこの操作だけ。対象が無い・家族でなければ MEM-001。今と同じ値なら何もしない。付ける時に年のある誕生日で
// 今日 18 歳以上なら MEM-012（誕生日が正）。付けたら本人と保護者へ child_flag_set。外す時は clearChild の後に
// 本人と消す前の保護者へ child_flag_cleared。
func (u *Users) SetChild(ctx context.Context, tx *store.Tx, id uuid.UUID, child bool, by uuid.UUID) error {
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if !usr.State.inFamily() {
		return ev(cgen.MEM001)
	}
	if usr.IsChild == child {
		return nil
	}
	params := map[string]any{"user_id": id.String(), "name": usr.DisplayName}
	if !child {
		former, err := u.clearChild(ctx, tx, id)
		if err != nil {
			return err
		}
		return u.hooks.notifyEach(ctx, tx, append([]uuid.UUID{id}, former...), noticeChildFlagCleared, noticeKey(id.String()), params)
	}
	b, err := u.readBirthday(ctx, tx, id)
	if err != nil {
		return err
	}
	if b != nil && b.ageOn(u.d.now()) >= childAge {
		return ev(cgen.MEM012)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET is_child = true, child_declared = true WHERE id = $1`, id); err != nil {
		return wrap(err)
	}
	if err := u.d.emitUserChanged(ctx, tx, id, fieldChild); err != nil {
		return err
	}
	if err := u.hooks.notifySelf(ctx, tx, id, noticeChildFlagSet, noticeKey(id.String()), params); err != nil {
		return err
	}
	return u.hooks.notifyGuardians(ctx, tx, id, noticeChildFlagSet, noticeKey(id.String()), params)
}

// clearChild は子ども区分を外し、消す前の保護者を返す（18 歳の解除と管理者の解除。第3章 9.1）。保護者の関係と
// child_controls を消し、その子の open の承認の求めを知らせなしで expired にする（保護者がいなくなれば承認できない。
// 端末の追加の求めは端末のコードも expired にし、新しい端末の poll は expired で終わる。denied ではない：誰も断っておらず、
// 区分が外れた本人が承認し直せる。第3章 4.4）。user.changed(fields: child)。
func (u *Users) clearChild(ctx context.Context, tx *store.Tx, id uuid.UUID) ([]uuid.UUID, error) {
	former, err := uuids(ctx, tx, `SELECT guardian_id FROM guardians WHERE child_id = $1 ORDER BY guardian_id`, id)
	if err != nil {
		return nil, err
	}
	for _, sql := range []string{
		`UPDATE users SET is_child = false, child_declared = false WHERE id = $1`,
		`DELETE FROM guardians WHERE child_id = $1`,
		`DELETE FROM child_controls WHERE child_id = $1`,
		`UPDATE device_codes SET state = 'expired' WHERE state = 'pending' AND device_code_sha256 IN
  (SELECT target_sha256 FROM guardian_approvals WHERE child_id = $1 AND kind = 'device' AND state = 'open')`,
		`UPDATE guardian_approvals SET state = 'expired' WHERE child_id = $1 AND state = 'open'`,
	} {
		if _, err := tx.Exec(ctx, sql, id); err != nil {
			return nil, wrap(err)
		}
	}
	return former, u.d.emitUserChanged(ctx, tx, id, fieldChild)
}

// applyAge は、その誕生日で today までに迎えた 16 歳・18 歳の解除を適用する（第3章 9.1）。区分が無ければ何もしない。
// 18 歳以上なら clearChild の後に本人と消す前の保護者へ child_released_18。16 歳以上なら利用の時間帯と外部リンクの
// 確認を外し、本人と保護者へ child_relaxed_16。ただし 16 歳の誕生日より後に保護者が設定を変えていれば（updated_by が
// 子以外）その設定が正で、既に外れていれば（同じ日の 2 回目）何もしない。戻りは適用した年齢（0 は無し）。
// ChildTransitions（誕生日の当日）と Guardians.SetBirthday（書いた直後）が使う。
func (u *Users) applyAge(ctx context.Context, tx *store.Tx, id uuid.UUID, b birthday, today time.Time) (int, error) {
	age := b.ageOn(today)
	if age < relaxAge {
		return 0, nil
	}
	usr, err := u.get(ctx, tx, id, true)
	if err != nil {
		return 0, err
	}
	if !usr.IsChild {
		return 0, nil
	}
	params := map[string]any{"user_id": id.String(), "name": usr.DisplayName}
	if age >= childAge {
		former, err := u.clearChild(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		return childAge, u.hooks.notifyEach(ctx, tx, append([]uuid.UUID{id}, former...), noticeChildReleased18, noticeKey(id.String()), params)
	}
	var by *uuid.UUID
	var updatedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT updated_by, updated_at FROM child_controls WHERE child_id = $1`, id).Scan(&by, &updatedAt); err != nil && !noRows(err) {
		return 0, wrap(err)
	}
	if by != nil && *by != id && updatedAt != nil && !updatedAt.Before(b.birthdayIn(b.Year+relaxAge, today.Location())) {
		return 0, nil
	}
	cur, err := u.guardians.settings(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if (cur.QuietHours == nil || *cur.QuietHours == "") && cur.LinkConfirm != nil && !*cur.LinkConfirm {
		return 0, nil
	}
	if err := u.guardians.relax(ctx, tx, id); err != nil {
		return 0, err
	}
	if err := u.hooks.notifySelf(ctx, tx, id, noticeChildRelaxed16, noticeKey(id.String()), params); err != nil {
		return 0, err
	}
	return relaxAge, u.hooks.notifyGuardians(ctx, tx, id, noticeChildRelaxed16, noticeKey(id.String()), params)
}

// ChildTransitions は誕生日のある子ども区分で、今日 16 歳か 18 歳になる人（第3章 9.1）。人ごとに自分のトランザクションで
// applyAge を呼ぶ。同じ日の 2 回目は何も変えない（I-10）。
func (u *Users) ChildTransitions(ctx context.Context, today time.Time) ([]Transition, error) {
	bs, err := u.birthdays(ctx, `u.is_child AND u.state IN ('normal', 'suspended', 'erase_pending')`)
	if err != nil {
		return nil, err
	}
	var out []Transition
	for id, b := range bs {
		if b.Year == 0 || !b.isToday(today) {
			continue
		}
		if age := b.ageOn(today); age != relaxAge && age != childAge {
			continue
		}
		var applied int
		err := u.d.separateTx(ctx, func(tx *store.Tx) error {
			var err error
			applied, err = u.applyAge(ctx, tx, id, b, today)
			return err
		})
		if err != nil {
			return nil, err
		}
		if applied != 0 {
			out = append(out, Transition{UserId: id, Age: applied})
		}
	}
	return out, nil
}
