// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package identity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/deps"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/envelope"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ratelimit"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 試験の DB：移行を済ませた雛形（itTemplate）から試験ごとに新しい DB を作る（家族の 1 行と管理者の有無が試験の間で混ざらない）。
const itTemplate = "ft_identity_tpl"

var (
	itInfo  store.ConnInfo
	itAdmin *store.Pool
	itSeq   atomic.Int64
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	c, err := tcpg.Run(ctx, "postgres:18", tcpg.WithDatabase(store.DbName), tcpg.WithUsername(store.DbUser),
		tcpg.WithPassword("test-pw"), tcpg.BasicWaitStrategies())
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres を起こせない:", err)
		os.Exit(1)
	}
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "5432/tcp")
	p, _ := strconv.Atoi(port.Port())
	itInfo = store.ConnInfo{Host: host, Port: uint16(p), Database: store.DbName, User: store.DbUser, Password: []byte("test-pw")}
	if itAdmin, err = store.Open(ctx, itInfo); err != nil {
		fmt.Fprintln(os.Stderr, "接続できない:", err)
		os.Exit(1)
	}
	if _, err := itAdmin.Exec(ctx, `CREATE DATABASE `+itTemplate); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	info := itInfo
	info.Database = itTemplate
	tpl, err := store.Open(ctx, info)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := store.NewMigrator(tpl).Apply(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "移行できない:", err)
		os.Exit(1)
	}
	tpl.Close()
	code := m.Run()
	itAdmin.Close()
	_ = c.Terminate(ctx)
	os.Exit(code)
}

