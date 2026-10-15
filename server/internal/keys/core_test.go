// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
)

// 第12章 5.3 の試験のベクトル。
var (
	vecUser    = uuid.Must(uuid.FromString("0199e1c2-0000-7000-8000-000000000001"))
	vecDevice  = uuid.Must(uuid.FromString("0199e1c2-0000-7000-8000-000000000002"))
	vecG       = gen.CommonJwkP2561{Crv: "P-256", Kty: "EC", X: "axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY", Y: "T-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU"}
	vec2G      = gen.CommonJwkP2561{Crv: "P-256", Kty: "EC", X: "fPJ7GI0DT36KUjgDBLUaw8CJaeJ38hs1pgtI_EdmmXg", Y: "B3dVENuO0EApPZrGn3Qw27p9reY86YIpngS3nSJ4c9E"}
	vec3G      = gen.CommonJwkP2561{Crv: "P-256", Kty: "EC", X: "Xsvk0aYzCkTI9--VHUvxZebGtyHvramF-0FmG8bn_Ww", Y: "hzRkDEmY_343SwbOGmSi7NgqsDY4T7g9mnmxJ6J9UDI"}
	vecUserSig = "NQmuB28fr3taQAYp_b_iTQSIKoVXcTlU0v3VbM2MjjW0K6VsXjsvmBiC0N13IS2RHp4AEIlMIfLSkMLA3sRs_A"
)

// R: R-12-3-2
// 第12章 5.3 のベクトル：user_sig の対象（179 バイト）の SHA-256、示した署名が 3G で通り、本人の識別子を 1 バイト変えると通らない。
// mls_credential の 16 進と base64url。DeviceKeys.VerifyUserSig が fdev/1 と fusr/1 だけで確かめる。
func TestUserSigAndCredentialVectors(t *testing.T) {
	g, _, ok1 := jwkPoint(vecG)
	g2, _, ok2 := jwkPoint(vec2G)
	_, pub3, ok3 := jwkPoint(vec3G)
	if !ok1 || !ok2 || !ok3 {
		t.Fatal("ベクトルの JWK が点にならない")
	}
	msg := userSigMessage(vecUser, vecDevice, g, g2)
	h := sha256.Sum256(msg)
	if len(msg) != 179 || hex.EncodeToString(h[:]) != "ef88bfcaeeda45220ad62882a56ef8ed164ee5d55f427721a14553d60d897e25" {
		t.Fatalf("対象が違う: %d %x", len(msg), h)
	}
	sig, ok := decodeB64(vecUserSig)
	if !ok || !verifyRS(pub3, msg, sig) {
		t.Fatal("ベクトルの user_sig が通らない")
	}
	other := vecUser
	other[15] ^= 1
	if verifyRS(pub3, userSigMessage(other, vecDevice, g, g2), sig) {
		t.Fatal("本人の識別子を変えた対象で通る")
	}
	cred := credentialFor(vecUser, vecDevice)
	if hex.EncodeToString(cred) != "0001200199e1c20000700080000000000000010199e1c2000070008000000000000002" ||
		b64.EncodeToString(cred) != "AAEgAZnhwgAAcACAAAAAAAAAAQGZ4cIAAHAAgAAAAAAAAAI" {
		t.Fatalf("credential が違う: %x", cred)
	}
	var dk DeviceKeys
	f := gen.Fdev1{DeviceId: vecDevice.String(), UserId: vecUser.String(), SigPub: vecG, KemPub: vec2G, UserSig: &vecUserSig}
	if !dk.VerifyUserSig(f, gen.Fusr1{UserSigPub: vec3G}) {
		t.Fatal("VerifyUserSig が通らない")
	}
	if dk.VerifyUserSig(f, gen.Fusr1{UserSigPub: vecG}) {
		t.Fatal("別の鍵で VerifyUserSig が通る")
	}
	f.UserSig = nil
	if dk.VerifyUserSig(f, gen.Fusr1{UserSigPub: vec3G}) {
		t.Fatal("user_sig の無い fdev で通る")
	}
}

