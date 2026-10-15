// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/rivo/uniseg"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// disclosureSource は明示の文の生成ファイル（第12章 7.1、8 から tools/gen_texts が作る。make gen。コミットしない）。
//
//go:embed gen/disclosure.json
var disclosureSource []byte

// Family は家族の 1 行（families）。
type Family struct {
	Id          uuid.UUID
	Name        string
	IconMediaId *uuid.UUID
	// Color は家族の色（manifest の theme_color）。
	Color       string
	InstalledAt time.Time
	// RecoveryPub は家族回復鍵の公開部の JWK（SetRecoveryPub が書く。srv-keys FamilyRecoveryKey.SetPublic から）。
	RecoveryPub json.RawMessage
	// RecoveryKeyVersion は公開部の版（初めは 0。置くたびに 1 増える）。
	RecoveryKeyVersion int
	// CardPrintedAt は家族回復鍵のカードを印刷した時刻。空の間は管理者の画面にバナー（第3章 3.1 の 6）。
	CardPrintedAt *time.Time
	// CardPrintedBy はカード 1 の持ち主（印刷した管理者。第12章 5.4）。
	CardPrintedBy *uuid.UUID
	adminNames    []string
}

// AdminNames は管理者の表示名（明示の文に埋める。2 人なら「、」で結ぶ）。
func (f Family) AdminNames() []string {
	return f.adminNames
}

// RecoveryCard は家族回復鍵のカード 1 枚の持ち主（GET /family/recovery-cards の 1 行）。
type RecoveryCard struct {
	// No は 1〜3。1 は CardPrintedBy、2 と 3 は recovery_cards。
	No       int
	HolderId *uuid.UUID
	HeldAt   *time.Time
}

// DisclosureDoc は GET /family/disclosure の応答（値を埋めた文と外へ送る情報の表と版）。
type DisclosureDoc = gen.GetFamilyDisclosureResponse

// familyColors は家族の色の 8 色（第8章 3.1。どれも白との対比が 4.5:1 以上）。
var familyColors = [8]string{"#1F5FAD", "#2E7D32", "#B23A48", "#6A3FA0", "#8A5A00", "#00796B", "#C2185B", "#455A64"}

// colorOf は家族の識別子の先頭 8 バイトを 8 で割った余りで色を選ぶ（第8章 3.1。利用者には選ばせない）。
func colorOf(id uuid.UUID) string {
	var n uint64
	for _, b := range id.Bytes()[:8] {
		n = n<<8 | uint64(b)
	}
	return familyColors[n%uint64(len(familyColors))]
}

// Families は家族の記録を持つ（B-060）。
type Families struct {
	d     *Deps
	icon  *Icon
	hooks *Hooks

	mu       sync.Mutex
	manifest map[string]any
	mfKey    string
}

// Get は家族の 1 行を返す。設置の前は SRV の状態の判定が先に返すので、行が無い時は予期しない失敗。
func (f *Families) Get(ctx context.Context) (Family, error) {
	return f.get(ctx, f.d.Pool)
}

func (f *Families) get(ctx context.Context, q querier) (Family, error) {
	var fam Family
	var pub []byte
	err := q.QueryRow(ctx, `SELECT id, name, icon_media_id, color, installed_at, recovery_pub, recovery_key_version, card_printed_at, card_printed_by
FROM families`).
		Scan(&fam.Id, &fam.Name, &fam.IconMediaId, &fam.Color, &fam.InstalledAt, &pub, &fam.RecoveryKeyVersion, &fam.CardPrintedAt, &fam.CardPrintedBy)
	if noRows(err) {
		return Family{}, fault.Wrap(errors.New("identity: 家族の行が無い"))
	}
	if err != nil {
		return Family{}, wrap(err)
	}
	fam.RecoveryPub = pub
	fam.InstalledAt = fam.InstalledAt.UTC()
	rows, err := q.Query(ctx, `SELECT display_name FROM users WHERE role = 'admin' AND state = 'normal' ORDER BY created_at, id`)
	if err != nil {
		return Family{}, wrap(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return Family{}, wrap(err)
		}
		fam.adminNames = append(fam.adminNames, n)
	}
	return fam, wrap(rows.Err())
}