// freshPool は雛形から新しい DB を作って開く。
func freshPool(t *testing.T) *store.Pool {
	t.Helper()
	ctx := context.Background()
	name := "ft_it_" + strconv.FormatInt(itSeq.Add(1), 10)
	if _, err := itAdmin.Exec(ctx, `CREATE DATABASE `+name+` TEMPLATE `+itTemplate); err != nil {
		t.Fatal(err)
	}
	info := itInfo
	info.Database = name
	p, err := store.Open(ctx, info)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// noticeRec はコミットされた知らせの 1 件。
type noticeRec struct {
	kind     string
	audience AudienceKind
	user     uuid.UUID
	params   map[string]any
}

// noticeLog は hook OnNotice に渡った知らせのうち、コミットされた物だけを覚える（rollback の知らせは数えない）。
type noticeLog struct {
	mu   sync.Mutex
	rows []noticeRec
}

func (l *noticeLog) hook(_ context.Context, tx *store.Tx, n Notice) error {
	r := noticeRec{kind: n.Kind, audience: n.Audience.Kind, params: n.Params}
	if n.Audience.UserId != nil {
		r.user = *n.Audience.UserId
	}
	tx.AfterCommit(func() {
		l.mu.Lock()
		l.rows = append(l.rows, r)
		l.mu.Unlock()
	})
	return nil
}

// of は kind の知らせ（出た順）。
func (l *noticeLog) of(kind string) []noticeRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []noticeRec
	for _, r := range l.rows {
		if r.kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// to は kind の知らせの宛先を「audience:利用者」の並びにする（利用者が無ければ audience だけ）。
func (l *noticeLog) to(kind string) []string {
	var out []string
	for _, r := range l.of(kind) {
		s := string(r.audience)
		if r.user != uuid.Nil {
			s += ":" + r.user.String()
		}
		out = append(out, s)
	}
	return out
}

// hookLog は本ブロックの hook の呼び出し（コミットされた物）。
type hookLog struct {
	mu   sync.Mutex
	rows []string
}

func (l *hookLog) add(tx *store.Tx, s string) {
	tx.AfterCommit(func() {
		l.mu.Lock()
		l.rows = append(l.rows, s)
		l.mu.Unlock()
	})
}

func (l *hookLog) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.rows {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// dbLayer は設定の DB の層の偽物（管理者の画面の設定の代わり）。
type dbLayer map[string]config.Value

func (d dbLayer) Get(key string) (config.Value, bool) {
	v, ok := d[key]
	return v, ok
}

// env は 1 つの試験の家族のサーバー（本ブロックと下のブロックは本物、上のブロックは偽物）。
type env struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	clk     *clock.TestClock
	cfg     *config.Config
	id      *Identity
	feed    *store.Feed
	pubs    *sync.Map
	audit   *fakeAudit
	notices *noticeLog
	hooks   *hookLog
	auth    *authenticator
	dataDir string
}

// newEnv は家族のサーバーを組む。extra は Ready の前に追加の hook を登録する（同時の試験の待ち合わせ）。
func newEnv(t *testing.T, extra ...func(h *Hooks)) *env {
	t.Helper()
	pool := freshPool(t)
	clk := clock.NewTestClock(wall())
	dataDir := t.TempDir()
	cfg := testConfig(t, dataDir)
	ix := ids.New(clk, rand.Reader)
	e := &env{t: t, ctx: context.Background(), pool: pool, clk: clk, cfg: cfg, feed: store.NewFeed(pool), pubs: &sync.Map{},
		audit: &fakeAudit{}, notices: &noticeLog{}, hooks: &hookLog{}, auth: newAuthenticator(), dataDir: dataDir}
	e.id = New(Deps{Pool: pool, Feed: e.feed, Jobs: store.NewJobs(pool, ix, clk), Retention: store.NewRetention(pool), Ids: ix,
		Envelope: envelope.New(rand.Reader), Secrets: testSecrets(t, clk.Now()), Clock: clk, Config: cfg})
	e.id.SetKeys(fakeKeyReader{pubs: e.pubs})
	e.id.SetPush(fakePush{})
	e.id.SetAudit(e.audit)
	e.id.SetState(fakeState{})
	h := e.id.Hooks
	h.RegisterOnNotice(e.notices.hook)
	h.RegisterOnDeviceRevoked(func(_ context.Context, tx *store.Tx, d uuid.UUID) error {
		e.hooks.add(tx, "device_revoked:"+d.String())
		return nil
	})
	h.RegisterOnUserSuspended(func(_ context.Context, tx *store.Tx, u uuid.UUID) error {
		e.hooks.add(tx, "suspended:"+u.String())
		return nil
	})
	h.RegisterOnUserDeleted(func(_ context.Context, tx *store.Tx, u uuid.UUID, erase bool) error {
		e.hooks.add(tx, "deleted:"+u.String()+":"+strconv.FormatBool(erase))
		return nil
	})
	h.RegisterOnUserErased(func(_ context.Context, tx *store.Tx, u uuid.UUID) error {
		e.hooks.add(tx, "erased:"+u.String())
		return nil
	})
	h.RegisterOnUserCreated(func(_ context.Context, tx *store.Tx, u, d uuid.UUID) error {
		e.hooks.add(tx, "created:"+u.String()+":"+d.String())
		return nil
	})
	h.RegisterOnPasskeyDeleted(func(_ context.Context, tx *store.Tx, u uuid.UUID, c []byte) error {
		e.hooks.add(tx, "passkey_deleted:"+u.String()+":"+base64.RawURLEncoding.EncodeToString(c))
		return nil
	})
	h.RegisterOnPasskeyReset(func(_ context.Context, tx *store.Tx, u, d uuid.UUID) error {
		e.hooks.add(tx, "passkey_reset:"+u.String()+":"+d.String())
		return nil
	})
	for _, fn := range extra {
		fn(h)
	}
	if err := e.id.Ready(); err != nil {
		t.Fatal(err)
	}
	return e
}

// setConfig は設定の DB の層を置き換える（auth.members_can_invite など）。
func (e *env) setConfig(kv map[string]any) {
	e.t.Helper()
	layer := dbLayer{}
	for k, v := range kv {
		val, err := config.NewValue(v)
		if err != nil {
			e.t.Fatal(err)
		}
		layer[k] = val
	}
	if err := e.cfg.Reload(layer); err != nil {
		e.t.Fatal(err)
	}
}

// tx は fn を 1 つのトランザクションで走らせ、誤りなら rollback して返す（要求のトランザクションの代わり）。
func (e *env) tx(fn func(tx *store.Tx) error) error {
	return e.pool.InTx(e.ctx, fn)
}

// must は誤りで試験を止める。
func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("予期しない誤り: %v", err)
	}
}

