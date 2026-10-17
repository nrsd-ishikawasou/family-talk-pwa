// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// wantMalformed は読みの誤りか。
func wantMalformed(t *testing.T, err error, what string) {
	t.Helper()
	var m *malformed
	if !errors.As(err, &m) {
		t.Fatalf("%s：形の誤りにならない: %v", what, err)
	}
}

// wantEvent は番号の Event か。
func wantEvent(t *testing.T, err error, code, what string) {
	t.Helper()
	var e *fault.Event
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("%s：%s にならない: %v", what, code, err)
	}
}

// R: R-12-3-2
// MLS の物の読み取り（T-L1-04）：公式の相互運用の検証ベクトル messages.json の 300 件の MLSMessage 7 種と単独の構造を、RFC 9420 の
// 文法で最後のバイトまで読み、書き戻すと元のバイト列になる。
func TestVectorMessagesRoundTrip(t *testing.T) {
	var cases []map[string]string
	vectorJSON(t, "messages.json", &cases)
	if len(cases) != 300 {
		t.Fatalf("件数 %d", len(cases))
	}
	msgs := []string{"mls_welcome", "mls_group_info", "mls_key_package", "public_message_application", "public_message_proposal",
		"public_message_commit", "private_message"}
	n := 0
	for i, c := range cases {
		for _, k := range msgs {
			in := unhex(t, c[k])
			m, err := decodeMessage(in)
			if err != nil {
				t.Fatalf("%d %s を読めない: %v", i, k, err)
			}
			if out := encMessage(m); !bytes.Equal(out, in) {
				t.Fatalf("%d %s の書き戻しが違う", i, k)
			}
			n++
		}
		single := map[string]func(r *tlsReader, e *enc){
			"add_proposal":    func(r *tlsReader, e *enc) { encKeyPackage(e, readKeyPackage(r)) },
			"update_proposal": func(r *tlsReader, e *enc) { l := readLeafNode(r); encLeafNode(e, &l) },
			"remove_proposal": func(r *tlsReader, e *enc) { e.u32(r.u32()) },
			"pre_shared_key_proposal": func(r *tlsReader, e *enc) {
				k := readPreSharedKeyID(r)
				encPsk(e, &k)
			},
			"re_init_proposal": func(r *tlsReader, e *enc) {
				encProposal(e, &proposal{typ: proposalReInit, reinit: &reInit{groupId: r.opaque(), version: r.u16(), suite: r.u16(), extensions: readExtensions(r)}})
				e.b = e.b[2:]
			},
			"external_init_proposal":            func(r *tlsReader, e *enc) { e.opaque(r.opaque()) },
			"group_context_extensions_proposal": func(r *tlsReader, e *enc) { encExtensions(e, readExtensions(r)) },
			"commit":                            func(r *tlsReader, e *enc) { encCommit(e, readCommit(r)) },
		}
		for k, fn := range single {
			in := unhex(t, c[k])
			r := &tlsReader{b: in}
			e := &enc{}
			fn(r, e)
			r.end()
			if r.err != nil {
				t.Fatalf("%d %s を読めない: %v", i, k, r.err)
			}
			if !bytes.Equal(e.b, in) {
				t.Fatalf("%d %s の書き戻しが違う", i, k)
			}
			n++
		}
	}
	if n != 300*15 {
		t.Fatalf("読んだ数 %d", n)
	}
}

// R: R-12-3-2
// RefHash と SignWithLabel の形（crypto-basics.json。ハッシュが SHA-256 のスイート 1〜3 の RefHash と、スイート 2 の SignWithLabel）。
func TestVectorCryptoBasics(t *testing.T) {
	var cases []struct {
		Suite   int `json:"cipher_suite"`
		RefHash struct {
			Label, Value, Out string
		} `json:"ref_hash"`
		Sign struct {
			Label, Content, Pub, Signature string
		} `json:"sign_with_label"`
	}
	vectorJSON(t, "crypto-basics.json", &cases)
	seen := 0
	for _, c := range cases {
		if c.Suite < 1 || c.Suite > 3 {
			continue
		}
		if got := refHash(c.RefHash.Label, unhex(t, c.RefHash.Value)); !bytes.Equal(got, unhex(t, c.RefHash.Out)) {
			t.Fatalf("スイート %d の RefHash が違う", c.Suite)
		}
		if c.Suite == mlsSuite {
			pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), unhex(t, c.Sign.Pub))
			if err != nil {
				t.Fatal(err)
			}
			if !verifyWithLabel(pub, c.Sign.Label, unhex(t, c.Sign.Content), unhex(t, c.Sign.Signature)) {
				t.Fatal("スイート 2 の SignWithLabel が通らない")
			}
			if verifyWithLabel(pub, c.Sign.Label+"x", unhex(t, c.Sign.Content), unhex(t, c.Sign.Signature)) {
				t.Fatal("ラベルを変えた SignWithLabel が通る")
			}
		}
		seen++
	}
	if seen != 3 {
		t.Fatalf("スイートの数 %d", seen)
	}
}

