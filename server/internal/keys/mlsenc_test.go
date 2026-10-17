// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
)

// 試験だけの書き戻し（mlsdecode の構造を RFC 9420 の TLS 符号化に戻す）と、本システムの規則で通る MLS の物を組む関数。
// 公式の相互運用の検証ベクトルの往復（読んで書き戻すと元のバイト列）と、確かめを 1 つずつ壊した入力を作るのに使う。

type enc struct{ b []byte }

func (e *enc) u8(v uint8)   { e.b = append(e.b, v) }
func (e *enc) u16(v uint16) { e.b = binary.BigEndian.AppendUint16(e.b, v) }
func (e *enc) u32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }
func (e *enc) u64(v uint64) { e.b = binary.BigEndian.AppendUint64(e.b, v) }
func (e *enc) opaque(v []byte) {
	e.b = putOpaque(e.b, v)
}

// vector は fn が書いた中身を可変長のベクタにする。
func (e *enc) vector(fn func(*enc)) {
	in := &enc{}
	fn(in)
	e.opaque(in.b)
}

func (e *enc) optional(present bool, fn func(*enc)) {
	if !present {
		e.u8(0)
		return
	}
	e.u8(1)
	fn(e)
}

func encMessage(m *mlsMessage) []byte {
	e := &enc{}
	e.u16(m.version)
	e.u16(m.wireFormat)
	switch {
	case m.public != nil:
		encPublicMessage(e, m.public)
	case m.private != nil:
		p := m.private
		e.opaque(p.groupId)
		e.u64(p.epoch)
		e.u8(p.contentType)
		e.opaque(p.authenticatedData)
		e.opaque(p.encryptedSenderData)
		e.opaque(p.ciphertext)
	case m.welcome != nil:
		encWelcome(e, m.welcome)
	case m.groupInfo != nil:
		encGroupInfo(e, m.groupInfo)
	case m.keyPackage != nil:
		encKeyPackage(e, m.keyPackage)
	}
	return e.b
}

func encPublicMessage(e *enc, p *publicMessage) {
	c := p.content
	e.opaque(c.groupId)
	e.u64(c.epoch)
	e.u8(c.sender.senderType)
	if c.sender.senderType == senderMember || c.sender.senderType == senderExternal {
		e.u32(c.sender.index)
	}
	e.opaque(c.authenticatedData)
	e.u8(c.contentType)
	switch c.contentType {
	case contentApplication:
		e.opaque(c.applicationData)
	case contentProposal:
		encProposal(e, c.proposal)
	case contentCommit:
		encCommit(e, c.commit)
	}
	e.opaque(p.auth.signature)
	if c.contentType == contentCommit {
		e.opaque(p.auth.confirmationTag)
	}
	if c.sender.senderType == senderMember {
		e.opaque(p.membershipTag)
	}
}

func encCommit(e *enc, c *commitMsg) {
	e.vector(func(v *enc) {
		for _, p := range c.proposals {
			v.u8(p.typ)
			if p.typ == proposalOrRefProposal {
				encProposal(v, p.proposal)
			} else {
				v.opaque(p.reference)
			}
		}
	})
	e.optional(c.path != nil, func(v *enc) { encUpdatePath(v, c.path) })
}

func encProposal(e *enc, p *proposal) {
	e.u16(p.typ)
	switch p.typ {
	case proposalAdd:
		encKeyPackage(e, p.add)
	case proposalUpdate:
		encLeafNode(e, p.update)
	case proposalRemove:
		e.u32(p.remove)
	case proposalPreSharedKey:
		encPsk(e, p.psk)
	case proposalReInit:
		e.opaque(p.reinit.groupId)
		e.u16(p.reinit.version)
		e.u16(p.reinit.suite)
		encExtensions(e, p.reinit.extensions)
	case proposalExternalInit:
		e.opaque(p.externalInit)
	case proposalGroupContextExtensions:
		encExtensions(e, p.extensions)
	}
}

func encPsk(e *enc, k *preSharedKeyID) {
	e.u8(k.pskType)
	if k.pskType == pskExternal {
		e.opaque(k.pskId)
	} else {
		e.u8(k.usage)
		e.opaque(k.pskGroupId)
		e.u64(k.pskEpoch)
	}
	e.opaque(k.pskNonce)
}

func encUpdatePath(e *enc, p *updatePath) {
	encLeafNode(e, &p.leaf)
	e.vector(func(v *enc) {
		for _, n := range p.nodes {
			v.opaque(n.encryptionKey)
			v.vector(func(w *enc) {
				for _, s := range n.secrets {
					w.opaque(s.kemOutput)
					w.opaque(s.ciphertext)
				}
			})
		}
	})
}