// wantCode は err が番号 code の Event であることを確かめる。
func (e *env) wantCode(err error, code string) *fault.Event {
	e.t.Helper()
	var ev *fault.Event
	if !errors.As(err, &ev) || ev.Code != code {
		e.t.Fatalf("%s を期待したが %v", code, err)
	}
	return ev
}

// member は試験の構成員（端末 1 台、セッション 1 つ、パスキー 1 つ）。
type member struct {
	id     uuid.UUID
	device uuid.UUID
	cookie string
	cred   *vcred
	devKey *ecdsa.PrivateKey
	name   string
}

// regOptions は登録の options を作り、challenge の識別子と options を返す。
func (e *env) regOptions(p *boundary.Principal, tok *RegToken) (uuid.UUID, gen.WebAuthnOptions) {
	e.t.Helper()
	ch, err := e.id.Passkeys.RegisterOptions(e.ctx, p, tok)
	e.must(err)
	return uuid.FromStringOrNil(ch.ChallengeId), ch.Options
}

// setupAdmin は設置のリンクで家族と最初の管理者を作る（S-02a）。
func (e *env) setupAdmin(name string) *member {
	e.t.Helper()
	var token string
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		token, err = e.id.Setup.Create(e.ctx, tx)
		return err
	}))
	cid, opts := e.regOptions(nil, &RegToken{Kind: ChallengeSetup, Token: token})
	resp, cred := e.auth.create(e.t, opts, credOpts{})
	var res AcceptResult
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		res, err = e.id.Setup.Consume(e.ctx, tx, token, cid, "山田家", name, resp, NewDevice{Name: name + "の電話", Platform: PlatformIphone})
		return err
	}))
	return e.adopt(name, res, cred)
}

// adopt は結果から member を作り、端末の署名鍵を偽の srv-keys に置く。
func (e *env) adopt(name string, res AcceptResult, cred *vcred) *member {
	e.t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	e.must(err)
	e.pubs.Store(res.Device.Id, &k.PublicKey)
	return &member{id: res.User.Id, device: res.Device.Id, cookie: res.Issued.Cookie, cred: cred, devKey: k, name: name}
}

// issueInvite は by が招待を出しトークンを返す。
func (e *env) issueInvite(by *member, spec InviteSpec) string {
	e.t.Helper()
	var tok string
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		_, tok, err = e.id.Invites.Issue(e.ctx, tx, by.id, spec)
		return err
	}))
	return tok
}

// accept は招待を受けて構成員を作る（S-03a）。
func (e *env) accept(token, name string) *member {
	e.t.Helper()
	m, err := e.tryAccept(token, name)
	e.must(err)
	return m
}

func (e *env) tryAccept(token, name string) (*member, error) {
	ch, err := e.id.Passkeys.RegisterOptions(e.ctx, nil, &RegToken{Kind: ChallengeInvite, Token: token})
	if err != nil {
		return nil, err
	}
	cid := uuid.FromStringOrNil(ch.ChallengeId)
	resp, cred := e.auth.create(e.t, ch.Options, credOpts{})
	var res AcceptResult
	err = e.tx(func(tx *store.Tx) error {
		var err error
		res, err = e.id.Invites.Accept(e.ctx, tx, token, cid, name, resp, NewDevice{Name: name + "の電話", Platform: PlatformAndroid})
		return err
	})
	if err != nil {
		return nil, err
	}
	return e.adopt(name, res, cred), nil
}

