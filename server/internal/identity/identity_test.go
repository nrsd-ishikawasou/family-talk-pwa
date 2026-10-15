// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/envelope"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/secret"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// testDomain は試験の家族のサーバーの名前。
const testDomain = "family.example.jp"

// codeOf は error が Event ならその番号、違えば空。
func codeOf(err error) string {
	var e *fault.Event
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// writeSecret は秘密のファイルを 0400 で置く（secret.Store が読む形）。
func writeSecret(t *testing.T, dir, name string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o400); err != nil {
		t.Fatal(err)
	}
}

// testSecrets は KEK 1 本とセッションの署名鍵 2 本（新旧）を置いた秘密の置き場を作る。
func testSecrets(t *testing.T, now time.Time) *secret.Store {
	t.Helper()
	dir := t.TempDir()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	writeSecret(t, dir, secret.NameKek, map[string]any{"keys": []map[string]any{
		{"id": uuid.Must(uuid.NewV7()).String(), "key": kek, "created_at": now.Add(-time.Hour)}}})
	old, cur := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(old)
	_, _ = rand.Read(cur)
	writeSecret(t, dir, secret.NameSessionKeys, map[string]any{"keys": []map[string]any{
		{"id": "k-old", "alg": "HS256", "key": old, "created_at": now.Add(-48 * time.Hour), "retire_after": now.Add(24 * time.Hour)},
		{"id": "k-new", "alg": "HS256", "key": cur, "created_at": now.Add(-time.Hour), "retire_after": now.Add(90 * 24 * time.Hour)}}})
	return secret.NewStore(secret.Dirs{Data: dir, Run: dir})
}