// create は家族の行を作る（SetupLink.Consume から。設置の 1 回だけ）。
func (f *Families) create(ctx context.Context, tx *store.Tx, name string) (Family, error) {
	if e := checkName(name, nameMax); e != nil {
		return Family{}, e
	}
	fam := Family{Id: f.d.Ids.NewV7(), Name: name, InstalledAt: f.d.now()}
	fam.Color = colorOf(fam.Id)
	_, err := tx.Exec(ctx, `INSERT INTO families (id, name, color, installed_at) VALUES ($1, $2, $3, $4)`,
		fam.Id, fam.Name, fam.Color, fam.InstalledAt)
	return fam, wrap(err)
}

// Update は /admin/family の名前とアイコンを書く（srv-admin の operation から）。名前は 20 書記素（MEM-005）。
// アイコンの添付の実体の確かめは呼ぶ側（srv-admin が srv-media で）。家族全員のトークのイベントは呼ぶ側の srv-admin が流す。
func (f *Families) Update(ctx context.Context, tx *store.Tx, name *string, iconMediaId *uuid.UUID) error {
	if name != nil {
		if e := checkName(*name, nameMax); e != nil {
			return e
		}
		if _, err := tx.Exec(ctx, `UPDATE families SET name = $1`, *name); err != nil {
			return wrap(err)
		}
		tx.AfterCommit(f.dropManifest)
	}
	if iconMediaId != nil {
		if _, err := tx.Exec(ctx, `UPDATE families SET icon_media_id = $1`, *iconMediaId); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// dropManifest は manifest のキャッシュを捨てる（名前の変更のコミットの後）。
func (f *Families) dropManifest() {
	f.mu.Lock()
	f.manifest = nil
	f.mfKey = ""
	f.mu.Unlock()
}

// iconSizes は manifest のアイコンの 4 つ（第8章 3.1）。
var iconSizes = []struct {
	size    int
	variant IconVariant
}{{192, IconAny}, {512, IconAny}, {512, IconMaskable}, {512, IconMonochrome}}

// Manifest は第8章 3.1 の manifest を返す。アイコンのファイルが無ければ作る（Icon のキャッシュ）。
func (f *Families) Manifest(ctx context.Context) (gen.GetManifestResponse, error) {
	fam, err := f.Get(ctx)
	if err != nil {
		return nil, err
	}
	key := fam.Name + "\x00" + fam.Color
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.manifest != nil && f.mfKey == key {
		return f.manifest, nil
	}
	icons := make([]any, 0, len(iconSizes))
	for _, s := range iconSizes {
		if err := f.icon.ensure(fam.Color, s.size, s.variant); err != nil {
			return nil, err
		}
		icons = append(icons, map[string]any{"src": iconPath(s.size, s.variant), "sizes": strconv.Itoa(s.size) + "x" + strconv.Itoa(s.size),
			"type": "image/png", "purpose": string(s.variant)})
	}
	m := map[string]any{
		"id": "/", "name": fam.Name, "short_name": shortName(fam.Name), "start_url": "/", "scope": "/",
		"display": "standalone", "lang": "ja", "dir": "ltr", "theme_color": fam.Color, "background_color": "#ffffff",
		"icons": icons, "description": "家族の連絡",
		"screenshots": []any{
			map[string]any{"src": "/assets/screenshots/narrow.png", "sizes": "1080x1920", "type": "image/png", "form_factor": "narrow", "label": "トーク"},
			map[string]any{"src": "/assets/screenshots/wide.png", "sizes": "1920x1080", "type": "image/png", "form_factor": "wide", "label": "トーク"},
		},
	}
	f.manifest = m
	f.mfKey = key
	return m, nil
}

// shortName は家族の名前を 12 書記素までに切る（第8章 3.1）。
func shortName(name string) string {
	var b strings.Builder
	g := uniseg.NewGraphemes(name)
	for n := 0; n < 12 && g.Next(); n++ {
		b.WriteString(g.Str())
	}
	return b.String()
}

// disclosureItem は生成ファイルの文の 1 行（mode は形 D の時だけ示す行）。
type disclosureItem struct {
	Text string `json:"text"`
	Mode string `json:"mode,omitempty"`
}

// disclosureRow は外へ送る情報の 1 行。
type disclosureRow struct {
	Content     string `json:"content"`
	Destination string `json:"destination"`
	Purpose     string `json:"purpose"`
	Mode        string `json:"mode,omitempty"`
}

// disclosureFile は gen/disclosure.json の形。
type disclosureFile struct {
	Title    string           `json:"title"`
	Lead     string           `json:"lead"`
	Items    []disclosureItem `json:"items"`
	Outbound []disclosureRow  `json:"outbound"`
}

// disclosureOnce は生成ファイルを起動の後の最初の 1 回だけ読む。
var disclosureOnce = sync.OnceValues(func() (disclosureFile, error) {
	var d disclosureFile
	if err := json.Unmarshal(disclosureSource, &d); err != nil {
		return d, fault.Wrap(err)
	}
	if d.Title == "" || d.Lead == "" || len(d.Items) == 0 || len(d.Outbound) == 0 {
		return d, fault.Wrap(errors.New("identity: gen/disclosure.json の形が違う"))
	}
	return d, nil
})

// Disclosure は第12章 7.1 の規則で文を埋めて返す（{admin}、{provider}、{card_holders}、形 D の行、版）。
func (f *Families) Disclosure(ctx context.Context) (DisclosureDoc, error) {
	src, err := disclosureOnce()
	if err != nil {
		return DisclosureDoc{}, err
	}
	fam, err := f.Get(ctx)
	if err != nil {
		return DisclosureDoc{}, err
	}
	holders, err := f.cardHoldersText(ctx)
	if err != nil {
		return DisclosureDoc{}, err
	}
	modeD := f.d.Config.Mode == "D"
	fill := strings.NewReplacer("{admin}", strings.Join(fam.AdminNames(), "、"),
		"{provider}", f.d.Config.Get(cgen.KeyServerHostingProvider).String(), "{card_holders}", holders)
	var doc DisclosureDoc
	doc.Notice.Title = src.Title
	doc.Notice.Lead = fill.Replace(src.Lead)
	doc.Notice.Items = []string{}
	for _, it := range src.Items {
		if it.Mode == "D" && !modeD {
			continue
		}
		doc.Notice.Items = append(doc.Notice.Items, fill.Replace(it.Text))
	}
	for _, r := range src.Outbound {
		if r.Mode == "D" && !modeD {
			continue
		}
		doc.Outbound = append(doc.Outbound, struct {
			Content     string `json:"content"`
			Destination string `json:"destination"`
			Purpose     string `json:"purpose"`
		}{r.Content, r.Destination, r.Purpose})
	}
	v, err := disclosureVersion(doc)
	if err != nil {
		return DisclosureDoc{}, err
	}
	doc.Version = v
	return doc, nil
}

// disclosureVersion は notice と outbound を RFC 8785 で正規化した JSON の SHA-256 の先頭 16 文字（第12章 7.1）。
// 値は文字列と配列とオブジェクトだけなので、鍵を並べ（map は鍵の順に出る）HTML の escape をしない JSON で同じ列になる。
func disclosureVersion(doc DisclosureDoc) (string, error) {
	items := make([]any, len(doc.Notice.Items))
	for i, s := range doc.Notice.Items {
		items[i] = s
	}
	out := make([]any, len(doc.Outbound))
	for i, r := range doc.Outbound {
		out[i] = map[string]any{"content": r.Content, "destination": r.Destination, "purpose": r.Purpose}
	}
	v := map[string]any{"notice": map[string]any{"title": doc.Notice.Title, "lead": doc.Notice.Lead, "items": items}, "outbound": out}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", wrap(err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])[:16], nil
}

// cardHoldersText は {card_holders} の値（「1 枚目は A、2 枚目は B、3 枚目は C」。未配布は「まだ配られていません」）。
// 持ち主の表示名は状態に依らず今の表示名。
func (f *Families) cardHoldersText(ctx context.Context) (string, error) {
	cards, err := f.Cards(ctx)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(cards))
	for _, c := range cards {
		who := "まだ配られていません"
		if c.HolderId != nil {
			var name string
			if err := f.d.Pool.QueryRow(ctx, `SELECT display_name FROM users WHERE id = $1`, *c.HolderId).Scan(&name); err != nil {
				return "", wrap(err)
			}
			who = name
		}
		parts = append(parts, strconv.Itoa(c.No)+" 枚目は"+who)
	}
	return strings.Join(parts, "、"), nil
}

// MarkCardPrinted は card_printed_at を今に、card_printed_by を by にする。2 回目からは最初の時刻を返す。
// 監査は呼ぶ側の srv-admin。
func (f *Families) MarkCardPrinted(ctx context.Context, tx *store.Tx, by uuid.UUID) (time.Time, error) {
	var at time.Time
	err := tx.QueryRow(ctx, `UPDATE families SET card_printed_at = $1, card_printed_by = $2 WHERE card_printed_at IS NULL RETURNING card_printed_at`,
		f.d.now(), by).Scan(&at)
	if noRows(err) {
		err = tx.QueryRow(ctx, `SELECT card_printed_at FROM families`).Scan(&at)
	}
	if err != nil {
		return time.Time{}, wrap(err)
	}
	return at.UTC(), nil
}

// ClearCardPrinted はカードの作り直しで card_printed_at と card_printed_by を空に戻す（srv-admin RotateRecoveryKey）。
func (f *Families) ClearCardPrinted(ctx context.Context, tx *store.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE families SET card_printed_at = NULL, card_printed_by = NULL`)
	return wrap(err)
}

// MarkCardHeld は POST /me/recovery-cards/{n}（n は 2 か 3）。第12章 5.4 の受け取りの記録の規則の 1〜4 の順に確かめる：
// 子ども区分は MEM-010。本人が既に持つ札は何もしない（冪等）。別の人が持つ札は MEM-009（params.holder に表示名）。
// 本人が別の札（カード 1 は card_printed_by、2 と 3 は recovery_cards）を持つ時は、CardHolderCandidates が 3 以上なら
// MEM-011（params.held に本人が持つ札の番号）、3 未満なら許す。user.changed は送らず、Disclosure の版が変わる。
func (f *Families) MarkCardHeld(ctx context.Context, tx *store.Tx, userId uuid.UUID, no int) error {
	var printedBy *uuid.UUID
	// 家族の 1 行の錠で受け取りの記録を直列にする（同時の 2 つの受け取りが 3 の数え方をすり抜けないため）。
	if err := tx.QueryRow(ctx, `SELECT card_printed_by FROM families FOR UPDATE`).Scan(&printedBy); err != nil {
		return wrap(err)
	}
	var child bool
	var state UserState
	if err := tx.QueryRow(ctx, `SELECT is_child, state FROM users WHERE id = $1`, userId).Scan(&child, &state); err != nil {
		return wrap(err)
	}
	if child {
		return ev(cgen.MEM010)
	}
	if state != UserNormal {
		return ev(cgen.MEM002)
	}
	var holder uuid.UUID
	var holderName string
	err := tx.QueryRow(ctx, `SELECT r.holder_user_id, u.display_name FROM recovery_cards r JOIN users u ON u.id = r.holder_user_id WHERE r.no = $1`, no).
		Scan(&holder, &holderName)
	switch {
	case err == nil && holder == userId:
		return nil
	case err == nil:
		return fault.New(cgen.MEM009, map[string]any{"holder": holderName})
	case !noRows(err):
		return wrap(err)
	}
	held := 0
	if printedBy != nil && *printedBy == userId {
		held = 1
	} else {
		err := tx.QueryRow(ctx, `SELECT no FROM recovery_cards WHERE holder_user_id = $1 AND no <> $2 ORDER BY no LIMIT 1`, userId, no).Scan(&held)
		if err != nil && !noRows(err) {
			return wrap(err)
		}
	}
	if held != 0 {
		n, err := f.candidates(ctx, tx)
		if err != nil {
			return err
		}
		if n >= 3 {
			return fault.New(cgen.MEM011, map[string]any{"held": held})
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO recovery_cards (no, holder_user_id, held_at) VALUES ($1, $2, $3)`, no, userId, f.d.now())
	return wrap(err)
}