// join は by の招待で構成員を作り、端末を承認する（srv-keys の S-32 の代わり）。child は子ども区分の指定。
func (e *env) join(by *member, name string, child *ChildSpec) *member {
	e.t.Helper()
	m := e.accept(e.issueInvite(by, InviteSpec{Role: boundary.RoleMember, Child: child}), name)
	e.approve(m.device, by.id)
	return m
}

// approve は端末を承認済みにする。
func (e *env) approve(device, by uuid.UUID) {
	e.t.Helper()
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.Approve(e.ctx, tx, device, by) }))
}

// principal は Cookie を本物の Verify で確かめた主体。
func (e *env) principal(m *member) boundary.Principal {
	e.t.Helper()
	p, ev := e.id.Sessions.Verify(e.ctx, m.cookie)
	if ev != nil {
		e.t.Fatalf("セッションが通らない: %v", ev)
	}
	return p
}

// as は主体を ctx に置く（srv-boundary の Session の middleware の代わり）。
func (e *env) as(m *member) context.Context {
	e.t.Helper()
	return boundary.WithPrincipal(e.ctx, e.principal(m))
}

// feedOf は利用者の feed（端末 device から見える行。uuid.Nil なら全部）。
func (e *env) feedOf(user, device uuid.UUID) []store.FeedRow {
	e.t.Helper()
	rows, err := e.feed.ReadSince(e.ctx, user, device, 0, 1000)
	e.must(err)
	return rows
}

// kinds は feed の行の種類の並び。
func kinds(rows []store.FeedRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Ref.Kind
	}
	return out
}

// scalar は 1 つの値を読む。
func (e *env) scalar(dst any, sql string, args ...any) {
	e.t.Helper()
	e.must(e.pool.QueryRow(e.ctx, sql, args...).Scan(dst))
}

// count は行の数。
func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	e.scalar(&n, sql, args...)
	return n
}

// within は fn を別の goroutine で走らせ、d の間に終わらなければ試験を止める（待ち合いで止まらないことの確かめ）。
func within[T any](t *testing.T, d time.Duration, fn func() T) T {
	t.Helper()
	ch := make(chan T, 1)
	go func() { ch <- fn() }()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("%v の間に終わらない（待ち合いで止まった）", d)
		var zero T
		return zero
	}
}

// actions は監査の操作の名の並び。
func (a *fakeAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.rows))
	for i, r := range a.rows {
		out[i] = r.action
	}
	return out
}

// 仮の認証器（WebAuthn Level 3 の認証器の振る舞いを試験の中で作る。attestation は none、鍵は ES256）。
// 応答は WebAuthn の toJSON の形（base64url）で、go-webauthn v0.18.2 の ParseCredentialCreationResponseBytes と
// ParseCredentialRequestResponseBytes がそのまま読む。

// vcred は認証器の中の 1 つの資格情報。
type vcred struct {
	id     []byte
	key    *ecdsa.PrivateKey
	handle []byte
	count  uint32
	be, bs bool
}

// credOpts は作る資格情報の性質と、応答の拡張の出力。
type credOpts struct {
	notDiscoverable bool           // credProps.rk を偽にする
	ext             map[string]any // clientExtensionResults を置き換える（nil なら credProps.rk だけ）
	synced          bool           // BE と BS を立てる（同期されるパスキー）
}

type authenticator struct {
	rpId   string
	origin string
}

func newAuthenticator() *authenticator {
	return &authenticator{rpId: testDomain, origin: "https://" + testDomain}
}

var b64 = base64.RawURLEncoding

// optString は options の文字列の欄（入れ子は . で辿る）。
func optString(t *testing.T, opts map[string]any, path string) string {
	t.Helper()
	var cur any = opts
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("options に %s が無い: %v", path, opts)
		}
		cur = m[k]
	}
	s, ok := cur.(string)
	if !ok {
		t.Fatalf("options の %s が文字列でない: %v", path, cur)
	}
	return s
}