// testConfig は試験の設定（データの置き場は試験の一時の置き場）。
func testConfig(t *testing.T, dataDir string) *config.Config {
	t.Helper()
	cfg, err := config.Load(map[string]string{"FT_DOMAIN": testDomain, "FT_DNS_PROVIDER": "test", "FT_DATA_DIR": dataDir}, nil, cgen.Items)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// R: R-3-2-2
// 誕生日の形と暦（2 月 29 日は年が無いか閏年だけ、1900 年より前は不可）、満年齢（2 月 29 日生まれは平年の 2 月 28 日に年を取る）、
// 今日が誕生日か、その年の誕生日の 0 時（第3章 9.1）。
func TestBirthdayParseAgeAndToday(t *testing.T) {
	for _, s := range []string{"2001-02-29", "1899-12-31", "2000-13-01", "2000-04-31", "02-30", "2000-1-01", "２０００-01-01", ""} {
		if _, e := parseBirthday(s); e == nil || e.Code != cgen.MEM005 {
			t.Fatalf("%q を誤りにしない: %v", s, e)
		}
	}
	for s, want := range map[string]birthday{"2000-02-29": {2000, 2, 29}, "02-29": {0, 2, 29}, "1900-01-01": {1900, 1, 1}} {
		b, e := parseBirthday(s)
		if e != nil || b != want || b.String() != s {
			t.Fatalf("%q の読みが違う: %+v %v %q", s, b, e, b.String())
		}
	}
	leap := birthday{2008, 2, 29}
	day := func(s string) time.Time {
		v, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		today string
		age   int
		isDay bool
	}{
		{"2024-02-28", 15, false}, {"2024-02-29", 16, true}, // 閏年は 2 月 29 日に年を取る
		{"2025-02-27", 16, false}, {"2025-02-28", 17, true}, // 平年は 2 月 28 日
		{"2026-03-01", 18, false},
	}
	for _, c := range cases {
		if got := leap.ageOn(day(c.today)); got != c.age {
			t.Fatalf("%s の満年齢が %d でない: %d", c.today, c.age, got)
		}
		if got := leap.isToday(day(c.today)); got != c.isDay {
			t.Fatalf("%s が誕生日の判定 %v でない", c.today, c.isDay)
		}
	}
	if (birthday{0, 5, 1}).ageOn(day("2026-05-01")) != -1 {
		t.Fatal("年の無い誕生日の満年齢が -1 でない")
	}
	if got := leap.birthdayIn(2025, time.UTC); !got.Equal(day("2025-02-28")) {
		t.Fatalf("平年の誕生日の 0 時が 2 月 28 日でない: %v", got)
	}
	if got := leap.birthdayIn(2024, time.UTC); !got.Equal(day("2024-02-29")) {
		t.Fatalf("閏年の誕生日の 0 時が 2 月 29 日でない: %v", got)
	}
}

// relLuminance は WCAG 2.2 の相対輝度。
func relLuminance(hexColor string) float64 {
	v, _ := strconv.ParseUint(hexColor[1:], 16, 32)
	ch := func(c uint64) float64 {
		s := float64(c) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(v>>16&0xff) + 0.7152*ch(v>>8&0xff) + 0.0722*ch(v&0xff)
}

// R: R-8-1-1
// 家族の色の 8 色は白との対比が 4.5:1 以上（WCAG 2.2 の対比の式）。色は家族の識別子の先頭 8 バイトで決まる（第8章 3.1）。
func TestFamilyColorsContrastAndChoice(t *testing.T) {
	for _, c := range familyColors {
		ratio := (1.0 + 0.05) / (relLuminance(c) + 0.05)
		if ratio < 4.5 {
			t.Fatalf("%s の白との対比が 4.5 未満: %.2f", c, ratio)
		}
	}
	seen := map[string]bool{}
	for n := 0; n < 64; n++ {
		var id uuid.UUID
		id[7] = byte(n)
		c := colorOf(id)
		if colorOf(id) != c {
			t.Fatal("同じ識別子で色が変わる")
		}
		if c != familyColors[n%8] {
			t.Fatalf("先頭 8 バイトを 8 で割った余りで選ばない: %d %s", n, c)
		}
		seen[c] = true
	}
	if len(seen) != len(familyColors) {
		t.Fatalf("8 色の全部が選ばれない: %d", len(seen))
	}
}

// R: R-1-2-5
// 名前は書記素で数える（絵文字の家族は 1 つ）。空白だけは不可。家族の名前の短い名は 12 書記素（第3章 6、第8章 3.1）。
func TestNamesCountGraphemes(t *testing.T) {
	family := "👨‍👩‍👧‍👦"
	if graphemes(family) != 1 {
		t.Fatalf("家族の絵文字が 1 書記素でない: %d", graphemes(family))
	}
	if checkName(strings.Repeat(family, nameMax), nameMax) != nil {
		t.Fatal("20 書記素の名前を拒んだ")
	}
	if e := checkName(strings.Repeat("あ", nameMax+1), nameMax); e == nil || e.Code != cgen.MEM005 {
		t.Fatal("21 書記素の名前が MEM-005 でない")
	}
	if e := checkName(" 　 ", nameMax); e == nil {
		t.Fatal("空白だけの名前を許した")
	}
	if got := shortName(strings.Repeat("が", 15)); got != strings.Repeat("が", 12) {
		t.Fatalf("短い名が 12 書記素でない: %q", got)
	}
	if e := badDeviceName(strings.Repeat("x", deviceNameMax+1)); e == nil || e.Code != cgen.NET001 {
		t.Fatal("41 書記素の端末の名前が NET-001 でない")
	}
}

// R: R-3-3-3
// 利用者の入力の揺れ（小文字、区切りの有無、全角の空白）を 4-4 の形に揃える（第3章 4.4）。
func TestNormalizeUserCode(t *testing.T) {
	for in, want := range map[string]string{"abcd-efgh": "ABCD-EFGH", "ABCDEFGH": "ABCD-EFGH", "ab cd　ef gh": "ABCD-EFGH", "ABC": "ABC"} {
		if got := normalizeUserCode(in); got != want {
			t.Fatalf("%q が %q でない: %q", in, want, got)
		}
	}
}

// R: R-3-3-2
// Cookie は __Host-session、Path=/、Secure、HttpOnly、SameSite=Lax、Max-Age は期限までの秒（第2章 4.1）。消す時は Max-Age=0。
func TestSessionCookieAttributes(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	c := SessionCookie("v", now.Add(90*24*time.Hour), now)
	for _, part := range []string{"__Host-session=v", "; Path=/", "; Max-Age=7776000", "; Secure", "; HttpOnly", "; SameSite=Lax", "; Expires=Tue, 12 Jan 2027 00:00:00 GMT"} {
		if !strings.Contains(c, part) {
			t.Fatalf("Set-Cookie に %q が無い: %s", part, c)
		}
	}
	if strings.Contains(c, "Domain=") {
		t.Fatal("__Host- の Cookie に Domain がある")
	}
	if got := SessionCookie("v", now.Add(-time.Second), now); !strings.Contains(got, "Max-Age=0;") {
		t.Fatalf("過ぎた期限の Max-Age が 0 でない: %s", got)
	}
	if got := ClearSessionCookie(); !strings.HasPrefix(got, "__Host-session=; ") || !strings.Contains(got, "Max-Age=0") {
		t.Fatalf("消す Cookie の形が違う: %s", got)
	}
}

// R: R-12-7-1
// 短いトークンは <鍵 ID>.<乱数>.<HMAC>。発行は新しい鍵、確かめは鍵 ID で新旧のどちらも。種類が違う・改竄・知らない鍵 ID は偽（第12章 10）。
func TestShortTokenSignatureAcrossKeys(t *testing.T) {
	now := wall().UTC()
	s := &Sessions{d: &Deps{Secrets: testSecrets(t, now)}}
	keys, err := s.d.Secrets.SessionKeys()
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{7}, ids.TokenBytes)
	enc := base64.RawURLEncoding
	mk := func(k secret.KeyWithId, kind string) string {
		return k.Id + "." + enc.EncodeToString(nonce) + "." + enc.EncodeToString(shortTokenMac(k.Key, kind, nonce))
	}
	for _, k := range keys {
		if ok, err := s.signedShort(mk(k, shortWs), shortWs); err != nil || !ok {
			t.Fatalf("鍵 %s のトークンが通らない: %v", k.Id, err)
		}
	}
	good := mk(keys[1], shortWs)
	bad := []string{
		mk(keys[1], shortShell),                             // 種類が違う
		"k-none" + strings.TrimPrefix(good, keys[1].Id),     // 知らない鍵 ID
		good[:len(good)-2] + "AA",                           // 署名の改竄
		keys[1].Id + ".AAAA." + strings.Split(good, ".")[2], // 乱数の長さが違う
		"no-dots", keys[1].Id + "." + enc.EncodeToString(nonce), // 形が違う
	}
	for _, v := range bad {
		if ok, _ := s.signedShort(v, shortWs); ok {
			t.Fatalf("%q を通した", v)
		}
	}
}

// R: R-2-1-1
// 一覧の cursor（第2章 3）：after だけと両方は進む向き（新しい側）に残りがあれば has_more、無い時と before は古い側。
// 返す並びは古い順。cursor が一覧に無ければ NET-001。
func TestPageInMemoryCursor(t *testing.T) {
	all := []string{"a", "b", "c", "d", "e"}
	key := func(s string) string { return s }
	two := 2
	s := func(v string) *string { return &v }
	cases := []struct {
		before, after *string
		want          string
		more          bool
	}{
		{nil, nil, "de", true},
		{nil, s("a"), "bc", true},
		{nil, s("c"), "de", false},
		{s("d"), nil, "bc", true},
		{s("c"), nil, "ab", false},
		{s("e"), s("a"), "bc", true},
		{s("d"), s("a"), "bc", false},
	}
	for _, c := range cases {
		got, more, err := pageInMemory(all, key, &two, c.before, c.after)
		if err != nil || strings.Join(got, "") != c.want || more != c.more {
			t.Fatalf("before=%v after=%v: %v %v %v", c.before, c.after, got, more, err)
		}
	}
	if _, _, err := pageInMemory(all, key, &two, nil, s("z")); codeOf(err) != cgen.NET001 {
		t.Fatalf("一覧に無い cursor が NET-001 でない: %v", err)
	}
	items, more := finish([]int{5, 4, 3}, newCursor[int](&two, nil, nil))
	if !more || len(items) != 2 || items[0] != 4 || items[1] != 5 {
		t.Fatalf("新しい側から読んだ結果を古い順に並べ直さない: %v %v", items, more)
	}
	empty, _ := finish([]int(nil), newCursor[int](nil, nil, nil))
	if empty == nil {
		t.Fatal("空の一覧が nil（JSON の null になる）")
	}
}

// newBare は DB に触れない組み立て（Ready と hook の試験）。
func newBare(t *testing.T) *Identity {
	t.Helper()
	clk := clock.NewTestClock(wall())
	return New(Deps{Pool: &store.Pool{}, Feed: store.NewFeed(nil), Jobs: &store.Jobs{}, Retention: &store.Retention{},
		Ids: ids.New(clk, rand.Reader), Envelope: envelope.New(rand.Reader), Secrets: testSecrets(t, clk.Now()), Clock: clk,
		Config: testConfig(t, t.TempDir())})
}

// R: R-7-6-1
// Ready は上のブロックの interface と OnNotice の登録が揃うまで Fault（知らせを黙って捨てない）。Ready の後の登録は panic。
// 登録の無い Notify は Fault。必須の Deps が無ければ New が panic。
func TestReadyRequiresInterfacesAndNoticeHook(t *testing.T) {
	id := newBare(t)
	if err := id.Ready(); err == nil {
		t.Fatal("interface の無い Ready が通った")
	}
	id.SetKeys(fakeKeyReader{})
	id.SetPush(fakePush{})
	id.SetAudit(&fakeAudit{})
	id.SetState(fakeState{})
	var f *fault.Fault
	if err := id.Ready(); !errors.As(err, &f) || !errors.Is(f.Cause, errNoNoticeHook) {
		t.Fatalf("OnNotice の登録の無い Ready が Fault でない: %v", err)
	}
	if err := id.Hooks.Notify(context.Background(), nil, Notice{Kind: noticeUpdateFailed}); !errors.As(err, &f) {
		t.Fatalf("登録の無い Notify が Fault でない: %v", err)
	}
	var got []Notice
	id.Hooks.RegisterOnNotice(func(_ context.Context, _ *store.Tx, n Notice) error { got = append(got, n); return nil })
	if err := id.Ready(); err != nil {
		t.Fatal(err)
	}
	if err := id.Hooks.Notify(context.Background(), nil, Notice{Kind: noticeUpdateFailed}); err != nil || len(got) != 1 {
		t.Fatalf("Notify が登録へ渡らない: %v %d", err, len(got))
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Ready の後の登録が panic しない")
			}
		}()
		id.Hooks.RegisterOnDeviceRevoked(func(context.Context, *store.Tx, uuid.UUID) error { return nil })
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("必須の Deps の無い New が panic しない")
			}
		}()
		New(Deps{})
	}()
}