// CardHolderCandidates はカードを持てる構成員の数（第12章 5.4 の 1：子ども区分でなく状態が通常。管理者を含む）。
func (f *Families) CardHolderCandidates(ctx context.Context) (int, error) {
	return f.candidates(ctx, f.d.Pool)
}

func (f *Families) candidates(ctx context.Context, q querier) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_child = false AND state = 'normal'`).Scan(&n)
	return n, wrap(err)
}

// HoldsCard は userId がカード 1〜3 のどれかの持ち主か（card_printed_by か recovery_cards）。
// Users の停止・削除・抹消が管理者への知らせの要否に使う（第12章 5.4 の持ち主の変化、第7章 9）。
func (f *Families) HoldsCard(ctx context.Context, userId uuid.UUID) (bool, error) {
	return f.holds(ctx, f.d.Pool, userId)
}

func (f *Families) holds(ctx context.Context, q querier, userId uuid.UUID) (bool, error) {
	cards, err := f.held(ctx, q, userId)
	return len(cards) > 0, err
}

// held は userId が持つ札の番号を番号の順に返す（カード 1 は card_printed_by、2 と 3 は recovery_cards）。
// card_holder_changed の params.cards（第7章 9）。
func (f *Families) held(ctx context.Context, q querier, userId uuid.UUID) ([]int, error) {
	rows, err := q.Query(ctx, `SELECT 1 FROM families WHERE card_printed_by = $1