// encLeafNodeTbs は LeafNode の先頭から extensions の終わりまで（署名の対象の前の部分）。
func encLeafNodeTbs(e *enc, l *leafNode) {
	e.opaque(l.encryptionKey)
	e.opaque(l.signatureKey)
	encCredential(e, l.credential)
	encCapabilities(e, l.capabilities)
	e.u8(l.source)
	switch l.source {
	case leafSourceKeyPackage:
		e.u64(l.notBefore)
		e.u64(l.notAfter)
	case leafSourceCommit:
		e.opaque(l.parentHash)
	}
	encExtensions(e, l.extensions)
}

func encLeafNode(e *enc, l *leafNode) {
	encLeafNodeTbs(e, l)
	e.opaque(l.signature)
}

func encCredential(e *enc, c credential) {
	e.u16(c.typ)
	if c.typ == credentialBasic {
		e.opaque(c.identity)
		return
	}
	e.vector(func(v *enc) {
		for _, x := range c.certificates {
			v.opaque(x)
		}
	})
}

func encCapabilities(e *enc, c capabilities) {
	for _, list := range [][]uint16{c.versions, c.suites, c.extensions, c.proposals, c.credentials} {
		e.vector(func(v *enc) {
			for _, x := range list {
				v.u16(x)
			}
		})
	}
}

func encExtensions(e *enc, xs []extension) {
	e.vector(func(v *enc) {
		for _, x := range xs {
			v.u16(x.typ)
			v.opaque(x.data)
		}
	})
}

func encKeyPackageTbs(e *enc, k *keyPackage) {
	e.u16(k.version)
	e.u16(k.suite)
	e.opaque(k.initKey)
	encLeafNode(e, &k.leaf)
	encExtensions(e, k.extensions)
}

func encKeyPackage(e *enc, k *keyPackage) {
	encKeyPackageTbs(e, k)
	e.opaque(k.signature)
}

func encWelcome(e *enc, w *welcomeMsg) {
	e.u16(w.suite)
	e.vector(func(v *enc) {
		for _, s := range w.secrets {
			v.opaque(s.newMember)
			v.opaque(s.secrets.kemOutput)
			v.opaque(s.secrets.ciphertext)
		}
	})
	e.opaque(w.encryptedGroupInfo)
}

func encGroupInfo(e *enc, g *groupInfo) {
	c := g.context
	e.u16(c.version)
	e.u16(c.suite)
	e.opaque(c.groupId)
	e.u64(c.epoch)
	e.opaque(c.treeHash)
	e.opaque(c.confirmedTranscriptHash)
	encExtensions(e, c.extensions)
	encExtensions(e, g.extensions)
	e.opaque(g.confirmationTag)
	e.u32(g.signer)
	e.opaque(g.signature)
}

// 本システムの規則で通る物を組む。