// clientData は clientDataJSON（WebAuthn L3 の 5.8.1）。
func (a *authenticator) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": a.origin, "crossOrigin": false})
	return b
}

// flags は認証器のデータの旗（UP、UV、BE、BS、AT）。
func flags(at, be, bs bool) byte {
	f := byte(0x01 | 0x04)
	if be {
		f |= 0x08
	}
	if bs {
		f |= 0x10
	}
	if at {
		f |= 0x40
	}
	return f
}

// create は navigator.credentials.create() の応答を作る（登録）。
func (a *authenticator) create(t *testing.T, opts gen.WebAuthnOptions, o credOpts) (gen.WebAuthnResponse, *vcred) {
	t.Helper()
	challenge := optString(t, opts, "challenge")
	handle, err := b64.DecodeString(optString(t, opts, "user.id"))
	if err != nil {
		t.Fatal(err)
	}
	if optString(t, opts, "rp.id") != a.rpId {
		t.Fatalf("rp.id が %s でない", a.rpId)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &vcred{id: make([]byte, 16), key: key, handle: handle, be: o.synced, bs: o.synced}
	_, _ = rand.Read(c.id)
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	cose := cborMap([][2][]byte{
		{cborInt(1), cborInt(2)}, {cborInt(3), cborInt(-7)}, {cborInt(-1), cborInt(1)},
		{cborInt(-2), cborBytes(pub[1:33])}, {cborInt(-3), cborBytes(pub[33:65])}})
	rpHash := sha256.Sum256([]byte(a.rpId))
	var auth bytes.Buffer
	auth.Write(rpHash[:])
	auth.WriteByte(flags(true, c.be, c.bs))
	_ = binary.Write(&auth, binary.BigEndian, uint32(0))
	auth.Write(make([]byte, 16)) // AAGUID
	_ = binary.Write(&auth, binary.BigEndian, uint16(len(c.id)))
	auth.Write(c.id)
	auth.Write(cose)
	attObj := cborMap([][2][]byte{
		{cborText("fmt"), cborText("none")}, {cborText("attStmt"), cborMap(nil)}, {cborText("authData"), cborBytes(auth.Bytes())}})
	ext := o.ext
	if ext == nil {
		ext = map[string]any{"credProps": map[string]any{"rk": !o.notDiscoverable}}
	}
	return gen.WebAuthnResponse{
		"id": b64.EncodeToString(c.id), "rawId": b64.EncodeToString(c.id), "type": "public-key", "authenticatorAttachment": "platform",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(a.clientData("webauthn.create", challenge)),
			"attestationObject": b64.EncodeToString(attObj), "transports": []string{"internal", "hybrid"}},
		"clientExtensionResults": ext,
	}, c
}

// get は navigator.credentials.get() の応答を作る（ログイン）。ext が nil なら拡張の出力は空。
func (a *authenticator) get(t *testing.T, opts gen.WebAuthnOptions, c *vcred, ext map[string]any) gen.WebAuthnResponse {
	t.Helper()
	challenge := optString(t, opts, "challenge")
	c.count++
	rpHash := sha256.Sum256([]byte(a.rpId))
	var auth bytes.Buffer
	auth.Write(rpHash[:])
	auth.WriteByte(flags(false, c.be, c.bs))
	_ = binary.Write(&auth, binary.BigEndian, c.count)
	cd := a.clientData("webauthn.get", challenge)
	cdHash := sha256.Sum256(cd)
	signed := sha256.Sum256(append(append([]byte{}, auth.Bytes()...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, c.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	if ext == nil {
		ext = map[string]any{}
	}
	return gen.WebAuthnResponse{
		"id": b64.EncodeToString(c.id), "rawId": b64.EncodeToString(c.id), "type": "public-key", "authenticatorAttachment": "platform",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(cd), "authenticatorData": b64.EncodeToString(auth.Bytes()),
			"signature": b64.EncodeToString(sig), "userHandle": b64.EncodeToString(c.handle)},
		"clientExtensionResults": ext,
	}
}

// CBOR（RFC 8949）の符号化のうち、attestation none と COSE の鍵に要る形だけ（最短の長さの決まった形）。

func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 1<<8:
		return []byte{major<<5 | 24, byte(n)}
	default:
		return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
	}
}