UNION ALL SELECT no FROM recovery_cards WHERE holder_user_id = $1 ORDER BY 1`, userId)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var no int
		if err := rows.Scan(&no); err != nil {
			return nil, wrap(err)
		}
		out = append(out, no)
	}
	return out, wrap(rows.Err())
}

// Cards は 3 枚の持ち主を番号の順に返す（GET /family/recovery-cards、明示の {card_holders}、管理者の画面の所在の表）。
func (f *Families) Cards(ctx context.Context) ([]RecoveryCard, error) {
	out := []RecoveryCard{{No: 1}, {No: 2}, {No: 3}}
	var by *uuid.UUID
	var at *time.Time
	if err := f.d.Pool.QueryRow(ctx, `SELECT card_printed_by, card_printed_at FROM families`).Scan(&by, &at); err != nil && !noRows(err) {
		return nil, wrap(err)
	}
	out[0].HolderId, out[0].HeldAt = by, utcPtr(at)
	rows, err := f.d.Pool.Query(ctx, `SELECT no, holder_user_id, held_at FROM recovery_cards ORDER BY no`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	for rows.Next() {
		var no int
		var holder uuid.UUID
		var held time.Time
		if err := rows.Scan(&no, &holder, &held); err != nil {
			return nil, wrap(err)
		}
		h, t := holder, held.UTC()
		out[no-1].HolderId, out[no-1].HeldAt = &h, &t
	}
	return out, wrap(rows.Err())
}

// ClearCardHolders はカードの作り直しで recovery_cards を空にする（srv-admin RotateRecoveryKey）。
func (f *Families) ClearCardHolders(ctx context.Context, tx *store.Tx) error {
	_, err := tx.Exec(ctx, `DELETE FROM recovery_cards`)
	return wrap(err)
}

// utcPtr は時刻の UTC の写し。
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// SetRecoveryPub は家族回復鍵の公開部（JWK）を置き、版を 1 増やして返す（families の行の錠を取る。第12章 5.4 (d)）。
// 表の持ち主は本ブロックで、呼ぶのは srv-keys FamilyRecoveryKey.SetPublic だけ（全体実装設計書 4.3）。JWK の形の確かめは呼ぶ側。
func (f *Families) SetRecoveryPub(ctx context.Context, tx *store.Tx, pub json.RawMessage) (int, error) {
	var version int
	err := tx.QueryRow(ctx, `UPDATE families SET recovery_pub = $1, recovery_key_version = recovery_key_version + 1 RETURNING recovery_key_version`,
		[]byte(pub)).Scan(&version)
	if noRows(err) {
		return 0, fault.Wrap(errors.New("identity: 家族の行が無い"))
	}
	return version, wrap(err)
}
