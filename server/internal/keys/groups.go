// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 再初期化の決まった値（第12章 5.2「拒まれた Commit からの復旧」）。
const (
	reinitWait   = 10 * time.Minute // 最後の報告から Commit が無いまま過ぎる時間（条件 (c)）
	reinitWindow = 24 * time.Hour   // 上限を数える時間
	reinitMax    = 2                // 1 端末 1 トークの上限
)

// Group は MLS のグループの 1 行（mls_groups。トークごとに 1 つ。第12章 5.2）。
type Group struct {
	RoomId uuid.UUID
	// Generation はグループの世代（0 から。再初期化で 1 増える）。
	Generation   int
	Epoch        int64
	Suite        int
	LastCommitAt time.Time
}

// Leaf は leaf の番号とその端末と encryption_key（mls_leaves。作成者の leaf 0 は最初の path 付きの Commit まで nil）。
type Leaf struct {
	Index         int
	DeviceId      uuid.UUID
	EncryptionKey []byte
}

// EpochInfo は GET /rooms/{id}/mls/epoch の本ブロックの分（応答の pending_add は srv-rooms が足す）。
type EpochInfo struct {
	Generation int
	// GroupId は現在の世代のグループの識別子（20 バイト）。
	GroupId           []byte
	Epoch             int64
	PendingAddDevices []uuid.UUID
	PendingRemove     []uuid.UUID
	PendingReadd      []uuid.UUID
}

// CommitItem は GET /rooms/{id}/mls/commits の 1 件（epoch は Commit で進んだ後の epoch）。
type CommitItem struct {
	Epoch  int64
	Commit []byte
	// CommitRef は Commit の参照（MLSMessage の SHA-256 の base64url。第12章 5.2）。
	CommitRef string
}

// CommitPage は GET /rooms/{id}/mls/commits の応答の本ブロックの分（現在の世代と項目と has_more）。
type CommitPage struct {
	Generation int
	Items      []CommitItem
	HasMore    bool
}

// CommitResult は受け付けた Commit の結果（srv-rooms Members.Resolve が構成員の待ちを解く元）。
type CommitResult struct {
	Epoch     int64
	CommitRef string
	Added     []uuid.UUID
	Removed   []uuid.UUID
}

// RejectResult は拒みの報告の結果（第2章 5.5）。
type RejectResult struct {
	// ReinitAllowedAt はこの端末が再初期化を求めてよい時刻。
	ReinitAllowedAt time.Time
}

// ReinitResult は再初期化の結果。
type ReinitResult struct {
	Generation int
	// GroupId は新しい世代のグループの識別子（20 バイト）。
	GroupId []byte
	Epoch   int64
}

// RoomDevice はトークと端末の組（休眠の leaf）。
type RoomDevice struct {
	RoomId   uuid.UUID
	DeviceId uuid.UUID
}

// PendingKind は端末の待ちの種類（mls_pending.kind）。
type PendingKind string

// 待ちの 3 つ（追加待ち、削除待ち、再参加待ち）。
const (
	PendingAdd    PendingKind = "add"
	PendingRemove PendingKind = "remove"
	PendingReadd  PendingKind = "readd"
)

// PendingDevice は残っている端末の待ち（srv-worker の監視。add が 7 日残れば知らせ）。
type PendingDevice struct {
	RoomId   uuid.UUID
	DeviceId uuid.UUID
	Kind     PendingKind
	Since    time.Time
}

// Groups は MLS のグループの世代と epoch、leaf の記録、端末の待ち、Commit、拒みの報告と再初期化を持つ（B-086。第12章 5.2、5.5）。
type Groups struct {
	d       *Deps
	keys    *Keys
	reader  *CommitReader
	inbox   *Inbox
	bundles *KeyBundles
	devices *DeviceKeys
}

