// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

// RFC 9420 の構造の読み。どの構造も欄を全部持つ（形だけ読む欄も opaque のまま持ち、試験の書き戻しに使う）。
// 読むのは RFC 9420 の形だけで、本システムの規則（スイート、credential の形、登録した鍵との一致）は使う側が確かめる
// （ブロックのページの CommitReader の読み方の約束）。

// MLSMessage の版（RFC 9420 6。mls10）。
const mlsVersion10 = 1

// Sender の種類（RFC 9420 6）。
const (
	senderMember            = 1
	senderExternal          = 2
	senderNewMemberProposal = 3
	senderNewMemberCommit   = 4
)

// Proposal の種類（RFC 9420 12.1。17.4 の表）。
const (
	proposalAdd                    = 1
	proposalUpdate                 = 2
	proposalRemove                 = 3
	proposalPreSharedKey           = 4
	proposalReInit                 = 5
	proposalExternalInit           = 6
	proposalGroupContextExtensions = 7
)

// ProposalOrRef の種類（RFC 9420 12.4）。
const (
	proposalOrRefProposal  = 1
	proposalOrRefReference = 2
)

// leaf_node_source（RFC 9420 7.2）。
const (
	leafSourceKeyPackage = 1
	leafSourceUpdate     = 2
	leafSourceCommit     = 3
)

// Credential の種類（RFC 9420 5.3）。
const (
	credentialBasic = 1
	credentialX509  = 2
)

// PreSharedKeyID の psktype（RFC 9420 8.4）。
const (
	pskExternal   = 1
	pskResumption = 2
)

type mlsMessage struct {
	version    uint16
	wireFormat uint16
	public     *publicMessage
	private    *privateMessage
	welcome    *welcomeMsg
	groupInfo  *groupInfo
	keyPackage *keyPackage
}

type publicMessage struct {
	content framedContent
	auth    framedContentAuthData
	// membershipTag は sender が member の時だけ（RFC 9420 6.2）。
	membershipTag []byte
}

type framedContent struct {
	groupId           []byte
	epoch             uint64
	sender            sender
	authenticatedData []byte
	contentType       uint8
	applicationData   []byte
	proposal          *proposal
	commit            *commitMsg
}

type sender struct {
	senderType uint8
	// index は member の leaf の番号、external の送り手の番号（他の種類は 0）。
	index uint32
}

type framedContentAuthData struct {
	signature []byte
	// confirmationTag は content_type が commit の時だけ（RFC 9420 6.1）。
	confirmationTag []byte
}

type commitMsg struct {
	proposals []proposalOrRef
	path      *updatePath
}

type proposalOrRef struct {
	typ       uint8
	proposal  *proposal
	reference []byte
}

type proposal struct {
	typ          uint16
	add          *keyPackage
	update       *leafNode
	remove       uint32
	psk          *preSharedKeyID
	reinit       *reInit
	externalInit []byte
	extensions   []extension
}

type preSharedKeyID struct {
	pskType    uint8
	pskId      []byte
	usage      uint8
	pskGroupId []byte
	pskEpoch   uint64
	pskNonce   []byte
}

type reInit struct {
	groupId    []byte
	version    uint16
	suite      uint16
	extensions []extension
}

type updatePath struct {
	leaf  leafNode
	nodes []updatePathNode
}

type updatePathNode struct {
	encryptionKey []byte
	secrets       []hpkeCiphertext
}

type hpkeCiphertext struct {
	kemOutput  []byte
	ciphertext []byte
}

type leafNode struct {
	encryptionKey []byte
	signatureKey  []byte
	credential    credential
	// credBytes は Credential の TLS 符号化そのもの（第12章 5.3 の 35 バイトと比べる）。
	credBytes    []byte
	capabilities capabilities
	source       uint8
	notBefore    uint64
	notAfter     uint64
	parentHash   []byte
	extensions   []extension
	signature    []byte
	// tbs は先頭から extensions の終わりまで（LeafNodeTBS の文脈の前の部分。RFC 9420 7.2）。
	tbs []byte
}

type credential struct {
	typ          uint16
	identity     []byte
	certificates [][]byte
}

type capabilities struct {
	versions    []uint16
	suites      []uint16
	extensions  []uint16
	proposals   []uint16
	credentials []uint16
}

type extension struct {
	typ  uint16
	data []byte
}

type keyPackage struct {
	version    uint16
	suite      uint16
	initKey    []byte
	leaf       leafNode
	extensions []extension
	signature  []byte
	// raw は KeyPackage の符号化（Ref の元）。tbs は先頭から extensions の終わりまで（KeyPackageTBS。RFC 9420 10）。
	raw []byte
	tbs []byte
}

type welcomeMsg struct {
	suite              uint16
	secrets            []encryptedGroupSecrets
	encryptedGroupInfo []byte
}

type encryptedGroupSecrets struct {
	newMember []byte
	secrets   hpkeCiphertext
}

type groupInfo struct {
	context         groupContext
	extensions      []extension
	confirmationTag []byte
	signer          uint32
	signature       []byte
}