// R: R-12-3-2
// JWK の P-256 の点：crv と kty、x と y の長さ、曲線の外の点、無限遠点（0, 0）、詰め物のある base64url を拒む。正規の形に組み直すと同じ鍵が
// 同じ値になる。本システムの署名は r と s の 64 バイトだけ（DER は通らない）。
func TestJwkPointAndSignatures(t *testing.T) {
	bad := map[string]gen.CommonJwkP2561{
		"crv":   {Crv: "P-384", Kty: "EC", X: vecG.X, Y: vecG.Y},
		"kty":   {Crv: "P-256", Kty: "OKP", X: vecG.X, Y: vecG.Y},
		"x の長さ": {Crv: "P-256", Kty: "EC", X: vecG.X[:40], Y: vecG.Y},
		"曲線の外":  {Crv: "P-256", Kty: "EC", X: vecG.X, Y: vec2G.Y},
		"無限遠点":  {Crv: "P-256", Kty: "EC", X: b64.EncodeToString(make([]byte, 32)), Y: b64.EncodeToString(make([]byte, 32))},
		"詰め物":   {Crv: "P-256", Kty: "EC", X: vecG.X + "=", Y: vecG.Y},
	}
	for what, j := range bad {
		if _, _, ok := jwkPoint(j); ok {
			t.Fatalf("%s を拒まない", what)
		}
	}
	g, _, _ := jwkPoint(vecG)
	if canonicalJwk(g) != vecG {
		t.Fatalf("正規の形が違う: %+v", canonicalJwk(g))
	}
	k := newKey(t)
	msg := []byte("ft.inbox/1")
	rs := signRS(t, k, msg)
	if !verifyRS(&k.PublicKey, msg, rs) || verifyRS(&k.PublicKey, append(msg, 0), rs) || verifyRS(&k.PublicKey, msg, rs[:63]) {
		t.Fatal("r と s の 64 バイトの確かめが違う")
	}
	der := signLabel(t, k, "x", msg)
	if verifyRS(&k.PublicKey, msg, der) {
		t.Fatal("DER の署名が本システムの署名として通る")
	}
	gid := groupIdOf(vecUser, 0x01020304)
	if len(gid) != 20 || !bytes.Equal(gid[:16], vecUser.Bytes()) || !bytes.Equal(gid[16:], []byte{1, 2, 3, 4}) {
		t.Fatalf("グループの識別子が違う: %x", gid)
	}
}

// R: R-12-3-2
// leaf の有効期間（第12章 5.2、DD-12-18）：全長 120 日の境、置く時の not_before（今 + 1 日より前）と not_after（今 + 7 日より後）、Add の
// 時の今の時刻（not_before − 1 日 から not_after まで）。
func TestLifetime(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	n := uint64(now.Unix())
	leaf := func(nb, na uint64) ParsedLeafNode { return ParsedLeafNode{NotBefore: nb, NotAfter: na} }
	cases := []struct {
		what         string
		l            ParsedLeafNode
		push, commit bool
	}{
		{"端末の既定（1 日前から 100 日後）", leaf(n-day, n+100*day), true, true},
		{"全長ちょうど 120 日", leaf(n-day, n+119*day), true, true},
		{"全長 120 日と 1 秒", leaf(n-day, n+119*day+1), false, false},
		{"not_after が not_before より前", leaf(n+day, n), false, false},
		{"not_before が今 + 1 日（置けない、Add は可）", leaf(n+day, n+100*day), false, true},
		{"not_before が今 + 1 日 + 1 秒", leaf(n+day+1, n+100*day), false, false},
		{"not_after が今 + 7 日（置けない、Add は可）", leaf(n-day, n+7*day), false, true},
		{"not_after が今（Add は可）", leaf(n-100*day, n), false, true},
		{"not_after が今 − 1 秒", leaf(n-100*day, n-1), false, false},
	}
	for _, c := range cases {
		if got := lifetimeOk(c.l, now, true); got != c.push {
			t.Fatalf("%s：置く時 %v", c.what, got)
		}
		if got := lifetimeOk(c.l, now, false); got != c.commit {
			t.Fatalf("%s：Add の時 %v", c.what, got)
		}
	}
}