// R: R-12-3-2
// passive-client のスイート 2：KeyPackageRef（頭 4 バイトを除いた KeyPackage の RefHash）が Welcome の new_member にあり、KeyPackage の
// 署名と leaf の署名（文脈なし）が leaf の署名鍵で通る。Commit の UpdatePath の leaf の署名（group_id と送信者の番号が文脈）も通る。
func TestVectorPassiveClient(t *testing.T) {
	type pc struct {
		Suite      int    `json:"cipher_suite"`
		KeyPackage string `json:"key_package"`
		Welcome    string `json:"welcome"`
		Epochs     []struct {
			Commit string `json:"commit"`
		} `json:"epochs"`
	}
	var r CommitReader
	kps, paths := 0, 0
	for _, name := range []string{"passive-client-welcome.json", "passive-client-handling-commit.json"} {
		var cases []pc
		vectorJSON(t, name, &cases)
		for i, c := range cases {
			if c.Suite != mlsSuite {
				continue
			}
			kp, err := r.ParseKeyPackage(unhex(t, c.KeyPackage))
			if err != nil {
				t.Fatalf("%s %d：KeyPackage を読めない: %v", name, i, err)
			}
			w, err := r.ParseWelcome(unhex(t, c.Welcome))
			if err != nil {
				t.Fatalf("%s %d：Welcome を読めない: %v", name, i, err)
			}
			if !slices.ContainsFunc(w.NewMembers, func(m []byte) bool { return bytes.Equal(m, kp.Ref) }) {
				t.Fatalf("%s %d：KeyPackageRef が Welcome の new_member に無い", name, i)
			}
			if !bytes.Equal(kp.Ref, r.KeyPackageRef(kp.Bytes)) {
				t.Fatalf("%s %d：KeyPackageRef が 2 通りで違う", name, i)
			}
			pub, ok := sec1PubOf(kp.Leaf.SigPub)
			if !ok || !kp.VerifySignature(pub) || !kp.Leaf.VerifySignature(pub, nil, 0) {
				t.Fatalf("%s %d：KeyPackage か leaf の署名が通らない", name, i)
			}
			other := newKey(t)
			if kp.VerifySignature(&other.PublicKey) {
				t.Fatalf("%s %d：別の鍵で KeyPackage の署名が通る", name, i)
			}
			kps++
			for _, ep := range c.Epochs {
				m, err := decodeMessage(unhex(t, ep.Commit))
				if err != nil {
					t.Fatalf("%s %d：Commit を読めない: %v", name, i, err)
				}
				if m.public == nil || m.public.content.commit == nil || m.public.content.commit.path == nil {
					continue
				}
				c := m.public.content
				leaf := parsedLeafNode(c.commit.path.leaf)
				pub, ok := sec1PubOf(leaf.SigPub)
				if !ok || !leaf.VerifySignature(pub, c.groupId, int(c.sender.index)) {
					t.Fatalf("%s %d：UpdatePath の leaf の署名が通らない", name, i)
				}
				if leaf.VerifySignature(pub, c.groupId, int(c.sender.index)+1) {
					t.Fatalf("%s %d：leaf の番号を変えた文脈で通る", name, i)
				}
				paths++
			}
		}
	}
	if kps == 0 || paths == 0 {
		t.Fatalf("スイート 2 の物が無い: KeyPackage %d、path %d", kps, paths)
	}
}

