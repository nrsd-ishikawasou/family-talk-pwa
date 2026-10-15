// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/binary"
	"math"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// MLSMessage の wire format（RFC 9420 6）。
const (
	wireFormatPublicMessage  = 0x0001
	wireFormatPrivateMessage = 0x0002
	wireFormatWelcome        = 0x0003
	wireFormatGroupInfo      = 0x0004
	wireFormatKeyPackage     = 0x0005
)

// ContentType は FramedContent の内容の種類（RFC 9420 6）。
const (
	contentApplication = 1
	contentProposal    = 2
	contentCommit      = 3
)

// SignWithLabel と RefHash のラベル（RFC 9420 5.1.2、5.2）。
const (
	labelPrefix        = "MLS 1.0 "
	labelKeyPackageTbs = "KeyPackageTBS"
	labelLeafNodeTbs   = "LeafNodeTBS"
	labelKeyPackageRef = "MLS 1.0 KeyPackage Reference"
)

// ParsedCommit は Commit（PublicMessage）から読んだ物（RFC 9420 6.2、12.4）。
type ParsedCommit struct {
	GroupId []byte
	Epoch   int64
	// SenderLeaf は送信者の leaf の番号（sender の種類は member だけ）。
	SenderLeaf int
	// Adds は値で含めた Add の KeyPackage（順のまま）。
	Adds []ParsedKeyPackage
	// Removes は値で含めた Remove の leaf の番号（順のまま）。
	Removes []int
	// Updates は値で含めた Update の数（第12章 5.2 の確かめ 3 で拒む）。
	Updates int
	// Path は UpdatePath の leaf_node（path が無ければ nil）。
	Path *ParsedLeafNode
}

// ParsedKeyPackage は KeyPackage から読んだ物（RFC 9420 10、7.2、5.3）。
type ParsedKeyPackage struct {
	// Bytes は KeyPackage の符号化（MLSMessage の頭 4 バイトを除く。Ref の元）。
	Bytes []byte
	// Ref は KeyPackageRef（RFC 9420 5.2）。
	Ref []byte
	// Version は KeyPackage の version（1 が mls10）。
	Version int
	// Suite は cipher_suite（本システムは 0x0002 だけを受け付ける。確かめるのは使う側）。
	Suite int
	// InitKey は init_key（Welcome の HPKE の宛先。RFC 9420 10）。
	InitKey []byte
	Leaf    ParsedLeafNode
	// tbs と sig は KeyPackageTBS の符号化と署名（DER）。
	tbs []byte
	sig []byte
}

// VerifySignature は KeyPackage の署名（SignWithLabel のラベル KeyPackageTBS。ECDSA の署名は DER。RFC 9420 5.1.2）を pub で確かめる。
// pub は呼ぶ側が渡す（登録した端末の署名鍵。Leaf.SigPub と同じであることは呼ぶ側が確かめる）。
func (p ParsedKeyPackage) VerifySignature(pub *ecdsa.PublicKey) bool {
	return verifyWithLabel(pub, labelKeyPackageTbs, p.tbs, p.sig)
}

// ParsedLeafNode は LeafNode から読んだ物（RFC 9420 7.2）。
type ParsedLeafNode struct {
	// EncryptionKey は encryption_key（HPKE の公開鍵）。
	EncryptionKey []byte
	// SigPub は signature_key（P-256 なら SEC 1 の非圧縮の 65 バイト。RFC 9420 5.1.1）。
	SigPub []byte
	// Credential は Credential の TLS 符号化そのもの（第12章 5.3 の 35 バイトと比べる）。
	Credential []byte
	// UserId と DeviceId は credential が basic で identity が 32 バイトの時だけ、前と後ろの 16 バイト（第12章 5.3）。他の形では空。
	UserId   uuid.UUID
	DeviceId uuid.UUID
	// Source は leaf_node_source（1 key_package、2 update、3 commit）。
	Source int
	// NotBefore と NotAfter は lifetime（秒。Source が key_package の時だけ。RFC 9420 7.2）。
	NotBefore uint64
	NotAfter  uint64
	// Capabilities は capabilities（未知の値もそのまま）。
	Capabilities ParsedCapabilities
	// extTypes は extensions の種類の並び（第12章 5.2 の 8 の確かめ）。
	extTypes []int
	// tbs と sig は LeafNodeTBS の文脈の前の部分（先頭から extensions の終わりまで）と署名（DER）。
	tbs []byte
	sig []byte
}