// R: R-7-6-1
// 知らせの宛先を構成員ごとに出す時は同じ人に 1 回（新旧の保護者が重なる時。第7章 9）。hook は登録の順に呼び、最初の誤りで止まる。
func TestNotifyEachOnceAndHookOrder(t *testing.T) {
	h := &Hooks{}
	var order []string
	var to []uuid.UUID
	h.RegisterOnNotice(func(_ context.Context, _ *store.Tx, n Notice) error {
		order = append(order, "a")
		to = append(to, *n.Audience.UserId)
		return nil
	})
	stop := errors.New("止める")
	h.RegisterOnNotice(func(context.Context, *store.Tx, Notice) error { order = append(order, "b"); return stop })
	h.RegisterOnNotice(func(context.Context, *store.Tx, Notice) error { order = append(order, "c"); return nil })
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := h.notifyEach(context.Background(), nil, []uuid.UUID{a, b, a}, noticeGuardiansChanged, nil); !errors.Is(err, stop) {
		t.Fatalf("最初の誤りで止まらない: %v", err)
	}
	if strings.Join(order, "") != "ab" || len(to) != 1 || to[0] != a {
		t.Fatalf("登録の順に呼び誤りで止まる形でない: %v %v", order, to)
	}
	h2 := &Hooks{}
	to = nil
	h2.RegisterOnNotice(func(_ context.Context, _ *store.Tx, n Notice) error { to = append(to, *n.Audience.UserId); return nil })
	if err := h2.notifyEach(context.Background(), nil, []uuid.UUID{a, b, a, b}, noticeGuardiansChanged, nil); err != nil || len(to) != 2 {
		t.Fatalf("同じ人に 2 回出した: %v %v", to, err)
	}
}

