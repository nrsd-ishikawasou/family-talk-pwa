// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"crypto/ecdsa"

	"github.com/gofrs/uuid/v5"
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
}

// ParsedKeyPackage は KeyPackage（MLSMessage の wire format mls_key_package）から読んだ物（RFC 9420 10、7.2、5.3）。
type ParsedKeyPackage struct {
	// Bytes は KeyPackage の符号化（MLSMessage の頭 4 バイトを除く）。
	Bytes []byte
	// Ref は KeyPackageRef（RFC 9420 5.2）。
	Ref []byte
	// UserId と DeviceId は basic の credential の identity（32 バイト）の前と後ろ（第12章 5.3）。
	UserId   uuid.UUID
	DeviceId uuid.UUID
	// SigPub は leaf の signature_key（SEC 1 の非圧縮の 65 バイト。RFC 9420 5.1.1）。
	SigPub []byte
	Suite  int
	// tbs と sig は KeyPackageTBS の符号化と署名（DER）。VerifySignature が使う。
	tbs []byte
	sig []byte
}

// VerifySignature は KeyPackage の署名（SignWithLabel のラベル KeyPackageTBS。ECDSA の署名は DER。RFC 9420 5.1.2）を pub で確かめる。
// pub は呼ぶ側が渡す（登録した端末の署名鍵。SigPub と同じであることは呼ぶ側が確かめる）。
func (p ParsedKeyPackage) VerifySignature(pub *ecdsa.PublicKey) bool {
	panic("未実装")
}

// ParsedWelcome は Welcome（MLSMessage の wire format mls_welcome）から読んだ物（RFC 9420 12.4.3.1）。
type ParsedWelcome struct {
	Suite int
	// NewMembers は secrets[].new_member（KeyPackageRef）の並び。
	NewMembers [][]byte
}

// PrivateHeader は本文（PrivateMessage）の外側（RFC 9420 6.3）。wire format は mls_private_message に限る。
type PrivateHeader struct {
	GroupId     []byte
	Epoch       int64
	ContentType int
}

// CommitReader は RFC 9420 の TLS 符号化を読む自前の最小の parser（ブロックのページのアルゴリズムの表）。暗号文と UpdatePath の中身は
// 読まない。可変長のベクタは RFC 9420 2.1.2 の最小の符号化でなければ拒む。読めない・規則に合わない物は KEY-009。
type CommitReader struct{}

// ParseCommit は MLSMessage（版 mls10、wire format mls_public_message）の PublicMessage の FramedContent と Commit の proposals を読む。
// sender が member でない、content_type が commit でない、参照の提案・PSK・ReInit・ExternalInit・GroupContextExtensions・未知の
// 種類を含めば KEY-009。Update は読み飛ばす。path、auth、membership_tag は長さだけ進める。
func (CommitReader) ParseCommit(b []byte) (ParsedCommit, error) {
	panic("未実装")
}

// ParseWelcome は MLSMessage（wire format mls_welcome）の cipher_suite と secrets[].new_member を読む。encrypted_group_info は読み飛ばす。
func (CommitReader) ParseWelcome(b []byte) (ParsedWelcome, error) {
	panic("未実装")
}

// ParseKeyPackage は MLSMessage（wire format mls_key_package）の KeyPackage を読む。版、スイート、leaf_node の signature_key、
// credential（basic の identity 32 バイト）、leaf_node_source（key_package でなければ KEY-009）を取り、Ref を求める。
func (CommitReader) ParseKeyPackage(b []byte) (ParsedKeyPackage, error) {
	panic("未実装")
}

// ParsePrivateHeader は MLSMessage の先頭 4 バイトと PrivateMessage の group_id、epoch、content_type を読む。残りは読まない。
func (CommitReader) ParsePrivateHeader(b []byte) (PrivateHeader, error) {
	panic("未実装")
}

// KeyPackageRef は RefHash("MLS 1.0 KeyPackage Reference", KeyPackage)（RFC 9420 5.2。スイート 0x0002 なので SHA-256）。
// keyPackage は MLSMessage の頭 4 バイトを除いた KeyPackage の符号化。
func (CommitReader) KeyPackageRef(keyPackage []byte) []byte {
	panic("未実装")
}