// VerifySignature は leaf_node の署名（SignWithLabel のラベル LeafNodeTBS、DER）を pub で確かめる。Source が update と commit の時は
// group_id と leaf の番号を対象に足す（RFC 9420 7.2）。key_package の時は groupId と leafIndex を使わない。
func (l ParsedLeafNode) VerifySignature(pub *ecdsa.PublicKey, groupId []byte, leafIndex int) bool {
	content := l.tbs
	if l.Source == leafSourceUpdate || l.Source == leafSourceCommit {
		if leafIndex < 0 || leafIndex > math.MaxUint32 {
			return false
		}
		content = putOpaque(append([]byte(nil), l.tbs...), groupId)
		content = binary.BigEndian.AppendUint32(content, uint32(leafIndex))
	}
	return verifyWithLabel(pub, labelLeafNodeTbs, content, l.sig)
}

// ParsedCapabilities は capabilities の 5 つの並び（RFC 9420 7.2）。
type ParsedCapabilities struct {
	Versions    []int
	Suites      []int
	Extensions  []int
	Proposals   []int
	Credentials []int
}

// ParsedWelcome は Welcome から読んだ物（RFC 9420 12.4.3.1）。
type ParsedWelcome struct {
	Suite int
	// NewMembers は secrets[].new_member（KeyPackageRef）の並び。
	NewMembers [][]byte
}

// PrivateHeader は本文（PrivateMessage）の外側（RFC 9420 6.3）。
type PrivateHeader struct {
	GroupId     []byte
	Epoch       int64
	ContentType int
}

// CommitReader は RFC 9420 の TLS 符号化を読む自前の最小の parser（ブロックのページのアルゴリズムの表）。暗号文は読まない。
// 読むのは RFC 9420 の形だけで、本システムの規則は使う側が確かめる。形の誤りは全部 KEY-009。
type CommitReader struct{}

// errMalformedEvent は形の誤りの番号（KEY-009）。
func errMalformedEvent() error {
	return fault.New(cgen.KEY009, nil)
}

// ParseCommit は MLSMessage（版 mls10、wire format mls_public_message）の PublicMessage を最後のバイトまで読む。sender が member で
// ない、content_type が commit でない、参照の提案・PSK・ReInit・ExternalInit・GroupContextExtensions を含めば KEY-009。値の Update は
// 数だけ数える。epoch が 2^63 以上なら KEY-009（記録の列は bigint）。
func (CommitReader) ParseCommit(b []byte) (ParsedCommit, error) {
	pc, err := parseCommit(b)
	if err != nil {
		return ParsedCommit{}, errMalformedEvent()
	}
	return pc, nil
}

// parseCommit は ParseCommit の本体（誤りの理由を返す。試験が使う）。
func parseCommit(b []byte) (ParsedCommit, error) {
	m, err := decodeMessage(b)
	if err != nil {
		return ParsedCommit{}, err
	}
	if m.public == nil {
		return ParsedCommit{}, &malformed{reason: "wire format が mls_public_message でない"}
	}
	c := m.public.content
	if c.sender.senderType != senderMember {
		return ParsedCommit{}, &malformed{reason: "sender が member でない"}
	}
	if c.contentType != contentCommit {
		return ParsedCommit{}, &malformed{reason: "内容の種類が commit でない"}
	}
	if c.epoch > math.MaxInt64 {
		return ParsedCommit{}, &malformed{reason: "epoch が大きすぎる"}
	}
	out := ParsedCommit{GroupId: c.groupId, Epoch: int64(c.epoch), SenderLeaf: int(c.sender.index), Adds: []ParsedKeyPackage{}, Removes: []int{}}
	for _, por := range c.commit.proposals {
		if por.typ != proposalOrRefProposal {
			return ParsedCommit{}, &malformed{reason: "参照の提案"}
		}
		p := por.proposal
		switch p.typ {
		case proposalAdd:
			out.Adds = append(out.Adds, parsedKeyPackage(p.add))
		case proposalRemove:
			out.Removes = append(out.Removes, int(p.remove))
		case proposalUpdate:
			out.Updates++
		default:
			return ParsedCommit{}, &malformed{reason: "読めない提案の種類"}
		}
	}
	if c.commit.path != nil {
		l := parsedLeafNode(c.commit.path.leaf)
		out.Path = &l
	}
	return out, nil
}