func cborInt(v int) []byte {
	if v >= 0 {
		return cborHead(0, uint64(v))
	}
	return cborHead(1, uint64(-1-v))
}

func cborBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }

func cborText(s string) []byte { return append(cborHead(3, uint64(len(s))), s...) }

// cborMap は鍵と値の組を並べた順に符号化する（呼ぶ側が決まった順に並べる）。
func cborMap(pairs [][2][]byte) []byte {
	out := cborHead(5, uint64(len(pairs)))
	for _, p := range pairs {
		out = append(append(out, p[0]...), p[1]...)
	}
	return out
}

// login は m の資格情報でログインする。proof が真なら端末の証（device_hint と device_sig）を添える。
func (e *env) login(m *member, proof bool, ext map[string]any) (Issued, error) {
	e.t.Helper()
	ch, err := e.id.Passkeys.LoginOptions(e.ctx)
	e.must(err)
	resp := e.auth.get(e.t, ch.Options, m.cred, ext)
	var dp *DeviceProof
	if proof {
		chal, err := b64.DecodeString(optString(e.t, ch.Options, "challenge"))
		e.must(err)
		h := sha256.Sum256(deviceProofMessage(chal))
		sig, err := ecdsa.SignASN1(rand.Reader, m.devKey, h[:])
		e.must(err)
		dp = &DeviceProof{DeviceId: m.device, Sig: sig}
	}
	var is Issued
	err = e.tx(func(tx *store.Tx) error {
		var err error
		is, err = e.id.Passkeys.LoginVerify(e.ctx, tx, uuid.FromStringOrNil(ch.ChallengeId), resp, dp, NewDevice{Name: "新しい端末", Platform: PlatformPc})
		return err
	})
	return is, err
}

// HTTP：本ブロックの Apis と Verifiers を本物の srv-boundary（要求と応答の検証、Cookie、冪等、レート制限）に通す。

type otherImpl struct{ gen.StrictServerInterface }

type noUpWs struct{}

func (noUpWs) Verify(context.Context, string) (uuid.UUID, *fault.Event) {
	return uuid.Nil, fault.New(cgen.AUTH001, nil)
}

type noRooms struct{}

func (noRooms) IsMember(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil }
func (noRooms) RoomRole(context.Context, uuid.UUID, uuid.UUID) (boundary.RoomRole, error) {
	return boundary.RoomRoleNone, nil
}

type noLinks struct{}

func (noLinks) Snapshot(context.Context, uuid.UUID) (gen.SyncSnapshot, int64, *fault.Event) {
	return gen.SyncSnapshot{Rooms: []gen.RoomSummary{}, Users: []gen.User{}, Devices: []gen.Device{}}, 0, nil
}
func (noLinks) ForFeed(context.Context, uuid.UUID, uuid.UUID, []uuid.UUID) (map[uuid.UUID]json.RawMessage, error) {
	return map[uuid.UUID]json.RawMessage{}, nil
}
func (noLinks) Set(uuid.UUID, uuid.UUID, bool, time.Duration)                    {}
func (noLinks) Pending(context.Context, uuid.UUID) ([]boundary.UpMessage, error) { return nil, nil }
func (noLinks) Ack(context.Context, uuid.UUID, uuid.UUID) error                  { return nil }
func (noLinks) Probe(context.Context) (boundary.HealthFacts, error) {
	return boundary.HealthFacts{CaddyCertDays: 60, DiskFreePct: 50, Mode: "A"}, nil
}