// R: R-3-3-3
// AUTH-005 の retryable は状態で決まる（authorization_pending と slow_down は真、expired は偽。第2章 9.2）。
func TestQrEventRetryable(t *testing.T) {
	for state, want := range map[string]bool{qrPending: true, qrSlowDown: true, qrExpired: false} {
		e := qrEvent(state)
		if e.Code != cgen.AUTH005 || e.Retryable != want || e.Params["state"] != state {
			t.Fatalf("%s: %+v", state, e)
		}
	}
}

// R: R-12-4-1
// 明示の版は notice と outbound を鍵の順に並べ HTML の escape をしない JSON の SHA-256 の先頭 16 文字（第12章 7.1）。
// 文が 1 字変われば版が変わる。
func TestDisclosureVersionIsCanonical(t *testing.T) {
	var doc DisclosureDoc
	doc.Notice.Title = "題"
	doc.Notice.Lead = "<管理者> & 家族"
	doc.Notice.Items = []string{"一"}
	doc.Outbound = append(doc.Outbound, struct {
		Content     string `json:"content"`
		Destination string `json:"destination"`
		Purpose     string `json:"purpose"`
	}{"c", "d", "p"})
	v, err := disclosureVersion(doc)
	if err != nil {
		t.Fatal(err)
	}
	canon := `{"notice":{"items":["一"],"lead":"<管理者> & 家族","title":"題"},"outbound":[{"content":"c","destination":"d","purpose":"p"}]}`
	sum := sha256.Sum256([]byte(canon))
	if want := hex.EncodeToString(sum[:])[:16]; v != want {
		t.Fatalf("版が正規化した JSON の SHA-256 でない: %s %s", v, want)
	}
	doc.Notice.Items[0] = "二"
	if v2, _ := disclosureVersion(doc); v2 == v {
		t.Fatal("文が変わっても版が変わらない")
	}
	src, err := disclosureOnce()
	if err != nil || src.Title == "" || len(src.Items) == 0 || len(src.Outbound) == 0 {
		t.Fatalf("生成の明示の文が読めない: %v", err)
	}
}