type groupContext struct {
	version                 uint16
	suite                   uint16
	groupId                 []byte
	epoch                   uint64
	treeHash                []byte
	confirmedTranscriptHash []byte
	extensions              []extension
}

type privateMessage struct {
	groupId             []byte
	epoch               uint64
	contentType         uint8
	authenticatedData   []byte
	encryptedSenderData []byte
	ciphertext          []byte
}

// decodeMessage は MLSMessage を最後のバイトまで読む（RFC 9420 6）。
func decodeMessage(b []byte) (*mlsMessage, error) {
	r := &tlsReader{b: b}
	m := readMessage(r)
	r.end()
	if r.err != nil {
		return nil, r.err
	}
	return m, nil
}

func readMessage(r *tlsReader) *mlsMessage {
	m := &mlsMessage{version: r.u16(), wireFormat: r.u16()}
	if r.err != nil {
		return nil
	}
	if m.version != mlsVersion10 {
		r.fail("版が mls10 でない")
		return nil
	}
	switch m.wireFormat {
	case wireFormatPublicMessage:
		m.public = readPublicMessage(r)
	case wireFormatPrivateMessage:
		m.private = readPrivateMessage(r)
	case wireFormatWelcome:
		m.welcome = readWelcome(r)
	case wireFormatGroupInfo:
		m.groupInfo = readGroupInfo(r)
	case wireFormatKeyPackage:
		m.keyPackage = readKeyPackage(r)
		if r.err == nil && m.keyPackage.version != m.version {
			r.fail("KeyPackage の版が MLSMessage の版と違う")
		}
	default:
		r.fail("wire format が表に無い")
	}
	return m
}

func readPublicMessage(r *tlsReader) *publicMessage {
	p := &publicMessage{}
	p.content = readFramedContent(r)
	p.auth = readAuthData(r, p.content.contentType)
	if p.content.sender.senderType == senderMember {
		p.membershipTag = r.opaque()
	}
	return p
}

func readFramedContent(r *tlsReader) framedContent {
	c := framedContent{groupId: r.opaque(), epoch: r.u64(), sender: readSender(r), authenticatedData: r.opaque(), contentType: r.u8()}
	if r.err != nil {
		return c
	}
	switch c.contentType {
	case contentApplication:
		c.applicationData = r.opaque()
	case contentProposal:
		c.proposal = readProposal(r)
	case contentCommit:
		c.commit = readCommit(r)
	default:
		r.fail("内容の種類が表に無い")
	}
	return c
}

func readSender(r *tlsReader) sender {
	s := sender{senderType: r.u8()}
	switch s.senderType {
	case senderMember, senderExternal:
		s.index = r.u32()
	case senderNewMemberProposal, senderNewMemberCommit:
	default:
		r.fail("sender の種類が表に無い")
	}
	return s
}

func readAuthData(r *tlsReader, contentType uint8) framedContentAuthData {
	a := framedContentAuthData{signature: r.opaque()}
	if contentType == contentCommit {
		a.confirmationTag = r.opaque()
	}
	return a
}

func readCommit(r *tlsReader) *commitMsg {
	c := &commitMsg{}
	r.vector(func(s *tlsReader) {
		c.proposals = append(c.proposals, readProposalOrRef(s))
	})
	r.optional(func(s *tlsReader) {
		p := readUpdatePath(s)
		c.path = &p
	})
	return c
}

func readProposalOrRef(r *tlsReader) proposalOrRef {
	p := proposalOrRef{typ: r.u8()}
	switch p.typ {
	case proposalOrRefProposal:
		p.proposal = readProposal(r)
	case proposalOrRefReference:
		p.reference = r.opaque()
	default:
		r.fail("ProposalOrRef の種類が表に無い")
	}
	return p
}

func readProposal(r *tlsReader) *proposal {
	p := &proposal{typ: r.u16()}
	if r.err != nil {
		return p
	}
	switch p.typ {
	case proposalAdd:
		p.add = readKeyPackage(r)
	case proposalUpdate:
		l := readLeafNode(r)
		p.update = &l
	case proposalRemove:
		p.remove = r.u32()
	case proposalPreSharedKey:
		k := readPreSharedKeyID(r)
		p.psk = &k
	case proposalReInit:
		p.reinit = &reInit{groupId: r.opaque(), version: r.u16(), suite: r.u16(), extensions: readExtensions(r)}
	case proposalExternalInit:
		p.externalInit = r.opaque()
	case proposalGroupContextExtensions:
		p.extensions = readExtensions(r)
	default:
		// GREASE を置いてはならない欄（RFC 9420 13.5）。
		r.fail("提案の種類が表に無い")
	}
	return p
}

func readPreSharedKeyID(r *tlsReader) preSharedKeyID {
	k := preSharedKeyID{pskType: r.u8()}
	switch k.pskType {
	case pskExternal:
		k.pskId = r.opaque()
	case pskResumption:
		k.usage = r.u8()
		k.pskGroupId = r.opaque()
		k.pskEpoch = r.u64()
	default:
		r.fail("psktype が表に無い")
	}
	k.pskNonce = r.opaque()
	return k
}