// sec1PubOf は SEC 1 の非圧縮の点を公開鍵にする（試験）。
func sec1PubOf(b []byte) (*ecdsa.PublicKey, bool) {
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b)
	return pub, err == nil
}

// R: R-12-3-2
// leaf の番号（RFC 9420 12.1.1、12.1.3、12.3）：tree-operations.json の 5 件で、木の前の leaf の記録に提案を当てた結果の記録が、木の後の
// leaf の並びと同じ（Add は左端の空き、無ければ幅の番号。Remove の後の右端の切り詰め）。
func TestVectorTreeOperations(t *testing.T) {
	var cases []struct {
		Proposal   string `json:"proposal"`
		Sender     uint32 `json:"proposal_sender"`
		TreeBefore string `json:"tree_before"`
		TreeAfter  string `json:"tree_after"`
	}
	vectorJSON(t, "tree-operations.json", &cases)
	if len(cases) != 5 {
		t.Fatalf("件数 %d", len(cases))
	}
	for i, c := range cases {
		before := leavesOfTree(t, unhex(t, c.TreeBefore))
		after := leavesOfTree(t, unhex(t, c.TreeAfter))
		r := &tlsReader{b: unhex(t, c.Proposal)}
		p := readProposal(r)
		r.end()
		if r.err != nil {
			t.Fatalf("%d：提案を読めない: %v", i, r.err)
		}
		rec := map[int]Leaf{}
		for idx, key := range before {
			rec[idx] = Leaf{Index: idx, EncryptionKey: key}
		}
		switch p.typ {
		case proposalRemove:
			delete(rec, int(p.remove))
		case proposalAdd:
			w := width(rec)
			j := freeLeaf(rec, &w)
			rec[j] = Leaf{Index: j, EncryptionKey: p.add.leaf.encryptionKey}
		case proposalUpdate:
			rec[int(c.Sender)] = Leaf{Index: int(c.Sender), EncryptionKey: p.update.encryptionKey}
		}
		if len(rec) != len(after) {
			t.Fatalf("%d：leaf の数が違う: %d と %d", i, len(rec), len(after))
		}
		for idx, key := range after {
			if l, ok := rec[idx]; !ok || !bytes.Equal(l.EncryptionKey, key) {
				t.Fatalf("%d：leaf %d が違う", i, idx)
			}
		}
	}
}

// leavesOfTree は ratchet_tree（optional<Node> の可変長のベクタ。RFC 9420 12.4.3.3）から、空でない leaf の番号と encryption_key。
func leavesOfTree(t *testing.T, b []byte) map[int][]byte {
	t.Helper()
	out := map[int][]byte{}
	r := &tlsReader{b: b}
	node := 0
	r.vector(func(s *tlsReader) {
		s.optional(func(n *tlsReader) {
			switch n.u8() {
			case 1:
				l := readLeafNode(n)
				out[node/2] = l.encryptionKey
			case 2:
				n.opaque()
				n.opaque()
				n.vector(func(u *tlsReader) { u.u32() })
			default:
				n.fail("node_type")
			}
		})
		node++
	})
	r.end()
	if r.err != nil {
		t.Fatalf("木を読めない: %v", r.err)
	}
	return out
}