// R: R-12-3-2
// 拡張の出力の読み（go-webauthn v0.18.2）：`prf: {}` は非 nil に復号され（ログインは prf があれば空でも拒む根拠）、
// results がある時だけ hasPrfResults が真（登録は results だけを拒む。第12章 5.4 (e)）。
func TestPrfOutputsDecoding(t *testing.T) {
	var empty, enabled, results protocol.AuthenticationExtensionsClientOutputs
	for raw, dst := range map[string]*protocol.AuthenticationExtensionsClientOutputs{
		`{"prf":{}}`: &empty, `{"prf":{"enabled":true}}`: &enabled, `{"prf":{"results":{"first":"AAAA"}}}`: &results} {
		if err := json.Unmarshal([]byte(raw), dst); err != nil {
			t.Fatal(err)
		}
	}
	if empty.PRF == nil {
		t.Fatal("空の prf が nil に復号された（ログインの拒否の前提が崩れる）")
	}
	if hasPrfResults(empty) || hasPrfResults(enabled) || !hasPrfResults(results) {
		t.Fatal("hasPrfResults が results の有無を見ない")
	}
	if got := deviceProofMessage([]byte{1, 2}); !bytes.Equal(got, append([]byte("ft.device_proof/1\x00"), 1, 2)) {
		t.Fatalf("端末の証の署名の対象が違う: %x", got)
	}
}

// R: R-8-1-1
// アイコン：大きさどおりの PNG。maskable は全面が不透明で、安全域（中心から半径 40%）の外は家族の色の地だけ。
// monochrome は白とアルファだけ。any は透明の地に家族の色（第8章 3.1、W3C Web App Manifest の maskable）。
func TestIconRenderSafeZone(t *testing.T) {
	ic := &Icon{dir: t.TempDir()}
	const size = 96
	for _, v := range []IconVariant{IconAny, IconMaskable, IconMonochrome} {
		b, err := ic.Render("#1F5FAD", size, v)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != size || img.Bounds().Dy() != size {
			t.Fatalf("%s の PNG の大きさが違う: %v", v, err)
		}
		inked := 0
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				r, g, bl, a := img.At(x, y).RGBA()
				dx, dy := float64(x)+0.5-size/2.0, float64(y)+0.5-size/2.0
				switch v {
				case IconMaskable:
					if a != 0xffff {
						t.Fatalf("maskable に透明の画素がある: %d,%d", x, y)
					}
					if math.Hypot(dx, dy) > 0.4*size && (r>>8 != 0x1F || g>>8 != 0x5F || bl>>8 != 0xAD) {
						t.Fatalf("maskable の安全域の外に絵柄がある: %d,%d", x, y)
					}
					if r>>8 == 0xff {
						inked++
					}
				case IconMonochrome:
					if a != 0 && (r != a || g != a || bl != a) {
						t.Fatalf("monochrome に白以外の色がある: %d,%d", x, y)
					}
					if a != 0 {
						inked++
					}
				case IconAny:
					if a == 0xffff {
						inked++
						if r>>8 != 0x1F || g>>8 != 0x5F || bl>>8 != 0xAD {
							t.Fatalf("any の絵柄が家族の色でない: %d,%d", x, y)
						}
					}
				}
			}
		}
		if inked == 0 {
			t.Fatalf("%s に絵柄が無い", v)
		}
	}
	if _, err := ic.Render("1F5FAD", size, IconAny); err == nil {
		t.Fatal("# の無い色を通した")
	}
	if err := ic.ensure("#1F5FAD", 192, IconAny); err != nil {
		t.Fatal(err)
	}
	if err := ic.ensure("#2E7D32", 512, IconMaskable); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ic.dir, iconName(192, IconAny))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("色が変わっても前の色のアイコンを消さない")
	}
}