// R: R-12-3-2
// KeyPackage の保管の 9 つの確かめ（第12章 5.2）：本システムの規則で組んだ物は通り、1 つずつ壊した物は通らない。capabilities の未知の値と
// 既定の拡張は許し、既定でない拡張は capabilities に無ければ拒む。
func TestCheckKeyPackageNine(t *testing.T) {
	now := testNow()
	dk := newDevKeys(t, testUuid(1), testUuid(2))
	reg := registration{userId: dk.userId, credential: credentialFor(dk.userId, dk.deviceId), sigSec1: sec1(t, dk.sig), sigPub: &dk.sig.PublicKey}
	var cr CommitReader
	check := func(raw []byte) bool {
		kp, err := cr.ParseKeyPackage(raw)
		return err == nil && checkKeyPackage(kp, reg, now, true)
	}
	if !check(buildKeyPackage(t, dk, now, kpOpts{})) {
		t.Fatal("正しい KeyPackage が通らない")
	}
	other := newDevKeys(t, dk.userId, testUuid(3))
	broken := map[string]kpOpts{
		"1 版":              {before: func(k *keyPackage) { k.version = 2 }},
		"1 スイート":           {before: func(k *keyPackage) { k.suite = 1 }},
		"2 credential":     {before: func(k *keyPackage) { k.leaf.credential = basicCredential(dk.userId, testUuid(3)) }},
		"3 署名鍵":            {before: func(k *keyPackage) { k.leaf.signatureKey = sec1(t, other.sig) }},
		"4 source":         {before: func(k *keyPackage) { k.leaf.source = leafSourceUpdate }},
		"5 KeyPackage の署名": {after: func(k *keyPackage) { k.signature = signLabel(t, dk.sig, labelKeyPackageTbs, []byte("x")) }},
		"6 leaf の署名":       {leafSigner: other.sig},
		"7 init_key と同じ":   {before: func(k *keyPackage) { k.initKey = k.leaf.encryptionKey }},
		"8 版の能力":           {before: func(k *keyPackage) { k.leaf.capabilities.versions = []uint16{2} }},
		"8 スイートの能力":        {before: func(k *keyPackage) { k.leaf.capabilities.suites = []uint16{1} }},
		"8 basic の能力":      {before: func(k *keyPackage) { k.leaf.capabilities.credentials = []uint16{credentialX509} }},
		"8 能力に無い拡張":        {before: func(k *keyPackage) { k.leaf.extensions = []extension{{typ: 0x0f0f, data: []byte{}}} }},
		"9 全長":             {before: func(k *keyPackage) { k.leaf.notAfter = k.leaf.notBefore + lifetimeMaxTotal + 1 }},
		"9 not_after の残り":  {before: func(k *keyPackage) { k.leaf.notAfter = uint64(now.Unix()) + 6*day }},
		"別の鍵で署名":           {signer: other.sig},
	}
	for what, o := range broken {
		if check(buildKeyPackage(t, dk, now, o)) {
			t.Fatalf("%s を壊した KeyPackage が通る", what)
		}
	}
	allowed := map[string]kpOpts{
		"能力の未知の値（GREASE）": {before: func(k *keyPackage) { k.leaf.capabilities.suites = append(k.leaf.capabilities.suites, 0x0a0a) }},
		"既定の拡張":           {before: func(k *keyPackage) { k.leaf.extensions = []extension{{typ: 1, data: []byte{1}}} }},
		"能力にある既定でない拡張": {before: func(k *keyPackage) {
			k.leaf.extensions = []extension{{typ: 0x0f0f}}
			k.leaf.capabilities.extensions = []uint16{0x0f0f}
		}},
		"KeyPackage の拡張（未知）": {before: func(k *keyPackage) { k.extensions = []extension{{typ: 0x1a1a, data: []byte{7}}} }},
	}
	for what, o := range allowed {
		if !check(buildKeyPackage(t, dk, now, o)) {
			t.Fatalf("%s の KeyPackage が通らない", what)
		}
	}
}