// R: R-12-3-2
// 形の誤り（読み方の約束）：正しい物から 1 か所ずつ壊した入力が全部 KEY-009（読みは形の誤り）になる。
func TestReaderMalformed(t *testing.T) {
	var cases []map[string]string
	vectorJSON(t, "messages.json", &cases)
	good := unhex(t, cases[0]["mls_key_package"])
	if _, err := decodeMessage(good); err != nil {
		t.Fatal(err)
	}
	mut := func(f func([]byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	bad := map[string][]byte{
		"残りのバイト":   mut(func(b []byte) []byte { return append(b, 0) }),
		"途中で終わる":   mut(func(b []byte) []byte { return b[:len(b)-1] }),
		"版が違う":     mut(func(b []byte) []byte { b[1] = 2; return b }),
		"wire が無い": mut(func(b []byte) []byte { b[3] = 9; return b }),
	}
	for what, in := range bad {
		_, err := decodeMessage(in)
		wantMalformed(t, err, what)
	}
	// 可変長のベクタの長さ：最小でない符号化、先頭 2 ビットが 11。
	for what, in := range map[string][]byte{"2 バイトで 63": {0x40, 0x3f}, "4 バイトで 16383": {0x80, 0x00, 0x3f, 0xff}, "先頭 11": {0xc0}} {
		r := &tlsReader{b: in}
		r.varint()
		if r.err == nil {
			t.Fatalf("%s を拒まない", what)
		}
	}
	for what, in := range map[string][]byte{"1 バイト 63": {0x3f}, "2 バイト 64": {0x40, 0x40}, "4 バイト 16384": {0x80, 0x00, 0x40, 0x00}} {
		r := &tlsReader{b: in}
		r.varint()
		if r.err != nil || r.i != len(in) {
			t.Fatalf("%s を読めない: %v", what, r.err)
		}
	}
	// optional の先頭が 2、ベクタの中身が要素の境で終わらない、拡張の種類の重なり。
	r := &tlsReader{b: []byte{2}}
	r.optional(func(*tlsReader) {})
	if r.err == nil {
		t.Fatal("optional の 2 を拒まない")
	}
	r = &tlsReader{b: []byte{3, 0, 1, 0}}
	readU16s(r)
	if r.err == nil {
		t.Fatal("奇数の長さの u16 の並びを拒まない")
	}
	e := &enc{}
	encExtensions(e, []extension{{typ: 7, data: []byte{}}, {typ: 7, data: []byte{1}}})
	r = &tlsReader{b: e.b}
	readExtensions(r)
	if r.err == nil {
		t.Fatal("同じ拡張の種類の 2 つを拒まない")
	}
	// 表に無い種類：提案（GREASE を置いてはならない）、credential、leaf_node_source、sender。
	for what, in := range map[string][]byte{"提案 8": {0, 8}, "提案 0x0a0a": {0x0a, 0x0a}} {
		r := &tlsReader{b: in}
		readProposal(r)
		if r.err == nil {
			t.Fatalf("%s を拒まない", what)
		}
	}
	r = &tlsReader{b: []byte{0, 3, 0}}
	readCredential(r)
	if r.err == nil {
		t.Fatal("credential の種類 3 を拒まない")
	}
	r = &tlsReader{b: []byte{5}}
	readSender(r)
	if r.err == nil {
		t.Fatal("sender の種類 5 を拒まない")
	}
	// Parse の外へは KEY-009。
	var cr CommitReader
	_, err := cr.ParseKeyPackage(bad["残りのバイト"])
	wantEvent(t, err, cgen.KEY009, "ParseKeyPackage")
	_, err = cr.ParseCommit(good)
	wantEvent(t, err, cgen.KEY009, "KeyPackage を ParseCommit")
	_, err = cr.ParseWelcome(good)
	wantEvent(t, err, cgen.KEY009, "KeyPackage を ParseWelcome")
	_, err = cr.ParsePrivateHeader(good)
	wantEvent(t, err, cgen.KEY009, "KeyPackage を ParsePrivateHeader")
	gi := unhex(t, cases[0]["mls_group_info"])
	for _, f := range []func([]byte) error{
		func(b []byte) error { _, err := cr.ParseCommit(b); return err },
		func(b []byte) error { _, err := cr.ParseWelcome(b); return err },
		func(b []byte) error { _, err := cr.ParseKeyPackage(b); return err },
		func(b []byte) error { _, err := cr.ParsePrivateHeader(b); return err },
	} {
		wantEvent(t, f(gi), cgen.KEY009, "GroupInfo")
	}
}

// R: R-12-3-2
// ParseCommit：値の Add と Remove と Update の数と path を読む。参照の提案、PSK、ReInit、ExternalInit、GroupContextExtensions、member で
// ない送信者、commit でない内容は KEY-009。
func TestParseCommitShapes(t *testing.T) {
	now := testNow()
	dk := newDevKeys(t, testUuid(1), testUuid(2))
	kp := buildKeyPackage(t, dk, now, kpOpts{})
	gid := groupIdOf(testUuid(9), 0)
	var cr CommitReader
	in := buildCommit(t, commitSpec{groupId: gid, epoch: 4, sender: 1, adds: [][]byte{kp}, removes: []uint32{3, 2}, updates: 1,
		path: pathLeaf(t, dk, gid, 1, nil)})
	pc, err := cr.ParseCommit(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pc.GroupId, gid) || pc.Epoch != 4 || pc.SenderLeaf != 1 || len(pc.Adds) != 1 || !slices.Equal(pc.Removes, []int{3, 2}) ||
		pc.Updates != 1 || pc.Path == nil || pc.Path.Source != leafSourceCommit {
		t.Fatalf("読んだ物が違う: %+v", pc)
	}
	if !bytes.Equal(pc.Adds[0].Ref, refOf(t, kp)) || pc.Adds[0].Leaf.DeviceId != dk.deviceId || pc.Adds[0].Leaf.UserId != dk.userId {
		t.Fatal("Add の KeyPackage が違う")
	}
	refused := map[string]proposalOrRef{
		"参照":           {typ: proposalOrRefReference, reference: []byte{1, 2}},
		"PSK":          {typ: proposalOrRefProposal, proposal: &proposal{typ: proposalPreSharedKey, psk: &preSharedKeyID{pskType: pskExternal, pskId: []byte{1}, pskNonce: []byte{2}}}},
		"ReInit":       {typ: proposalOrRefProposal, proposal: &proposal{typ: proposalReInit, reinit: &reInit{groupId: gid, version: 1, suite: 2}}},
		"ExternalInit": {typ: proposalOrRefProposal, proposal: &proposal{typ: proposalExternalInit, externalInit: []byte{1}}},
		"GCE":          {typ: proposalOrRefProposal, proposal: &proposal{typ: proposalGroupContextExtensions}},
	}
	for what, p := range refused {
		_, err := cr.ParseCommit(buildCommit(t, commitSpec{groupId: gid, epoch: 4, sender: 1, extra: []proposalOrRef{p}}))
		wantEvent(t, err, cgen.KEY009, what)
	}
	m, _ := decodeMessage(in)
	m.public.content.sender = sender{senderType: senderExternal, index: 0}
	m.public.membershipTag = nil
	_, err = cr.ParseCommit(encMessage(m))
	wantEvent(t, err, cgen.KEY009, "external の送信者")
	app := encMessage(&mlsMessage{version: 1, wireFormat: wireFormatPublicMessage, public: &publicMessage{content: framedContent{groupId: gid,
		sender: sender{senderType: senderMember}, authenticatedData: []byte{}, contentType: contentApplication, applicationData: []byte{1}},
		auth: framedContentAuthData{signature: []byte{1}}, membershipTag: []byte{1}}})
	_, err = cr.ParseCommit(app)
	wantEvent(t, err, cgen.KEY009, "application の PublicMessage")
	kems := [][]byte{hpkeKey(t), {1, 2, 3}}
	w, err := cr.ParseWelcome(buildWelcomeKems(mlsSuite, kems, pc.Adds[0].Ref, []byte{1}))
	if err != nil || w.Suite != mlsSuite || len(w.NewMembers) != 2 || !bytes.Equal(w.NewMembers[0], pc.Adds[0].Ref) ||
		len(w.KemOutputs) != 2 || !bytes.Equal(w.KemOutputs[0], kems[0]) || !bytes.Equal(w.KemOutputs[1], kems[1]) {
		t.Fatalf("Welcome が違う: %+v %v", w, err)
	}
	w, err = cr.ParseWelcome(buildWelcome(mlsSuite, pc.Adds[0].Ref, []byte{1}))
	if err != nil || len(w.KemOutputs) != 2 || !hpkePubOk(w.KemOutputs[0]) || !hpkePubOk(w.KemOutputs[1]) {
		t.Fatalf("試験の Welcome の kem_output が P-256 の点でない: %+v %v", w, err)
	}
	h, err := cr.ParsePrivateHeader(buildPrivate(gid, 7, contentApplication))
	if err != nil || !bytes.Equal(h.GroupId, gid) || h.Epoch != 7 || h.ContentType != contentApplication {
		t.Fatalf("PrivateHeader が違う: %+v %v", h, err)
	}
	_, err = cr.ParsePrivateHeader(buildPrivate(gid, 1<<63, contentApplication))
	wantEvent(t, err, cgen.KEY009, "epoch が 2^63")
}

// R: R-12-3-2
// 一次の MLS の実装の出力（testdata/mls-primary-impl.json。本システムの設定で作った物）：非圧縮の署名鍵の KeyPackage は保管の 9 つと Add の
// 時の確かめを通り、Add の Commit は読めて Welcome の宛先が Add と一致し、path の Commit の leaf は確かめ 8 を通る。既定の作り方の圧縮の
// 署名鍵（RFC 9420 5.1.1 に反する）の KeyPackage は確かめ 3 で拒まれる。
func TestPrimaryImplementationOutput(t *testing.T) {
	type dev struct {
		User       string `json:"user"`
		Device     string `json:"device"`
		KeyPackage string `json:"key_package"`
	}
	var f struct {
		A          dev    `json:"a"`
		B          dev    `json:"b"`
		Compressed dev    `json:"compressed"`
		CommitAdd  string `json:"commit_add"`
		Welcome    string `json:"welcome"`
		CommitPath string `json:"commit_path"`
		GroupId    string `json:"group_id"`
	}
	raw, err := os.ReadFile("testdata/mls-primary-impl.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	var cr CommitReader
	regOf := func(d dev, kp ParsedKeyPackage) registration {
		u, dv := uuid.FromStringOrNil(d.User), uuid.FromStringOrNil(d.Device)
		pub, _ := sec1PubOf(kp.Leaf.SigPub)
		return registration{userId: u, credential: credentialFor(u, dv), sigSec1: kp.Leaf.SigPub, sigPub: pub}
	}
	var regB registration
	for _, d := range []dev{f.A, f.B} {
		kp, err := cr.ParseKeyPackage(unhex(t, d.KeyPackage))
		if err != nil {
			t.Fatal(err)
		}
		now := time.Unix(int64(kp.Leaf.NotBefore), 0).Add(48 * time.Hour)
		reg := regOf(d, kp)
		if !checkKeyPackage(kp, reg, now, true) || !checkKeyPackage(kp, reg, now, false) {
			t.Fatal("一次の実装の KeyPackage が確かめを通らない")
		}
		if d == f.B {
			regB = reg
		}
	}
	kp, err := cr.ParseKeyPackage(unhex(t, f.Compressed.KeyPackage))
	if err != nil {
		t.Fatal(err)
	}
	if len(kp.Leaf.SigPub) != 33 {
		t.Fatalf("圧縮の署名鍵の長さ %d", len(kp.Leaf.SigPub))
	}
	u, dv := uuid.FromStringOrNil(f.Compressed.User), uuid.FromStringOrNil(f.Compressed.Device)
	if _, ok := sec1PubOf(kp.Leaf.SigPub); ok {
		t.Fatal("圧縮の点が登録できる形として読めた")
	}
	reg := registration{userId: u, credential: credentialFor(u, dv), sigSec1: kp.Leaf.SigPub}
	if checkKeyPackage(kp, reg, time.Unix(int64(kp.Leaf.NotBefore), 0).Add(48*time.Hour), true) {
		t.Fatal("圧縮の署名鍵の KeyPackage が通る")
	}
	pc, err := cr.ParseCommit(unhex(t, f.CommitAdd))
	if err != nil || len(pc.Adds) != 1 || hex.EncodeToString(pc.GroupId) != f.GroupId || pc.Epoch != 0 {
		t.Fatalf("Add の Commit: %+v %v", pc, err)
	}
	w, err := cr.ParseWelcome(unhex(t, f.Welcome))
	if err != nil || !welcomeMatches(pc.Adds, &w) {
		t.Fatalf("Welcome: %+v %v", w, err)
	}
	pp, err := cr.ParseCommit(unhex(t, f.CommitPath))
	if err != nil || pp.Path == nil {
		t.Fatalf("path の Commit: %v", err)
	}
	p := pp.Path
	if p.Source != leafSourceCommit || !bytes.Equal(p.Credential, regB.credential) || !bytes.Equal(p.SigPub, regB.sigSec1) || !capabilitiesOk(*p) ||
		!p.VerifySignature(regB.sigPub, pp.GroupId, pp.SenderLeaf) || !hpkePubOk(p.EncryptionKey) {
		t.Fatal("path の leaf が確かめ 8 を通らない")
	}
}