// idStrings は識別子の並びを文字列にする（`= ANY($1::uuid[])` に渡す）。
func idStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// Create は世代 0、epoch 0 のグループを作り、leaf 0 に creatorDevice を置く（encryption_key は最初の path 付きの Commit で埋まる。
// トークの作成と同じトランザクション。srv-rooms から）。
func (g *Groups) Create(ctx context.Context, tx *store.Tx, roomId, creatorDevice uuid.UUID) error {
	now := g.d.now()
	if _, err := tx.Exec(ctx, `INSERT INTO mls_groups (room_id, generation, epoch, suite, last_commit_at) VALUES ($1, 0, 0, $2, $3)`,
		roomId, mlsSuite, now); err != nil {
		return wrap(err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO mls_leaves (room_id, leaf_index, device_id, since_epoch) VALUES ($1, 0, $2, 0)`, roomId, creatorDevice)
	return wrap(err)
}

// readGroup は世代と epoch（lock なら FOR UPDATE）。無ければ ROOM-001。
func (g *Groups) readGroup(ctx context.Context, q store.Querier, roomId uuid.UUID, lock bool) (int, int64, error) {
	sql := `SELECT generation, epoch FROM mls_groups WHERE room_id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var gen int
	var epoch int64
	err := q.QueryRow(ctx, sql, roomId).Scan(&gen, &epoch)
	if noRows(err) {
		return 0, 0, ev(cgen.ROOM001)
	}
	return gen, epoch, wrap(err)
}

// Epoch は世代、グループの識別子、epoch と端末の待ち。グループが無ければ ROOM-001。
func (g *Groups) Epoch(ctx context.Context, roomId uuid.UUID) (EpochInfo, error) {
	gen, epoch, err := g.readGroup(ctx, g.d.Pool, roomId, false)
	if err != nil {
		return EpochInfo{}, err
	}
	out := EpochInfo{Generation: gen, GroupId: groupIdOf(roomId, gen), Epoch: epoch, PendingAddDevices: []uuid.UUID{}, PendingRemove: []uuid.UUID{},
		PendingReadd: []uuid.UUID{}}
	rows, err := g.d.Pool.Query(ctx, `SELECT device_id, kind FROM mls_pending WHERE room_id = $1 ORDER BY device_id`, roomId)
	if err != nil {
		return EpochInfo{}, wrap(err)
	}
	defer rows.Close()
	for rows.Next() {
		var dev uuid.UUID
		var kind string
		if err := rows.Scan(&dev, &kind); err != nil {
			return EpochInfo{}, wrap(err)
		}
		switch PendingKind(kind) {
		case PendingAdd:
			out.PendingAddDevices = append(out.PendingAddDevices, dev)
		case PendingRemove:
			out.PendingRemove = append(out.PendingRemove, dev)
		case PendingReadd:
			out.PendingReadd = append(out.PendingReadd, dev)
		}
	}
	return out, wrap(rows.Err())
}

// CommitsSince は現在の世代の、sinceEpoch より後の Commit を epoch の順に最大 limit 件（上限 100）と has_more と世代。世代と項目は
// 1 つの文で読む（再初期化と混ざらない）。グループが無ければ ROOM-001。
func (g *Groups) CommitsSince(ctx context.Context, roomId uuid.UUID, sinceEpoch int64, limit int) (CommitPage, error) {
	if limit < 1 || limit > commitListMax {
		limit = commitListMax
	}
	rows, err := g.d.Pool.Query(ctx, `SELECT g.generation, c.epoch, c.commit, c.commit_ref FROM mls_groups g LEFT JOIN LATERAL
  (SELECT epoch, commit, commit_ref FROM mls_commits WHERE room_id = g.room_id AND epoch > $2 ORDER BY epoch LIMIT $3) c ON true
WHERE g.room_id = $1`, roomId, sinceEpoch, limit+1)
	if err != nil {
		return CommitPage{}, wrap(err)
	}
	defer rows.Close()
	page := CommitPage{Items: []CommitItem{}}
	found := false
	for rows.Next() {
		var gen int
		var epoch *int64
		var commit []byte
		var ref *string
		if err := rows.Scan(&gen, &epoch, &commit, &ref); err != nil {
			return CommitPage{}, wrap(err)
		}
		found, page.Generation = true, gen
		if epoch != nil {
			page.Items = append(page.Items, CommitItem{Epoch: *epoch, Commit: commit, CommitRef: *ref})
		}
	}
	if err := rows.Err(); err != nil {
		return CommitPage{}, wrap(err)
	}
	if !found {
		return CommitPage{}, ev(cgen.ROOM001)
	}
	if len(page.Items) > limit {
		page.Items, page.HasMore = page.Items[:limit], true
	}
	return page, nil
}

// commitRef は Commit の参照（MLSMessage のバイト列の SHA-256 の base64url。第12章 5.2）。
func commitRef(commit []byte) string {
	h := sha256.Sum256(commit)
	return b64.EncodeToString(h[:])
}

// loadLeaves は leaf の記録（番号から引く）。
func (g *Groups) loadLeaves(ctx context.Context, q store.Querier, roomId uuid.UUID) (map[int]Leaf, error) {
	list, err := g.Leaves(ctx, q, roomId)
	if err != nil {
		return nil, err
	}
	out := make(map[int]Leaf, len(list))
	for _, l := range list {
		out[l.Index] = l
	}
	return out, nil
}

// width は木の幅（記録の最大の番号 m について 2^d > m の最小の 2^d。RFC 9420 12.1.1、12.1.3）。
func width(rec map[int]Leaf) int {
	w := 1
	for i := range rec {
		for w <= i {
			w *= 2
		}
	}
	return w
}

// freeLeaf は Add の置き場（RFC 9420 12.1.1）：0 から w - 1 の中で記録に無い最小の番号。無ければ w で、木の幅を 2 倍にする。
// w は Remove を当てた後の記録の幅から始め、Add のたびに同じ w を渡す（12.3 の順）。
func freeLeaf(rec map[int]Leaf, w *int) int {
	for i := 0; i < *w; i++ {
		if _, used := rec[i]; !used {
			return i
		}
	}
	i := *w
	*w *= 2
	return i
}

// Commit は POST /rooms/{id}/mls/commit の本体（srv-rooms が受け、同じトランザクションで本操作、srv-rooms Members.Resolve の順に呼ぶ）。
// 第12章 5.2「Commit の確かめ」の 9 つを順に確かめ、1 つでも成り立たなければ KEY-009 で epoch は進まない。RoomLock は本操作が取る
// （休眠の Add の端末を承認済みへ戻す devices の行の錠を RoomLock より先に取るため）。epoch が現在と違うか、グループの識別子の世代が
// 古ければ KEY-007。leaf の記録と待ちを更新し、Commit を置き、Welcome を Add の各端末へ、room.mls と room.member を構成員へ流す。
func (g *Groups) Commit(ctx context.Context, tx *store.Tx, roomId, byDevice uuid.UUID, expectedEpoch int64, commit, welcome []byte) (CommitResult, error) {
	pc, err := g.reader.ParseCommit(commit)
	if err != nil {
		return CommitResult{}, err
	}
	var pw *ParsedWelcome
	if welcome != nil {
		w, err := g.reader.ParseWelcome(welcome)
		if err != nil {
			return CommitResult{}, err
		}
		pw = &w
	}
	devs := g.d.Identity.Devices
	if err := g.wakeDormant(ctx, tx, pc.Adds); err != nil {
		return CommitResult{}, err
	}
	if err := g.d.RoomLock.Lock(ctx, tx, roomId); err != nil {
		return CommitResult{}, err
	}
	gen, epoch, err := g.readGroup(ctx, tx, roomId, true)
	if err != nil {
		return CommitResult{}, err
	}
	if epoch != expectedEpoch {
		return CommitResult{}, ev(cgen.KEY007)
	}
	if !bytes.Equal(pc.GroupId, groupIdOf(roomId, gen)) {
		if len(pc.GroupId) == groupIdLen && bytes.Equal(pc.GroupId[:16], roomId.Bytes()) {
			return CommitResult{}, ev(cgen.KEY007)
		}
		return CommitResult{}, ev(cgen.KEY009)
	}
	if pc.Epoch != expectedEpoch {
		return CommitResult{}, ev(cgen.KEY009)
	}
	rec, err := g.loadLeaves(ctx, tx, roomId)
	if err != nil {
		return CommitResult{}, err
	}
	bad := ev(cgen.KEY009)
	// 1. 送信者の leaf が送信した端末の leaf。
	sender, ok := rec[pc.SenderLeaf]
	if !ok || sender.DeviceId != byDevice {
		return CommitResult{}, bad
	}
	// 2. Remove の番号が記録にあり送信者でなく、同じ番号が 2 つ無い（順に記録に当てる）。
	work := make(map[int]Leaf, len(rec))
	for i, l := range rec {
		work[i] = l
	}
	var removed []uuid.UUID
	for _, r := range pc.Removes {
		l, ok := work[r]
		if !ok || r == pc.SenderLeaf {
			return CommitResult{}, bad
		}
		delete(work, r)
		removed = append(removed, l.DeviceId)
	}
	// 3. 値で含めた Update が無い。
	if pc.Updates > 0 {
		return CommitResult{}, bad
	}
	now := g.d.now()
	inWork := map[uuid.UUID]bool{}
	usedKeys := map[string]bool{}
	for _, l := range work {
		inWork[l.DeviceId] = true
		if l.EncryptionKey != nil {
			usedKeys[string(l.EncryptionKey)] = true
		}
	}
	owners := map[uuid.UUID]uuid.UUID{}
	var added []uuid.UUID
	for _, a := range pc.Adds {
		did, uid := a.Leaf.DeviceId, a.Leaf.UserId
		if did == uuid.Nil || uid == uuid.Nil {
			return CommitResult{}, bad
		}
		// 4. KeyPackage の 9 つの確かめと今の時刻。
		reg, ok, err := readRegistration(ctx, tx, did, false)
		if err != nil {
			return CommitResult{}, err
		}
		if !ok || reg.userId != uid || !checkKeyPackage(a, reg, now, false) {
			return CommitResult{}, bad
		}
		// 5. 端末が承認済みか休眠で、トークの構成員（追加待ちを含む）の物で、Remove の後の記録に無く、Add の中で重ならない。
		dv, err := devs.Get(ctx, tx, did)
		if isEvent(err, cgen.KEY001) {
			return CommitResult{}, bad
		}
		if err != nil {
			return CommitResult{}, err
		}
		if dv.UserId != uid || (dv.State != identity.DeviceApproved && dv.State != identity.DeviceDormant) {
			return CommitResult{}, bad
		}
		member, err := g.keys.rooms.IsMemberOrPending(ctx, tx, roomId, uid)
		if err != nil {
			return CommitResult{}, wrap(err)
		}
		if !member || inWork[did] || owners[did] != uuid.Nil {
			return CommitResult{}, bad
		}
		// 6. encryption_key が記録のどれとも、他の Add の物とも違う。
		if usedKeys[string(a.Leaf.EncryptionKey)] {
			return CommitResult{}, bad
		}
		usedKeys[string(a.Leaf.EncryptionKey)] = true
		owners[did] = uid
		added = append(added, did)
	}
	// 7. 提案が空か、Remove を含めば path が要る（RFC 9420 12.4）。
	if (len(pc.Adds) == 0 && len(pc.Removes) == 0 || len(pc.Removes) > 0) && pc.Path == nil {
		return CommitResult{}, bad
	}
	// 8. path の leaf。
	if p := pc.Path; p != nil {
		reg, ok, err := readRegistration(ctx, tx, byDevice, false)
		if err != nil {
			return CommitResult{}, err
		}
		if !ok || !bytes.Equal(p.Credential, reg.credential) || !bytes.Equal(p.SigPub, reg.sigSec1) || p.Source != leafSourceCommit ||
			!capabilitiesOk(*p) || !p.VerifySignature(reg.sigPub, pc.GroupId, pc.SenderLeaf) || usedKeys[string(p.EncryptionKey)] {
			return CommitResult{}, bad
		}
	}
	// 9. Welcome の宛先が Add の KeyPackageRef の集合と同じ。Add が無ければ Welcome が無い。
	if !welcomeMatches(pc.Adds, pw) {
		return CommitResult{}, bad
	}
	ref, err := g.apply(ctx, tx, roomId, gen, expectedEpoch, byDevice, commit, pc, work)
	if err != nil {
		return CommitResult{}, err
	}
	if err := g.resolvePending(ctx, tx, roomId, added, removed); err != nil {
		return CommitResult{}, err
	}
	for _, did := range added {
		if _, err := g.inbox.PutWelcome(ctx, tx, byDevice, did, welcome); err != nil {
			return CommitResult{}, err
		}
	}
	if err := g.emitCommit(ctx, tx, roomId, gen, expectedEpoch+1, ref, byDevice, owners, added, removed); err != nil {
		return CommitResult{}, err
	}
	return CommitResult{Epoch: expectedEpoch + 1, CommitRef: ref, Added: sortedIds(added), Removed: sortedIds(removed)}, nil
}

// wakeDormant は Add の端末の devices の行を、RoomLock より先に識別子の順に共有の錠で押さえ（取り消しと直列にする。I-8）、
// 休眠の物を承認済みへ戻す。Commit が拒まれればトランザクションごと戻る。
func (g *Groups) wakeDormant(ctx context.Context, tx *store.Tx, adds []ParsedKeyPackage) error {
	var ids []uuid.UUID
	for _, a := range adds {
		if a.Leaf.DeviceId != uuid.Nil {
			ids = append(ids, a.Leaf.DeviceId)
		}
	}
	devs, err := g.lockDevices(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, dv := range devs {
		if dv.State == identity.DeviceDormant {
			if err := g.d.Identity.Devices.MarkActive(ctx, tx, dv.Id); err != nil {
				return err
			}
		}
	}
	return nil
}

// lockDevices は端末の devices の行を識別子の順に共有の錠で読む（無い端末は飛ばす。トークの錠より先に取る）。
func (g *Groups) lockDevices(ctx context.Context, tx *store.Tx, ids []uuid.UUID) ([]identity.Device, error) {
	var out []identity.Device
	for _, id := range sortedIds(ids) {
		dv, err := g.d.Identity.Devices.LockShared(ctx, tx, id)
		if isEvent(err, cgen.KEY001) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, dv)
	}
	return out, nil
}

// welcomeMatches は確かめ 9：Add があれば Welcome があり、スイートが 0x0002 で、new_member の集合が Add の KeyPackageRef の集合と
// 同じ（数も同じで重なりが無い）。Add が無ければ Welcome が無い。
func welcomeMatches(adds []ParsedKeyPackage, w *ParsedWelcome) bool {
	if len(adds) == 0 {
		return w == nil
	}
	if w == nil || w.Suite != mlsSuite || len(w.NewMembers) != len(adds) {
		return false
	}
	want := map[string]bool{}
	for _, a := range adds {
		want[string(a.Ref)] = true
	}
	seen := map[string]bool{}
	for _, m := range w.NewMembers {
		if !want[string(m)] || seen[string(m)] {
			return false
		}
		seen[string(m)] = true
	}
	return true
}

// apply は受け付けた Commit を記録に反映する（leaf の番号は RFC 9420 12.1.1、12.1.3、12.3。I-4）。work は Remove を当てた後の記録。
// 戻りは commit_ref。
func (g *Groups) apply(ctx context.Context, tx *store.Tx, roomId uuid.UUID, gen int, expectedEpoch int64, byDevice uuid.UUID, commit []byte,
	pc ParsedCommit, work map[int]Leaf) (string, error) {
	if len(pc.Removes) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM mls_leaves WHERE room_id = $1 AND leaf_index = ANY($2::int[])`, roomId, pc.Removes); err != nil {
			return "", wrap(err)
		}
	}
	w := width(work)
	for _, a := range pc.Adds {
		i := freeLeaf(work, &w)
		work[i] = Leaf{Index: i, DeviceId: a.Leaf.DeviceId, EncryptionKey: a.Leaf.EncryptionKey}
		if _, err := tx.Exec(ctx, `INSERT INTO mls_leaves (room_id, leaf_index, device_id, encryption_key, since_epoch) VALUES ($1, $2, $3, $4, $5)`,
			roomId, i, a.Leaf.DeviceId, a.Leaf.EncryptionKey, expectedEpoch+1); err != nil {
			return "", wrap(err)
		}
	}
	if pc.Path != nil {
		if _, err := tx.Exec(ctx, `UPDATE mls_leaves SET encryption_key = $3 WHERE room_id = $1 AND leaf_index = $2`,
			roomId, pc.SenderLeaf, pc.Path.EncryptionKey); err != nil {
			return "", wrap(err)
		}
	}
	now := g.d.now()
	tag, err := tx.Exec(ctx, `UPDATE mls_groups SET epoch = epoch + 1, last_commit_at = $3 WHERE room_id = $1 AND epoch = $2`, roomId, expectedEpoch, now)
	if err != nil {
		return "", wrap(err)
	}
	if tag.RowsAffected() != 1 {
		return "", ev(cgen.KEY007)
	}
	ref := commitRef(commit)
	if _, err := tx.Exec(ctx, `INSERT INTO mls_commits (room_id, generation, epoch, commit, commit_ref, by_device, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		roomId, gen, expectedEpoch+1, commit, ref, byDevice, now); err != nil {
		return "", wrap(err)
	}
	return ref, nil
}

// resolvePending は Commit で解けた待ちの行を消す（add は足した端末、remove は外した端末、readd は外して足した端末）。
func (g *Groups) resolvePending(ctx context.Context, tx *store.Tx, roomId uuid.UUID, added, removed []uuid.UUID) error {
	var both []uuid.UUID
	for _, d := range added {
		if slices.Contains(removed, d) {
			both = append(both, d)
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM mls_pending WHERE room_id = $1 AND ((kind = 'add' AND device_id = ANY($2::uuid[]))
  OR (kind = 'remove' AND device_id = ANY($3::uuid[])) OR (kind = 'readd' AND device_id = ANY($4::uuid[])))`,
		roomId, idStrings(added), idStrings(removed), idStrings(both))
	return wrap(err)
}

// emitCommit は room.mls（Welcome を置いた端末の持ち主は has_welcome_for_me が真。payload ごとに id を分ける）と、足した端末と外した
// 端末ごとの room.member を流す。宛先はトークの構成員と端末の持ち主。
func (g *Groups) emitCommit(ctx context.Context, tx *store.Tx, roomId uuid.UUID, gen int, epoch int64, ref string, byDevice uuid.UUID,
	owners map[uuid.UUID]uuid.UUID, added, removed []uuid.UUID) error {
	members, err := g.keys.rooms.MembersOf(ctx, tx, roomId)
	if err != nil {
		return wrap(err)
	}
	welcomed := map[uuid.UUID]bool{}
	for _, u := range owners {
		welcomed[u] = true
	}
	var yes, no []uuid.UUID
	for _, u := range members {
		if welcomed[u] {
			yes = append(yes, u)
		} else {
			no = append(no, u)
		}
	}
	for _, part := range []struct {
		to   []uuid.UUID
		mine bool
	}{{yes, true}, {no, false}} {
		if len(part.to) == 0 {
			continue
		}
		if err := g.d.emit(tx, part.to, "room.mls", roomId, uuid.Nil, roomMls{Generation: gen, Epoch: epoch, CommitRef: ref, HasWelcomeForMe: part.mine}); err != nil {
			return err
		}
	}
	by, err := g.d.Identity.Devices.Get(ctx, tx, byDevice)
	if err != nil {
		return err
	}
	for _, c := range []struct {
		list   []uuid.UUID
		change string
	}{{sortedIds(added), memberAdded}, {sortedIds(removed), memberRemoved}} {
		for _, did := range c.list {
			dv, err := g.d.Identity.Devices.Get(ctx, tx, did)
			if err != nil {
				return err
			}
			d, b := did, by.UserId
			if err := g.d.emit(tx, append(slices.Clone(members), dv.UserId), "room.member", roomId, uuid.Nil,
				roomMember{UserId: dv.UserId, DeviceId: &d, Change: c.change, By: &b}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reject は POST /rooms/{id}/mls/reject（srv-rooms が受ける。第12章 5.2 の復旧の 1、第2章 5.5）。generation が現在の世代でなければ
// KEY-007。epoch が無ければ現在の epoch で、1 以上で現在以下でなければ KEY-009。commitRef があればその epoch の Commit の参照と同じで
// なければ KEY-009。byDevice が leaf を持たなければ KEY-009。報告を記録し、その端末を再参加待ちにして room.member（pending_readd）を
// 構成員へ流す。戻りは再初期化を求めてよい時刻。
func (g *Groups) Reject(ctx context.Context, tx *store.Tx, roomId, byDevice uuid.UUID, generation int, epoch *int64, commitRef *string, reason string) (RejectResult, error) {
	if err := g.d.RoomLock.Lock(ctx, tx, roomId); err != nil {
		return RejectResult{}, err
	}
	gen, cur, err := g.readGroup(ctx, tx, roomId, true)
	if err != nil {
		return RejectResult{}, err
	}
	if generation != gen {
		return RejectResult{}, ev(cgen.KEY007)
	}
	e := cur
	if epoch != nil {
		e = *epoch
	}
	if e < 1 || e > cur {
		return RejectResult{}, ev(cgen.KEY009)
	}
	if commitRef != nil {
		var stored string
		err := tx.QueryRow(ctx, `SELECT commit_ref FROM mls_commits WHERE room_id = $1 AND epoch = $2`, roomId, e).Scan(&stored)
		if err != nil && !noRows(err) {
			return RejectResult{}, wrap(err)
		}
		if err != nil || stored != *commitRef {
			return RejectResult{}, ev(cgen.KEY009)
		}
	}
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM mls_leaves WHERE room_id = $1 AND device_id = $2`, roomId, byDevice).Scan(&one)
	if noRows(err) {
		return RejectResult{}, ev(cgen.KEY009)
	}
	if err != nil {
		return RejectResult{}, wrap(err)
	}
	now := g.d.now()
	if _, err := tx.Exec(ctx, `INSERT INTO mls_rejections (room_id, generation, epoch, device_id, commit_ref, reason, reported_at) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (room_id, generation, epoch, device_id) DO UPDATE SET commit_ref = EXCLUDED.commit_ref, reason = EXCLUDED.reason, reported_at = EXCLUDED.reported_at`,
		roomId, gen, e, byDevice, commitRef, reason, now); err != nil {
		return RejectResult{}, wrap(err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO mls_pending (room_id, device_id, kind, since) VALUES ($1, $2, 'readd', $3)
ON CONFLICT (room_id, device_id) DO UPDATE SET kind = 'readd', since = EXCLUDED.since WHERE mls_pending.kind <> 'readd'`, roomId, byDevice, now)
	if err != nil {
		return RejectResult{}, wrap(err)
	}
	if tag.RowsAffected() == 1 {
		if err := g.emitPending(ctx, tx, roomId, byDevice, memberPendingReadd); err != nil {
			return RejectResult{}, err
		}
	}
	_, at, err := g.reinitVerdict(ctx, tx, roomId, gen, cur, byDevice, now)
	if err != nil {
		return RejectResult{}, err
	}
	return RejectResult{ReinitAllowedAt: at.UTC()}, nil
}

// emitPending は端末の待ちの room.member（pending_add、pending_remove、pending_readd）を構成員へ流す。
func (g *Groups) emitPending(ctx context.Context, tx *store.Tx, roomId, deviceId uuid.UUID, change string) error {
	dv, err := g.d.Identity.Devices.Get(ctx, tx, deviceId)
	if err != nil {
		return err
	}
	members, err := g.keys.rooms.MembersOf(ctx, tx, roomId)
	if err != nil {
		return wrap(err)
	}
	d := deviceId
	return g.d.emit(tx, members, "room.member", roomId, uuid.Nil, roomMember{UserId: dv.UserId, DeviceId: &d, Change: change})
}

// reinitVerdict は再初期化を許すかと、許す時刻（第12章 5.2「サーバーが再初期化を許す条件」）。(a) 現在の epoch の Commit を作った端末
// 以外の全 leaf の端末が現在の epoch を拒んだ、(b) byDevice が現在の世代で 2 回以上拒んだ、のどちらかなら今。(c) byDevice の最後の
// 報告 r から 10 分が過ぎ、r の後に Commit が無ければ r + 10 分で許す。どれでもなければ許さず、時刻は r + 10 分（報告が無ければ今 + 10 分）。
func (g *Groups) reinitVerdict(ctx context.Context, q store.Querier, roomId uuid.UUID, gen int, cur int64, byDevice uuid.UUID, now time.Time) (bool, time.Time, error) {
	var a bool
	err := q.QueryRow(ctx, `WITH o AS (SELECT device_id FROM mls_leaves WHERE room_id = $1
    AND device_id IS DISTINCT FROM (SELECT by_device FROM mls_commits WHERE room_id = $1 AND epoch = $3))
SELECT count(*) > 0 AND count(*) = (SELECT count(*) FROM mls_rejections r JOIN o ON o.device_id = r.device_id
    WHERE r.room_id = $1 AND r.generation = $2 AND r.epoch = $3) FROM o`, roomId, gen, cur).Scan(&a)
	if err != nil {
		return false, time.Time{}, wrap(err)
	}
	var n int
	var last *time.Time
	if err := q.QueryRow(ctx, `SELECT count(*), max(reported_at) FROM mls_rejections WHERE room_id = $1 AND generation = $2 AND device_id = $3`,
		roomId, gen, byDevice).Scan(&n, &last); err != nil {
		return false, time.Time{}, wrap(err)
	}
	if a || n >= 2 {
		return true, now, nil
	}
	if last == nil {
		return false, now.Add(reinitWait), nil
	}
	at := last.Add(reinitWait)
	var after bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mls_commits WHERE room_id = $1 AND created_at > $2)`, roomId, *last).Scan(&after); err != nil {
		return false, time.Time{}, wrap(err)
	}
	return !after && !at.After(now), at, nil
}

// reinitDenied は KEY-013（params.reason と allowed_at。再試行は too_early だけ可）。
func reinitDenied(reason string, at *time.Time) error {
	params := map[string]any{"reason": reason}
	if at != nil {
		params["allowed_at"] = at.UTC().Format(time.RFC3339)
	}
	e := fault.New(cgen.KEY013, params)
	e.Retryable = reason == "too_early"
	return e
}

// reinitCheck は再初期化を許す 3 つの判定（再参加待ちか、許す条件、24 時間の上限）。上限に当たれば、管理者への知らせ mls_reinit_limit を
// 別のトランザクションで確定してから KEY-013（limit）。戻りは今の世代。
func (g *Groups) reinitCheck(ctx context.Context, tx *store.Tx, roomId, byDevice uuid.UUID, lock bool, now time.Time) (int, error) {
	gen, cur, err := g.readGroup(ctx, tx, roomId, lock)
	if err != nil {
		return 0, err
	}
	var kind string
	err = tx.QueryRow(ctx, `SELECT kind FROM mls_pending WHERE room_id = $1 AND device_id = $2`, roomId, byDevice).Scan(&kind)
	if err != nil && !noRows(err) {
		return 0, wrap(err)
	}
	if kind != string(PendingReadd) {
		return 0, reinitDenied("not_pending", nil)
	}
	ok, at, err := g.reinitVerdict(ctx, tx, roomId, gen, cur, byDevice, now)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, reinitDenied("too_early", &at)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM mls_reinits WHERE room_id = $1 AND by_device = $2 AND created_at > $3`,
		roomId, byDevice, now.Add(-reinitWindow)).Scan(&n); err != nil {
		return 0, wrap(err)
	}
	if n >= reinitMax {
		// 知らせは notices と feed の行だけを書き、要求のトランザクションが錠を持つ行（rooms、mls_*）を書かず指さない。
		if err := g.d.separateTx(ctx, func(stx *store.Tx) error {
			return g.notifyReinit(ctx, stx, roomId, byDevice, noticeMlsReinitLimit, nil)
		}); err != nil {
			return 0, err
		}
		return 0, reinitDenied("limit", nil)
	}
	return gen, nil
}

// notifyReinit は管理者への知らせ mls_reinit（generation あり）と mls_reinit_limit（第7章 9）。
func (g *Groups) notifyReinit(ctx context.Context, tx *store.Tx, roomId, byDevice uuid.UUID, kind string, generation *int) error {
	dv, err := g.d.Identity.Devices.Get(ctx, tx, byDevice)
	if err != nil {
		return err
	}
	usr, err := g.d.Identity.Users.Get(ctx, tx, dv.UserId)
	if err != nil {
		return err
	}
	name, err := g.d.Identity.Devices.NameOf(ctx, tx, byDevice)
	if err != nil {
		return err
	}
	params := map[string]any{"user_id": dv.UserId.String(), "name": usr.DisplayName, "device_name": name, "room_id": roomId.String()}
	if generation != nil {
		params["generation"] = *generation
	}
	return g.d.Identity.Hooks.Notify(ctx, tx, identity.Notice{Kind: kind, Params: params, Audience: identity.Audience{Kind: identity.AudienceAdmins}})
}

// reinitTarget は再初期化で追加待ちにする端末とその持ち主。
type reinitTarget struct {
	user   uuid.UUID
	device uuid.UUID
}

// reinitTargets は再初期化で入るべき端末：構成員のうち active か pending_add の人（RoomReader.IsMemberOrPending）の、承認済みの端末
// （休眠は含めない）から byDevice を除いた物（第12章 5.2）。端末の識別子の順。
func (g *Groups) reinitTargets(ctx context.Context, tx *store.Tx, roomId, byDevice uuid.UUID) ([]reinitTarget, []uuid.UUID, error) {
	members, err := g.keys.rooms.MembersOf(ctx, tx, roomId)
	if err != nil {
		return nil, nil, wrap(err)
	}
	var out []reinitTarget
	for _, u := range sortedIds(members) {
		in, err := g.keys.rooms.IsMemberOrPending(ctx, tx, roomId, u)
		if err != nil {
			return nil, nil, wrap(err)
		}
		if !in {
			continue
		}
		devs, err := g.d.Identity.Devices.OfUser(ctx, tx, u)
		if err != nil {
			return nil, nil, err
		}
		for _, dv := range devs {
			if dv.State == identity.DeviceApproved && dv.Id != byDevice {
				out = append(out, reinitTarget{user: u, device: dv.Id})
			}
		}
	}
	slices.SortFunc(out, func(x, y reinitTarget) int { return bytes.Compare(x.device[:], y.device[:]) })
	return out, members, nil
}

// Reinit は POST /rooms/{id}/mls/reinit（srv-rooms が受ける。第12章 5.2 の復旧の 2）。錠の前と錠の中で reinitCheck（再参加待ちでなければ
// KEY-013 not_pending、条件の前なら too_early、24 時間に 2 回を超えれば limit）。入るべき端末の devices の行を、トークの錠の前に共有の錠で
// 押さえる（取り消しと直列にする）。世代を 1 増やして epoch 0、前の世代の leaf の記録・待ち・Commit・報告を消し、byDevice を leaf 0 に
// 置き、入るべき端末を追加待ちにする。room.member（pending_add）と room.mls（新しい世代、epoch 0）を構成員へ流し、管理者へ知らせ
// mls_reinit。
func (g *Groups) Reinit(ctx context.Context, tx *store.Tx, roomId, byDevice uuid.UUID) (ReinitResult, error) {
	now := g.d.now()
	if _, err := g.reinitCheck(ctx, tx, roomId, byDevice, false, now); err != nil {
		return ReinitResult{}, err
	}
	pre, _, err := g.reinitTargets(ctx, tx, roomId, byDevice)
	if err != nil {
		return ReinitResult{}, err
	}
	ids := make([]uuid.UUID, len(pre))
	for i, t := range pre {
		ids[i] = t.device
	}
	locked, err := g.lockDevices(ctx, tx, ids)
	if err != nil {
		return ReinitResult{}, err
	}
	if err := g.d.RoomLock.Lock(ctx, tx, roomId); err != nil {
		return ReinitResult{}, err
	}
	gen, err := g.reinitCheck(ctx, tx, roomId, byDevice, true, now)
	if err != nil {
		return ReinitResult{}, err
	}
	next := gen + 1
	if _, err := tx.Exec(ctx, `UPDATE mls_groups SET generation = $2, epoch = 0, last_commit_at = $3 WHERE room_id = $1`, roomId, next, now); err != nil {
		return ReinitResult{}, wrap(err)
	}
	for _, t := range []string{"mls_leaves", "mls_pending", "mls_commits", "mls_rejections"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE room_id = $1`, roomId); err != nil {
			return ReinitResult{}, wrap(err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mls_leaves (room_id, leaf_index, device_id, since_epoch) VALUES ($1, 0, $2, 0)`, roomId, byDevice); err != nil {
		return ReinitResult{}, wrap(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mls_reinits (room_id, generation, by_device, created_at) VALUES ($1, $2, $3, $4)`, roomId, next, byDevice, now); err != nil {
		return ReinitResult{}, wrap(err)
	}
	// 錠の中で求め直す（錠の前の後に構成員や端末が変わった分）。錠の前に押さえた端末は、その時の状態が承認済みの物だけを置く。
	targets, members, err := g.reinitTargets(ctx, tx, roomId, byDevice)
	if err != nil {
		return ReinitResult{}, err
	}
	state := map[uuid.UUID]identity.DeviceState{}
	for _, dv := range locked {
		state[dv.Id] = dv.State
	}
	for _, t := range targets {
		if st, ok := state[t.device]; ok && st != identity.DeviceApproved {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mls_pending (room_id, device_id, kind, since) VALUES ($1, $2, 'add', $3)`, roomId, t.device, now); err != nil {
			return ReinitResult{}, wrap(err)
		}
		d := t.device
		if err := g.d.emit(tx, members, "room.member", roomId, uuid.Nil, roomMember{UserId: t.user, DeviceId: &d, Change: memberPendingAdd}); err != nil {
			return ReinitResult{}, err
		}
	}
	if err := g.d.emit(tx, members, "room.mls", roomId, uuid.Nil, roomMls{Generation: next, Epoch: 0}); err != nil {
		return ReinitResult{}, err
	}
	if err := g.notifyReinit(ctx, tx, roomId, byDevice, noticeMlsReinit, &next); err != nil {
		return ReinitResult{}, err
	}
	return ReinitResult{Generation: next, GroupId: groupIdOf(roomId, next), Epoch: 0}, nil
}

// CheckSender は送信の時の外形の検査（srv-rooms Timeline.Post の 4。第12章 5.2）。本文の外側を CommitReader.ParsePrivateHeader で読み、
// wire format と content_type が違えば ROOM-005、外側と本文の group_id が同じ 20 バイトでこのトークの物でなければ ROOM-005、group_id の
// 世代か外側と本文の epoch が現在と違えば ROOM-006、送信した端末が leaf の記録に無ければ ROOM-006。グループが無ければ ROOM-001。
// q は呼ぶ側（RoomLock を持つ Timeline.Post）のトランザクション。
func (g *Groups) CheckSender(ctx context.Context, q store.Querier, roomId, device uuid.UUID, body gen.FtEventMls1) error {
	ct, ok := decodeB64(body.Ciphertext)
	if !ok {
		return ev(cgen.ROOM005)
	}
	h, err := g.reader.ParsePrivateHeader(ct)
	if err != nil || h.ContentType != contentApplication {
		return ev(cgen.ROOM005)
	}
	gid, ok := decodeB64(body.GroupId)
	if !ok || !bytes.Equal(gid, h.GroupId) || len(gid) != groupIdLen || !bytes.Equal(gid[:16], roomId.Bytes()) {
		return ev(cgen.ROOM005)
	}
	generation, cur, err := g.readGroup(ctx, q, roomId, false)
	if err != nil {
		return err
	}
	if binary.BigEndian.Uint32(gid[16:]) != uint32(generation) || h.Epoch != cur || int64(body.Epoch) != cur {
		return ev(cgen.ROOM006)
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM mls_leaves WHERE room_id = $1 AND device_id = $2`, roomId, device).Scan(&one)
	if noRows(err) {
		return ev(cgen.ROOM006)
	}
	return wrap(err)
}

// Leaves は leaf の番号と端末と encryption_key（番号の順）。q は ctx のトランザクションか Pool。
func (g *Groups) Leaves(ctx context.Context, q store.Querier, roomId uuid.UUID) ([]Leaf, error) {
	rows, err := q.Query(ctx, `SELECT leaf_index, device_id, encryption_key FROM mls_leaves WHERE room_id = $1 ORDER BY leaf_index`, roomId)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	out := []Leaf{}
	for rows.Next() {
		var l Leaf
		if err := rows.Scan(&l.Index, &l.DeviceId, &l.EncryptionKey); err != nil {
			return nil, wrap(err)
		}
		out = append(out, l)
	}
	return out, wrap(rows.Err())
}

// DormantLeafs は srv-identity Devices.ApprovedSeenBefore(olderThan) の端末が持つ leaf（srv-worker DormantLeafHandler。端末とトークの順）。
func (g *Groups) DormantLeafs(ctx context.Context, olderThan time.Time) ([]RoomDevice, error) {
	devs, err := g.d.Identity.Devices.ApprovedSeenBefore(ctx, olderThan)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(devs))
	for i, dv := range devs {
		ids[i] = dv.Id
	}
	rows, err := g.d.Pool.Query(ctx, `SELECT room_id, device_id FROM mls_leaves WHERE device_id = ANY($1::uuid[]) ORDER BY device_id, room_id`, idStrings(ids))
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	out := []RoomDevice{}
	for rows.Next() {
		var rd RoomDevice
		if err := rows.Scan(&rd.RoomId, &rd.DeviceId); err != nil {
			return nil, wrap(err)
		}
		out = append(out, rd)
	}
	return out, wrap(rows.Err())
}

// RetireDormant は休眠（第12章 5.2）：srv-worker から端末ごとに 1 トランザクション。srv-identity Devices.MarkDormant(olderThan) が
// 変えなければ何もしない（判定の後に使われた端末を休眠にしない）。変えればその端末のコピー (a) だけを消し（その端末へ包んだ他人の片は
// 残す）、各グループに削除待ちを置く。
func (g *Groups) RetireDormant(ctx context.Context, tx *store.Tx, deviceId uuid.UUID, olderThan time.Time) error {
	changed, err := g.d.Identity.Devices.MarkDormant(ctx, tx, deviceId, olderThan)
	if err != nil || !changed {
		return err
	}
	if err := g.bundles.deleteDeviceCopy(ctx, tx, deviceId); err != nil {
		return err
	}
	return g.removeLeafsOfDevices(ctx, tx, []uuid.UUID{deviceId})
}

// RemoveLeafsOfDevice は取り消し・休眠：その端末が leaf を持つ各グループに削除待ちを置き、room.member（pending_remove）を構成員へ流す。
// leaf の無いトークのその端末の追加待ちと再参加待ちは消す。leaf の記録は Remove の Commit で消える。
func (g *Groups) RemoveLeafsOfDevice(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	return g.removeLeafsOfDevices(ctx, tx, []uuid.UUID{deviceId})
}

// removeLeafsOfDevices は複数の端末の分をまとめ、トークの錠をトークの識別子の順に取る（削除の hook が全端末を 1 度に渡す）。
func (g *Groups) removeLeafsOfDevices(ctx context.Context, tx *store.Tx, devices []uuid.UUID) error {
	ids := idStrings(devices)
	rooms, err := scanIds(ctx, tx, `SELECT room_id FROM mls_leaves WHERE device_id = ANY($1::uuid[]) UNION SELECT room_id FROM mls_pending WHERE device_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return err
	}
	for _, r := range sortedIds(rooms) {
		if err := g.d.RoomLock.Lock(ctx, tx, r); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT room_id, device_id FROM mls_leaves WHERE device_id = ANY($1::uuid[]) ORDER BY room_id, device_id`, ids)
	if err != nil {
		return wrap(err)
	}
	var leaves []RoomDevice
	for rows.Next() {
		var rd RoomDevice
		if err := rows.Scan(&rd.RoomId, &rd.DeviceId); err != nil {
			rows.Close()
			return wrap(err)
		}
		leaves = append(leaves, rd)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return wrap(err)
	}
	now := g.d.now()
	for _, rd := range leaves {
		tag, err := tx.Exec(ctx, `INSERT INTO mls_pending (room_id, device_id, kind, since) VALUES ($1, $2, 'remove', $3)
ON CONFLICT (room_id, device_id) DO UPDATE SET kind = 'remove', since = EXCLUDED.since WHERE mls_pending.kind <> 'remove'`, rd.RoomId, rd.DeviceId, now)
		if err != nil {
			return wrap(err)
		}
		if tag.RowsAffected() == 1 {
			if err := g.emitPending(ctx, tx, rd.RoomId, rd.DeviceId, memberPendingRemove); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM mls_pending p WHERE p.device_id = ANY($1::uuid[]) AND p.kind IN ('add', 'readd')
  AND NOT EXISTS (SELECT 1 FROM mls_leaves l WHERE l.room_id = p.room_id AND l.device_id = p.device_id)`, ids)
	return wrap(err)
}

// scanIds は識別子の 1 列を読む。
func scanIds(ctx context.Context, q store.Querier, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, wrap(err)
		}
		out = append(out, id)
	}
	return out, wrap(rows.Err())
}

// RequestAdd は端末の追加待ち（第12章 5.5 の 4）：本人が構成員のトーク（RoomReader.RoomsOf）をトークの識別子の順に錠を取り、グループが
// あってその端末の leaf も待ちも無いトークに追加待ちを置き、room.member（pending_add）を構成員へ流す。戻りは置いた数
// （POST /me/mls/rejoin の応答）。
func (g *Groups) RequestAdd(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) (int, error) {
	dv, err := g.d.Identity.Devices.LockShared(ctx, tx, deviceId)
	if err != nil {
		return 0, err
	}
	rooms, err := g.keys.rooms.RoomsOf(ctx, tx, dv.UserId)
	if err != nil {
		return 0, wrap(err)
	}
	n := 0
	now := g.d.now()
	for _, r := range sortedIds(rooms) {
		if err := g.d.RoomLock.Lock(ctx, tx, r); err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO mls_pending (room_id, device_id, kind, since)
SELECT $1, $2, 'add', $3 WHERE EXISTS (SELECT 1 FROM mls_groups WHERE room_id = $1)
  AND NOT EXISTS (SELECT 1 FROM mls_leaves WHERE room_id = $1 AND device_id = $2)
ON CONFLICT (room_id, device_id) DO NOTHING`, r, deviceId, now)
		if err != nil {
			return 0, wrap(err)
		}
		if tag.RowsAffected() == 1 {
			n++
			if err := g.emitPending(ctx, tx, r, deviceId, memberPendingAdd); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

// PendingDevices は残っている端末の待ち（立てた時刻の順。srv-worker の監視）。
func (g *Groups) PendingDevices(ctx context.Context) ([]PendingDevice, error) {
	rows, err := g.d.Pool.Query(ctx, `SELECT room_id, device_id, kind, since FROM mls_pending ORDER BY since, room_id, device_id`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	out := []PendingDevice{}
	for rows.Next() {
		var p PendingDevice
		var kind string
		if err := rows.Scan(&p.RoomId, &p.DeviceId, &kind, &p.Since); err != nil {
			return nil, wrap(err)
		}
		p.Kind, p.Since = PendingKind(kind), p.Since.UTC()
		out = append(out, p)
	}
	return out, wrap(rows.Err())
}

// dropPendingAdds は停止で本人の端末の追加待ちを消す（hook OnUserSuspended。leaf と削除待ちは触らない）。トークの錠を識別子の順に取る。
func (g *Groups) dropPendingAdds(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	devs, err := g.d.Identity.Devices.OfUser(ctx, tx, userId)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(devs))
	for i, dv := range devs {
		ids[i] = dv.Id
	}
	rooms, err := scanIds(ctx, tx, `SELECT DISTINCT room_id FROM mls_pending WHERE device_id = ANY($1::uuid[]) AND kind = 'add'`, idStrings(ids))
	if err != nil {
		return err
	}
	for _, r := range sortedIds(rooms) {
		if err := g.d.RoomLock.Lock(ctx, tx, r); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM mls_pending WHERE device_id = ANY($1::uuid[]) AND kind = 'add'`, idStrings(ids))
	return wrap(err)
}