// web は HTTP の家族のサーバー（httptest）。
type web struct {
	e   *env
	srv *httptest.Server
}

// serve は e の本ブロックを srv-boundary に組み込んで HTTP で受ける。
func (e *env) serve() *web {
	e.t.Helper()
	dir := e.t.TempDir()
	e.must(os.MkdirAll(filepath.Join(dir, "dl"), 0o700))
	e.must(os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<script nonce="__FT_NONCE__"></script>`), 0o600))
	e.must(os.WriteFile(filepath.Join(dir, "dl", "version.json"), []byte(`{"v":1,"server":{"version":"1.2.3","released_at":"2026-10-01T00:00:00Z"},"boundary":"0.1.0","min_app":{"web":"1.0.0","android":"1.0.0"}}`), 0o600))
	ix := ids.New(e.clk, rand.Reader)
	base := boundary.Base{Config: e.cfg, Deps: deps.Deps{Clock: e.clk}, Log: logx.New(io.Discard, "api", logx.LevelInfo, e.clk.Now),
		Limiter: ratelimit.New(e.clk, cgen.Policies), Ids: ix, Pool: e.pool, Feed: e.feed, Notifier: store.NewNotifier(e.pool, e.clk),
		Idempotency: store.NewIdempotency(e.pool, e.clk), Jobs: store.NewJobs(e.pool, ix, e.clk), WwwDir: dir, ValidateResponses: true}
	v := e.id.Verifiers()
	apis := e.id.Apis()
	other := otherImpl{}
	impls := gen.Implementations{Setup: apis.Setup, Invites: apis.Invites, Auth: apis.Auth, Me: apis.Me, Family: apis.Family, Users: apis.Users,
		Devices: apis.Devices, Keys: other, Rooms: other, Media: other, Calls: other, Push: other, Admin: other, Shell: other}
	s := boundary.NewServer()
	h, err := s.Build(base, impls, boundary.Verifiers{Session: v.Session, Shell: v.Shell, Ws: v.Ws, UpWs: noUpWs{}, Membership: noRooms{},
		State: fakeState{}}, boundary.Links{Snapshot: noLinks{}, Events: noLinks{}, Typing: noLinks{}, Up: noLinks{}, Probe: noLinks{}})
	e.must(err)
	srv := httptest.NewServer(h)
	e.t.Cleanup(srv.Close)
	return &web{e: e, srv: srv}
}

// do は端末の要求を送り、状態と本文と応答を返す。cookie が空なら Cookie を付けない。
func (w *web) do(method, path, cookie string, body any, hdr map[string]string) (*http.Response, []byte) {
	w.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		w.e.must(err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, rd)
	w.e.must(err)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	req.Header.Set("X-Requested-With", "family-talk")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	w.e.must(err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// problem は problem+json の番号と状態を確かめる。
func (w *web) problem(resp *http.Response, body []byte, status int, code string) map[string]any {
	w.e.t.Helper()
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil || resp.StatusCode != status || p["code"] != code {
		w.e.t.Fatalf("%d %s を期待したが %d %s", status, code, resp.StatusCode, body)
	}
	return p
}

// sessionCookie は応答の Set-Cookie の __Host-session の値。
func sessionCookie(t *testing.T, resp *http.Response) string {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return c.Value
		}
	}
	t.Fatalf("Set-Cookie に %s が無い: %v", cookieName, resp.Header.Values("Set-Cookie"))
	return ""
}

// newKey は Idempotency-Key（UUIDv7）。
func newKey() string { return uuid.Must(uuid.NewV7()).String() }

// mustAddr は IP の文字列を読む。
func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

// boundaryPrincipal は ctx の主体。
func boundaryPrincipal(ctx context.Context) boundary.Principal { return boundary.PrincipalFrom(ctx) }