// ParseWelcome は MLSMessage（wire format mls_welcome）の cipher_suite と secrets[].new_member を読む。
func (CommitReader) ParseWelcome(b []byte) (ParsedWelcome, error) {
	m, err := decodeMessage(b)
	if err != nil || m.welcome == nil {
		return ParsedWelcome{}, errMalformedEvent()
	}
	out := ParsedWelcome{Suite: int(m.welcome.suite), NewMembers: [][]byte{}}
	for _, s := range m.welcome.secrets {
		out.NewMembers = append(out.NewMembers, s.newMember)
	}
	return out, nil
}

// ParseKeyPackage は MLSMessage（wire format mls_key_package）の KeyPackage を最後のバイトまで読み、Ref を求める。
func (CommitReader) ParseKeyPackage(b []byte) (ParsedKeyPackage, error) {
	m, err := decodeMessage(b)
	if err != nil || m.keyPackage == nil {
		return ParsedKeyPackage{}, errMalformedEvent()
	}
	return parsedKeyPackage(m.keyPackage), nil
}

// ParsePrivateHeader は MLSMessage の先頭 4 バイトと PrivateMessage の 6 つの欄を最後のバイトまで読み、group_id、epoch、
// content_type を返す。authenticated_data、encrypted_sender_data、ciphertext は長さだけ読む。形の誤りは KEY-009（CheckSender が
// ROOM-005 に写す）。
func (CommitReader) ParsePrivateHeader(b []byte) (PrivateHeader, error) {
	m, err := decodeMessage(b)
	if err != nil || m.private == nil || m.private.epoch > math.MaxInt64 {
		return PrivateHeader{}, errMalformedEvent()
	}
	return PrivateHeader{GroupId: m.private.groupId, Epoch: int64(m.private.epoch), ContentType: int(m.private.contentType)}, nil
}

// KeyPackageRef は RefHash("MLS 1.0 KeyPackage Reference", KeyPackage)（RFC 9420 5.2。スイート 0x0002 なので SHA-256）。
// keyPackage は MLSMessage の頭 4 バイトを除いた KeyPackage の符号化。
func (CommitReader) KeyPackageRef(keyPackage []byte) []byte {
	return keyPackageRef(keyPackage)
}

func keyPackageRef(kp []byte) []byte {
	return refHash(labelKeyPackageRef, kp)
}

// refHash は RefHash（RFC 9420 5.2。ラベルに前置きは付けない。スイート 0x0002 なので SHA-256）。
func refHash(label string, value []byte) []byte {
	in := putOpaque(nil, []byte(label))
	in = putOpaque(in, value)
	h := sha256.Sum256(in)
	return h[:]
}

// verifyWithLabel は VerifyWithLabel（RFC 9420 5.1.2。SignContent は vec("MLS 1.0 " ‖ label) ‖ vec(content)。ECDSA P-256 の DER）。
func verifyWithLabel(pub *ecdsa.PublicKey, label string, content, sig []byte) bool {
	if pub == nil || content == nil {
		return false
	}
	in := putOpaque(nil, []byte(labelPrefix+label))
	in = putOpaque(in, content)
	h := sha256.Sum256(in)
	return ecdsa.VerifyASN1(pub, h[:], sig)
}

func parsedKeyPackage(k *keyPackage) ParsedKeyPackage {
	return ParsedKeyPackage{Bytes: k.raw, Ref: keyPackageRef(k.raw), Version: int(k.version), Suite: int(k.suite), InitKey: k.initKey,
		Leaf: parsedLeafNode(k.leaf), tbs: k.tbs, sig: k.signature}
}

func parsedLeafNode(l leafNode) ParsedLeafNode {
	out := ParsedLeafNode{EncryptionKey: l.encryptionKey, SigPub: l.signatureKey, Credential: l.credBytes, Source: int(l.source),
		NotBefore: l.notBefore, NotAfter: l.notAfter, tbs: l.tbs, sig: l.signature,
		Capabilities: ParsedCapabilities{Versions: ints(l.capabilities.versions), Suites: ints(l.capabilities.suites),
			Extensions: ints(l.capabilities.extensions), Proposals: ints(l.capabilities.proposals), Credentials: ints(l.capabilities.credentials)}}
	if l.credential.typ == credentialBasic && len(l.credential.identity) == 32 {
		out.UserId = uuid.FromBytesOrNil(l.credential.identity[:16])
		out.DeviceId = uuid.FromBytesOrNil(l.credential.identity[16:])
	}
	for _, e := range l.extensions {
		out.extTypes = append(out.extTypes, int(e.typ))
	}
	return out
}

func ints(v []uint16) []int {
	out := make([]int, len(v))
	for i, x := range v {
		out[i] = int(x)
	}
	return out
}