func readUpdatePath(r *tlsReader) updatePath {
	p := updatePath{leaf: readLeafNode(r)}
	r.vector(func(s *tlsReader) {
		n := updatePathNode{encryptionKey: s.opaque()}
		s.vector(func(t *tlsReader) {
			n.secrets = append(n.secrets, readHpkeCiphertext(t))
		})
		p.nodes = append(p.nodes, n)
	})
	return p
}

func readHpkeCiphertext(r *tlsReader) hpkeCiphertext {
	return hpkeCiphertext{kemOutput: r.opaque(), ciphertext: r.opaque()}
}

func readLeafNode(r *tlsReader) leafNode {
	start := r.i
	l := leafNode{encryptionKey: r.opaque(), signatureKey: r.opaque()}
	credStart := r.i
	l.credential = readCredential(r)
	if r.err == nil {
		l.credBytes = r.b[credStart:r.i:r.i]
	}
	l.capabilities = readCapabilities(r)
	l.source = r.u8()
	switch l.source {
	case leafSourceKeyPackage:
		l.notBefore = r.u64()
		l.notAfter = r.u64()
	case leafSourceUpdate:
	case leafSourceCommit:
		l.parentHash = r.opaque()
	default:
		r.fail("leaf_node_source が表に無い")
	}
	l.extensions = readExtensions(r)
	if r.err == nil {
		l.tbs = r.b[start:r.i:r.i]
	}
	l.signature = r.opaque()
	return l
}

func readCredential(r *tlsReader) credential {
	c := credential{typ: r.u16()}
	switch c.typ {
	case credentialBasic:
		c.identity = r.opaque()
	case credentialX509:
		r.vector(func(s *tlsReader) {
			c.certificates = append(c.certificates, s.opaque())
		})
	default:
		r.fail("credential の種類が表に無い")
	}
	return c
}

func readCapabilities(r *tlsReader) capabilities {
	return capabilities{versions: readU16s(r), suites: readU16s(r), extensions: readU16s(r), proposals: readU16s(r), credentials: readU16s(r)}
}

// readU16s は u16 の可変長のベクタ。中身の長さが偶数でなければ要素の境で終わらず誤り。未知の値も残す（RFC 9420 13.4、13.5）。
func readU16s(r *tlsReader) []uint16 {
	out := []uint16{}
	r.vector(func(s *tlsReader) {
		out = append(out, s.u16())
	})
	return out
}

// readExtensions は Extension の可変長のベクタ。同じ種類が 2 つあれば誤り（RFC 9420 13.4）。
func readExtensions(r *tlsReader) []extension {
	out := []extension{}
	seen := map[uint16]bool{}
	r.vector(func(s *tlsReader) {
		e := extension{typ: s.u16(), data: s.opaque()}
		if s.err != nil {
			return
		}
		if seen[e.typ] {
			s.fail("同じ拡張の種類が 2 つ")
			return
		}
		seen[e.typ] = true
		out = append(out, e)
	})
	return out
}

func readKeyPackage(r *tlsReader) *keyPackage {
	start := r.i
	k := &keyPackage{version: r.u16(), suite: r.u16(), initKey: r.opaque()}
	k.leaf = readLeafNode(r)
	k.extensions = readExtensions(r)
	if r.err == nil {
		k.tbs = r.b[start:r.i:r.i]
	}
	k.signature = r.opaque()
	if r.err == nil {
		k.raw = r.b[start:r.i:r.i]
	}
	return k
}

func readWelcome(r *tlsReader) *welcomeMsg {
	w := &welcomeMsg{suite: r.u16()}
	r.vector(func(s *tlsReader) {
		w.secrets = append(w.secrets, encryptedGroupSecrets{newMember: s.opaque(), secrets: readHpkeCiphertext(s)})
	})
	w.encryptedGroupInfo = r.opaque()
	return w
}

func readGroupInfo(r *tlsReader) *groupInfo {
	g := &groupInfo{}
	g.context = groupContext{version: r.u16(), suite: r.u16(), groupId: r.opaque(), epoch: r.u64(), treeHash: r.opaque(),
		confirmedTranscriptHash: r.opaque(), extensions: readExtensions(r)}
	g.extensions = readExtensions(r)
	g.confirmationTag = r.opaque()
	g.signer = r.u32()
	g.signature = r.opaque()
	return g
}

func readPrivateMessage(r *tlsReader) *privateMessage {
	p := &privateMessage{groupId: r.opaque(), epoch: r.u64(), contentType: r.u8()}
	if r.err == nil && (p.contentType < contentApplication || p.contentType > contentCommit) {
		r.fail("内容の種類が表に無い")
	}
	p.authenticatedData = r.opaque()
	p.encryptedSenderData = r.opaque()
	p.ciphertext = r.opaque()
	return p
}