// devKeys は試験の 1 台の端末（構成員と端末の識別子、署名鍵、鍵合意鍵）。
type devKeys struct {
	userId   uuid.UUID
	deviceId uuid.UUID
	sig      *ecdsa.PrivateKey
	kem      *ecdsa.PrivateKey
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newDevKeys(t testing.TB, userId, deviceId uuid.UUID) *devKeys {
	return &devKeys{userId: userId, deviceId: deviceId, sig: newKey(t), kem: newKey(t)}
}

// sec1 は公開鍵の SEC 1 の非圧縮の 65 バイト。
func sec1(t testing.TB, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	b, err := k.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// hpkeKey は HPKE の公開鍵として使う P-256 の点（65 バイト）。
func hpkeKey(t testing.TB) []byte {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKey().Bytes()
}

// badHpkeKeys は P-256 の公開鍵の形でない 4 通り（第12章 5.2「KeyPackage の保管」の 7。RFC 9180 7.1.1、7.1.4）：曲線の外（生成元の
// y の最下位のビットを替えた点）、無限遠点（0x04 と 64 バイトの 0）、64 バイト（非圧縮の頭の無い x と y）、圧縮の 33 バイト。
func badHpkeKeys() map[string][]byte {
	p := elliptic.P256().Params()
	g := make([]byte, 65)
	g[0] = 4
	p.Gx.FillBytes(g[1:33])
	p.Gy.FillBytes(g[33:])
	off := append([]byte(nil), g...)
	off[64] ^= 1
	inf := make([]byte, 65)
	inf[0] = 4
	comp := append([]byte{2 | g[64]&1}, g[1:33]...)
	return map[string][]byte{"曲線の外": off, "無限遠点": inf, "64 バイト": append([]byte(nil), g[1:]...), "圧縮の 33 バイト": comp}
}

// signLabel は SignWithLabel（RFC 9420 5.1.2。DER）。
func signLabel(t testing.TB, k *ecdsa.PrivateKey, label string, content []byte) []byte {
	t.Helper()
	in := putOpaque(nil, []byte(labelPrefix+label))
	in = putOpaque(in, content)
	h := sha256.Sum256(in)
	sig, err := ecdsa.SignASN1(rand.Reader, k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// signRS は本システムの署名（ECDSA P-256、SHA-256、r と s の 64 バイト）。
func signRS(t testing.TB, k *ecdsa.PrivateKey, message []byte) []byte {
	t.Helper()
	h := sha256.Sum256(message)
	r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

// basicCredential は第12章 5.3 の credential の構造。
func basicCredential(userId, deviceId uuid.UUID) credential {
	return credential{typ: credentialBasic, identity: append(append([]byte(nil), userId.Bytes()...), deviceId.Bytes()...)}
}

// defaultCaps は第12章 5.2 の 8 を満たす capabilities。
func defaultCaps() capabilities {
	return capabilities{versions: []uint16{mlsVersion10}, suites: []uint16{mlsSuite}, extensions: []uint16{},
		proposals: []uint16{}, credentials: []uint16{credentialBasic}}
}

// signLeaf は leaf の署名を置く（source が update と commit なら group_id と leaf の番号を対象に足す）。
func signLeaf(t testing.TB, l *leafNode, k *ecdsa.PrivateKey, groupId []byte, index uint32) {
	t.Helper()
	e := &enc{}
	encLeafNodeTbs(e, l)
	content := e.b
	if l.source == leafSourceUpdate || l.source == leafSourceCommit {
		content = putOpaque(content, groupId)
		content = binary.BigEndian.AppendUint32(content, index)
	}
	l.signature = signLabel(t, k, labelLeafNodeTbs, content)
}

// kpOpts は KeyPackage を組む時の変え方。before は署名の前、after は署名の後（署名を壊す）に呼ぶ。
type kpOpts struct {
	before func(k *keyPackage)
	after  func(k *keyPackage)
	// signer は署名に使う鍵（空なら端末の署名鍵）。leafSigner は leaf の署名だけに使う鍵（空なら signer）。
	signer     *ecdsa.PrivateKey
	leafSigner *ecdsa.PrivateKey
}

// buildKeyPackage は端末の KeyPackage（MLSMessage の符号化）。有効期間は now の 1 日前から 100 日後（第12章 5.2）。
func buildKeyPackage(t testing.TB, dk *devKeys, now time.Time, o kpOpts) []byte {
	t.Helper()
	k := &keyPackage{version: mlsVersion10, suite: mlsSuite, initKey: hpkeKey(t), extensions: []extension{}}
	k.leaf = leafNode{encryptionKey: hpkeKey(t), signatureKey: sec1(t, dk.sig), credential: basicCredential(dk.userId, dk.deviceId),
		capabilities: defaultCaps(), source: leafSourceKeyPackage, notBefore: uint64(now.Add(-24 * time.Hour).Unix()),
		notAfter: uint64(now.Add(100 * 24 * time.Hour).Unix()), extensions: []extension{}}
	if o.before != nil {
		o.before(k)
	}
	signer := dk.sig
	if o.signer != nil {
		signer = o.signer
	}
	leafSigner := signer
	if o.leafSigner != nil {
		leafSigner = o.leafSigner
	}
	signLeaf(t, &k.leaf, leafSigner, nil, 0)
	tbs := &enc{}
	encKeyPackageTbs(tbs, k)
	k.signature = signLabel(t, signer, labelKeyPackageTbs, tbs.b)
	if o.after != nil {
		o.after(k)
	}
	return encMessage(&mlsMessage{version: mlsVersion10, wireFormat: wireFormatKeyPackage, keyPackage: k})
}

// keyPackageOf は MLSMessage の KeyPackage の構造（Commit の Add に入れる）。
func keyPackageOf(t testing.TB, msg []byte) *keyPackage {
	t.Helper()
	m, err := decodeMessage(msg)
	if err != nil || m.keyPackage == nil {
		t.Fatalf("KeyPackage を読めない: %v", err)
	}
	return m.keyPackage
}

// refOf は MLSMessage の KeyPackage の KeyPackageRef。
func refOf(t testing.TB, msg []byte) []byte {
	t.Helper()
	return keyPackageRef(keyPackageOf(t, msg).raw)
}

// commitSpec は Commit（PublicMessage）の中身。
type commitSpec struct {
	groupId []byte
	epoch   uint64
	sender  uint32
	adds    [][]byte // KeyPackage の MLSMessage
	removes []uint32
	updates int
	// extra は値で含める他の提案（読めない提案の拒みの試験）。
	extra []proposalOrRef
	path  *updatePath
}

// pathLeaf は Commit を作る端末の UpdatePath の leaf（source commit、group_id と送信者の番号を文脈に署名）。
func pathLeaf(t testing.TB, dk *devKeys, groupId []byte, index uint32, mod func(l *leafNode)) *updatePath {
	t.Helper()
	l := leafNode{encryptionKey: hpkeKey(t), signatureKey: sec1(t, dk.sig), credential: basicCredential(dk.userId, dk.deviceId),
		capabilities: defaultCaps(), source: leafSourceCommit, parentHash: []byte{}, extensions: []extension{}}
	if mod != nil {
		mod(&l)
	}
	signLeaf(t, &l, dk.sig, groupId, index)
	return &updatePath{leaf: l, nodes: []updatePathNode{{encryptionKey: hpkeKey(t), secrets: []hpkeCiphertext{{kemOutput: []byte{1}, ciphertext: []byte{2}}}}}}
}

// buildCommit は Commit の PublicMessage（MLSMessage）。FramedContent の署名と MAC はサーバーが確かめないので任意の値。
func buildCommit(t testing.TB, c commitSpec) []byte {
	t.Helper()
	cm := &commitMsg{path: c.path}
	for _, r := range c.removes {
		cm.proposals = append(cm.proposals, proposalOrRef{typ: proposalOrRefProposal, proposal: &proposal{typ: proposalRemove, remove: r}})
	}
	for _, a := range c.adds {
		cm.proposals = append(cm.proposals, proposalOrRef{typ: proposalOrRefProposal, proposal: &proposal{typ: proposalAdd, add: keyPackageOf(t, a)}})
	}
	for range c.updates {
		l := leafNode{encryptionKey: hpkeKey(t), signatureKey: hpkeKey(t), credential: credential{typ: credentialBasic, identity: []byte{1}},
			capabilities: defaultCaps(), source: leafSourceUpdate, extensions: []extension{}, signature: []byte{3}}
		cm.proposals = append(cm.proposals, proposalOrRef{typ: proposalOrRefProposal, proposal: &proposal{typ: proposalUpdate, update: &l}})
	}
	cm.proposals = append(cm.proposals, c.extra...)
	p := &publicMessage{content: framedContent{groupId: c.groupId, epoch: c.epoch, sender: sender{senderType: senderMember, index: c.sender},
		authenticatedData: []byte{}, contentType: contentCommit, commit: cm},
		auth: framedContentAuthData{signature: []byte{0x30, 0x00}, confirmationTag: make([]byte, 32)}, membershipTag: make([]byte, 32)}
	return encMessage(&mlsMessage{version: mlsVersion10, wireFormat: wireFormatPublicMessage, public: p})
}

// buildWelcome は Welcome（MLSMessage）。secrets の new_member に refs を並べ、kem_output は P-256 の点（確かめ 9 の形。秘密の値を
// 並びの番号 + 1 とした鍵の公開鍵）。暗号文は任意の値。
func buildWelcome(suite uint16, refs ...[]byte) []byte {
	kems := make([][]byte, len(refs))
	for i := range refs {
		scalar := make([]byte, 32)
		scalar[31] = byte(i + 1)
		k, err := ecdh.P256().NewPrivateKey(scalar)
		if err != nil {
			panic(err)
		}
		kems[i] = k.PublicKey().Bytes()
	}
	return buildWelcomeKems(suite, kems, refs...)
}

// buildWelcomeKems は kem_output を secrets の並びごとに与える Welcome（形の誤りの試験）。
func buildWelcomeKems(suite uint16, kems [][]byte, refs ...[]byte) []byte {
	w := &welcomeMsg{suite: suite, encryptedGroupInfo: []byte{9, 9}}
	for i, r := range refs {
		w.secrets = append(w.secrets, encryptedGroupSecrets{newMember: r, secrets: hpkeCiphertext{kemOutput: kems[i], ciphertext: []byte{2}}})
	}
	return encMessage(&mlsMessage{version: mlsVersion10, wireFormat: wireFormatWelcome, welcome: w})
}

// buildPrivate は本文（PrivateMessage の MLSMessage）。中身は任意の値。
func buildPrivate(groupId []byte, epoch uint64, contentType uint8) []byte {
	return encMessage(&mlsMessage{version: mlsVersion10, wireFormat: wireFormatPrivateMessage, private: &privateMessage{groupId: groupId,
		epoch: epoch, contentType: contentType, authenticatedData: []byte{}, encryptedSenderData: []byte{1, 2}, ciphertext: []byte{3, 4}}})
}

// testNow は試験の今の時刻（壁の時計。time.Now を直接呼ばない：ftlint）。
func testNow() time.Time {
	return clock.WallClock{}.Now()
}

// testUuid は試験の識別子（UUIDv7 の形で n を末尾に置いた物）。
func testUuid(n byte) uuid.UUID {
	return uuid.Must(uuid.FromString("0199e1c2-0000-7000-8000-0000000000" + hexByte(n)))
}

func hexByte(n byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[n>>4], digits[n&15]})
}
