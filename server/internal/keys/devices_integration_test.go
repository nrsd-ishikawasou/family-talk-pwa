// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package keys

import (
	"bytes"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// R: R-12-3-2
// 端末の鍵の登録（第12章 5.3、第2章 5.4）：未承認で置き、同じ物の再送は同じ物を返し、違う鍵と credential の食い違いと、他の端末に登録済みの
// 公開鍵（署名鍵、鍵合意鍵、列をまたいでも）は KEY-009。取り消し済みの端末は KEY-001。DeviceKeyReader の 4 つ。
func TestDeviceKeysPut(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	var dv identity.Device
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		dv, err = e.id.Devices.Register(e.ctx, tx, a.id, identity.PlatformPc, "花子の PC")
		return err
	}))
	dk := newDevKeys(t, a.id, dv.Id)
	put := func(sig, kem gen.CommonJwkP2561, cred []byte) (gen.Fdev1, error) {
		var f gen.Fdev1
		err := e.tx(func(tx *store.Tx) error {
			var err error
			f, err = e.k.DeviceKeys.Put(e.ctx, tx, a.id, dv.Id, sig, kem, cred)
			return err
		})
		return f, err
	}
	cred := credentialFor(a.id, dv.Id)
	f, err := put(jwkOf(t, dk.sig), jwkOf(t, dk.kem), cred)
	e.must(err)
	if f.UserSig != nil || f.ApprovedAt != nil || f.MlsCredential != b64.EncodeToString(cred) || f.SigPub != jwkOf(t, dk.sig) {
		t.Fatalf("登録した fdev/1 が違う: %+v", f)
	}
	again, err := put(jwkOf(t, dk.sig), jwkOf(t, dk.kem), cred)
	e.must(err)
	if again.CreatedAt != f.CreatedAt {
		t.Fatal("同じ物の再送で行が替わった")
	}
	_, err = put(jwkOf(t, newKey(t)), jwkOf(t, dk.kem), cred)
	e.wantCode(err, cgen.KEY009)
	_, err = put(jwkOf(t, dk.sig), jwkOf(t, dk.kem), credentialFor(a.id, a.dev().deviceId))
	e.wantCode(err, cgen.KEY009)
	// 他の端末に登録済みの公開鍵。
	var dv2 identity.Device
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		dv2, err = e.id.Devices.Register(e.ctx, tx, a.id, identity.PlatformPc, "花子の 2 台目")
		return err
	}))
	for what, pair := range map[string][2]gen.CommonJwkP2561{
		"署名鍵":  {jwkOf(t, dk.sig), jwkOf(t, newKey(t))},
		"鍵合意鍵": {jwkOf(t, newKey(t)), jwkOf(t, dk.kem)},
		"列をまたぐ（署名に鍵合意）": {jwkOf(t, dk.kem), jwkOf(t, newKey(t))},
	} {
		err := e.tx(func(tx *store.Tx) error {
			_, err := e.k.DeviceKeys.Put(e.ctx, tx, a.id, dv2.Id, pair[0], pair[1], credentialFor(a.id, dv2.Id))
			return err
		})
		if err == nil {
			t.Fatalf("%s の重なりを拒まない", what)
		}
		e.wantCode(err, cgen.KEY009)
	}
	// 取り消し済みの端末と、他人の端末。
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.Revoke(e.ctx, tx, dv2.Id, a.id, false) }))
	err = e.tx(func(tx *store.Tx) error {
		_, err := e.k.DeviceKeys.Put(e.ctx, tx, a.id, dv2.Id, jwkOf(t, newKey(t)), jwkOf(t, newKey(t)), credentialFor(a.id, dv2.Id))
		return err
	})
	e.wantCode(err, cgen.KEY001)
	b := e.member("太郎", boundary.RoleMember)
	err = e.tx(func(tx *store.Tx) error {
		_, err := e.k.DeviceKeys.Put(e.ctx, tx, b.id, dv.Id, jwkOf(t, newKey(t)), jwkOf(t, newKey(t)), credentialFor(b.id, dv.Id))
		return err
	})
	e.wantCode(err, cgen.KEY001)
	// DeviceKeyReader：KeysOf は未承認を含み、DevicesOf は承認済みだけ。SigningKeys は fusr/1。VerifyProof は r と s の 64 バイト。
	r := e.k.Reader()
	keys, err := r.KeysOf(e.ctx, e.pool, a.id)
	e.must(err)
	if _, ok := keys[dv.Id]; !ok || len(keys) != 2 {
		t.Fatalf("KeysOf: %d", len(keys))
	}
	devs, err := r.DevicesOf(e.ctx, e.pool, a.id)
	e.must(err)
	if len(devs) != 1 || devs[0].DeviceId != a.dev().deviceId.String() || devs[0].UserSig == nil {
		t.Fatalf("DevicesOf: %+v", devs)
	}
	sigs, err := r.SigningKeys(e.ctx, e.pool, []uuid.UUID{a.id, b.id, uuid.Must(uuid.NewV7())})
	e.must(err)
	if len(sigs) != 2 || sigs[a.id].Version != 1 || sigs[a.id].UserSigPub != jwkOf(t, a.usk) {
		t.Fatalf("SigningKeys: %+v", sigs)
	}
	msg := append([]byte("ft.device_proof/1"), make([]byte, 33)...)
	ok, err := r.VerifyProof(e.ctx, e.pool, dv.Id, msg, signRS(t, dk.sig, msg))
	e.must(err)
	bad, err := r.VerifyProof(e.ctx, e.pool, dv.Id, msg, signRS(t, dk.kem, msg))
	e.must(err)
	none, err := r.VerifyProof(e.ctx, e.pool, uuid.Must(uuid.NewV7()), msg, signRS(t, dk.sig, msg))
	e.must(err)
	if !ok || bad || none {
		t.Fatalf("VerifyProof: %v %v %v", ok, bad, none)
	}
}

// R: R-12-3-2
// 端末の承認（第12章 5.5 の 2、第3章 4.4）：自己承認と他の端末による承認、user_sig が通らなければ KEY-003、未承認の端末による他の端末の
// 承認は KEY-003、二重は KEY-002。承認で端末の追加待ちが本人のトークに立ち（room.member の pending_add）、その端末の open の回復の求めが
// 閉じる。
func TestDeviceKeysApprove(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	r1 := e.room(a.dev(), a)
	dk := e.addDevice(a, "花子のタブレット", false)
	other := e.addDevice(a, "花子の古い電話", false)
	// 未承認の端末からの他の端末の承認。
	e.wantCode(e.approve(a, other, dk), cgen.KEY003)
	// 通らない user_sig。
	err := e.tx(func(tx *store.Tx) error {
		_, err := e.k.DeviceKeys.Approve(e.ctx, tx, a.id, a.dev().deviceId, dk.deviceId, signRS(t, newKey(t), []byte("x")))
		return err
	})
	e.wantCode(err, cgen.KEY003)
	if e.devState(dk.deviceId) != identity.DeviceUnapproved {
		t.Fatal("拒まれた承認で状態が変わった")
	}
	// 回復の求めを立ててから承認する（家族の片を持つ人が 2 人要るので、求めの行を直に置く）。
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO recovery_requests (id, user_id, device_id, state, expires_at, created_at) VALUES ($1, $2, $3, 'open', $4, $4)`,
		uuid.Must(uuid.NewV7()), a.id, dk.deviceId, e.clk.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.must(e.approve(a, a.dev(), dk))
	if e.devState(dk.deviceId) != identity.DeviceApproved {
		t.Fatal("承認されない")
	}
	if n := e.count(`SELECT count(*) FROM recovery_requests WHERE device_id = $1 AND state = 'open'`, dk.deviceId); n != 0 {
		t.Fatalf("回復の求めが閉じない: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM mls_pending WHERE room_id = $1 AND device_id = $2 AND kind = 'add'`, r1, dk.deviceId); n != 1 {
		t.Fatalf("端末の追加待ちが立たない: %d", n)
	}
	mem := payloads[roomMember](t, e.feedOf(a.id, "room.member"))
	if len(mem) != 1 || mem[0].Change != memberPendingAdd || *mem[0].DeviceId != dk.deviceId {
		t.Fatalf("room.member: %+v", mem)
	}
	e.wantCode(e.approve(a, a.dev(), dk), cgen.KEY002)
	f, err := e.k.DeviceKeys.Get(e.ctx, e.pool, dk.deviceId)
	e.must(err)
	fusr, err := e.k.UserSigningKeys.Get(e.ctx, e.pool, a.id)
	e.must(err)
	if f.ApprovedAt == nil || !e.k.DeviceKeys.VerifyUserSig(*f, *fusr) {
		t.Fatal("承認の後の fdev/1 が本人の署名鍵で通らない")
	}
	// rejoin：既に待ちがあれば立てない。
	var n int
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		n, err = e.k.Groups.RequestAdd(e.ctx, tx, dk.deviceId)
		return err
	}))
	if n != 0 {
		t.Fatalf("二重の追加待ち: %d", n)
	}
}

// R: R-3-6-1
// 子の端末の鍵の承認（第3章 4.4、第12章 5.5 の 2）：保護者の承認が有効で、保護者が通していない端末は MEM-006 で、保護者への求めは要求が
// 失敗しても残る（別のトランザクション）。保護者が通した端末は承認できる。
func TestChildDeviceApprovalNeedsGuardian(t *testing.T) {
	e := newEnv(t)
	parent := e.member("花子", boundary.RoleAdmin)
	var kid identity.User
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		kid, err = e.id.Users.Create(e.ctx, tx, "孫", boundary.RoleMember, &identity.ChildSpec{IsChild: true})
		if err != nil {
			return err
		}
		// 保護者の組は招待の経路が置く（set_by は招待した人）。試験では行を直に置く。
		_, err = tx.Exec(e.ctx, `INSERT INTO guardians (child_id, guardian_id, set_by, set_at) VALUES ($1, $2, $2, $3)`, kid.Id, parent.id, e.clk.Now())
		return err
	}))
	m := &member{id: kid.Id, usk: newKey(t)}
	dk := e.newDevice(kid.Id, "孫の電話")
	m.devs = append(m.devs, dk)
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.UserSigningKeys.Put(e.ctx, tx, m.id, dk.deviceId, jwkOf(t, m.usk), nil)
		return err
	}))
	e.wantCode(e.approve(m, dk, dk), cgen.MEM006)
	if n := e.count(`SELECT count(*) FROM guardian_approvals WHERE child_id = $1 AND target_device_id = $2 AND state = 'open'`, kid.Id, dk.deviceId); n != 1 {
		t.Fatalf("保護者への求めが残らない: %d", n)
	}
	e.wantCode(e.approve(m, dk, dk), cgen.MEM006)
	if n := e.count(`SELECT count(*) FROM guardian_approvals WHERE child_id = $1`, kid.Id); n != 1 {
		t.Fatalf("同じ端末の求めが 2 つ: %d", n)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.MarkGuardianCleared(e.ctx, tx, dk.deviceId) }))
	e.must(e.approve(m, dk, dk))
	if e.devState(dk.deviceId) != identity.DeviceApproved {
		t.Fatal("保護者が通した端末を承認できない")
	}
}

// R: R-12-3-2
// 本人の署名鍵の交代（第12章 5.4 の S の交代）：承認済みと休眠の全端末の user_sig が揃い通る時だけ置き換え、版を 1 増やし、全端末の user_sig を
// 置き換え、古い版のコピーを消し、家族へ user.changed（signing_key）、本人へ keybundle.changed（5 つ）。欠ける・通らない（KEY-003）、知らない
// 端末（KEY-001）、同じ端末が 2 つ（KEY-009）、初回の deviceSigs（KEY-009）、承認済みでない端末からの置き換え（KEY-004）、古い版の書き込み
// （KEY-011）。
func TestSigningKeyRotation(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	d2 := e.addDevice(a, "花子のタブレット", true)
	un := e.addDevice(a, "花子の新しい電話", false)
	// v1 のコピーを置いておく（交代で消える）。
	e.must(e.tx(func(tx *store.Tx) error {
		return e.k.KeyBundles.Put(e.ctx, tx, a.id, a.dev().deviceId, BundleDevice, a.dev().deviceId.String(), deviceCopy(a.id, a.dev().deviceId, 1))
	}))
	next := newKey(t)
	sigFor := func(dk *devKeys) DeviceSig { return DeviceSig{DeviceId: dk.deviceId, UserSig: userSigFor(t, next, dk)} }
	rotate := func(by *devKeys, sigs ...DeviceSig) (gen.Fusr1, error) {
		var f gen.Fusr1
		err := e.tx(func(tx *store.Tx) error {
			var err error
			f, err = e.k.UserSigningKeys.Put(e.ctx, tx, a.id, by.deviceId, jwkOf(t, next), sigs)
			return err
		})
		return f, err
	}
	_, err := rotate(a.dev(), sigFor(a.dev()))
	e.wantCode(err, cgen.KEY003)
	_, err = rotate(a.dev(), sigFor(a.dev()), DeviceSig{DeviceId: d2.deviceId, UserSig: userSigFor(t, a.usk, d2)})
	e.wantCode(err, cgen.KEY003)
	_, err = rotate(a.dev(), sigFor(a.dev()), sigFor(d2), sigFor(un))
	e.wantCode(err, cgen.KEY001)
	_, err = rotate(a.dev(), sigFor(a.dev()), sigFor(d2), sigFor(d2))
	e.wantCode(err, cgen.KEY009)
	_, err = rotate(un, sigFor(a.dev()), sigFor(d2))
	e.wantCode(err, cgen.KEY004)
	err = e.tx(func(tx *store.Tx) error {
		_, err := e.k.UserSigningKeys.Put(e.ctx, tx, b.id, b.dev().deviceId, jwkOf(t, newKey(t)), []DeviceSig{{DeviceId: b.dev().deviceId}})
		return err
	})
	if err == nil {
		t.Fatal("置き換えに欠けた署名が通る")
	}
	if v, _ := e.k.UserSigningKeys.Version(e.ctx, e.pool, a.id); v != 1 {
		t.Fatalf("拒まれた交代で版が変わった: %d", v)
	}
	f, err := rotate(a.dev(), sigFor(a.dev()), sigFor(d2))
	e.must(err)
	if f.Version != 2 || f.RotatedAt == nil || f.UserSigPub != jwkOf(t, next) {
		t.Fatalf("交代の後の fusr/1: %+v", f)
	}
	for _, dk := range []*devKeys{a.dev(), d2} {
		fd, err := e.k.DeviceKeys.Get(e.ctx, e.pool, dk.deviceId)
		e.must(err)
		if !e.k.DeviceKeys.VerifyUserSig(*fd, f) {
			t.Fatal("交代の後の user_sig が新しい鍵で通らない")
		}
	}
	if n := e.count(`SELECT count(*) FROM keybundles WHERE user_id = $1`, a.id); n != 0 {
		t.Fatalf("古い版のコピーが残る: %d", n)
	}
	uc := payloads[userChanged](t, e.feedOf(b.id, "user.changed"))
	if len(uc) != 1 || uc[0].UserId != a.id || uc[0].Fields[0] != fieldSigningKey {
		t.Fatalf("家族への user.changed: %+v", uc)
	}
	kc := payloads[keybundleChanged](t, e.feedOf(a.id, "keybundle.changed"))
	if len(kc) != 2 || len(kc[1].Kinds) != 5 {
		t.Fatalf("本人への keybundle.changed: %+v", kc)
	}
	err = e.tx(func(tx *store.Tx) error {
		return e.k.KeyBundles.Put(e.ctx, tx, a.id, a.dev().deviceId, BundleDevice, a.dev().deviceId.String(), deviceCopy(a.id, a.dev().deviceId, 1))
	})
	ev := e.wantCode(err, cgen.KEY011)
	if ev.Params["version"] != 2 {
		t.Fatalf("KEY-011 の params: %+v", ev.Params)
	}
}

// deviceCopy はコピー (a) の fkb/1（中身は任意の暗号文）。
func deviceCopy(user, device uuid.UUID, version int) gen.Fkb1 {
	return gen.Fkb1{V: 1, Kind: gen.Fkb1KindDevice, UserId: user.String(), TargetId: device.String(), KeyVersion: version, Alg: algAes,
		Nonce: b64.EncodeToString(make([]byte, 12)), Ciphertext: b64.EncodeToString([]byte{1, 2, 3})}
}

// R: R-12-3-2
// KeyPackage（第12章 5.2、I-11）：置く時の確かめ（壊した物は全部を KEY-009）、20 を超えると古い物から消える、最後の手段は 1 つで置き換わる、
// 期限は 90 日と not_after の 1 日前の早い方。取り出しは使い切りから古い順に消し、尽きたら最後の手段（KEY-006 の印。消さない）、それも無ければ
// KEY-001。承認済みでない端末と削除の構成員は取り出せない。期限を過ぎた物は掃除で消える。
func TestKeyPackages(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	dk := a.dev()
	now := e.clk.Now()
	push := func(pkgs [][]byte, last []byte) (int, error) {
		var n int
		err := e.tx(func(tx *store.Tx) error {
			var err error
			n, err = e.k.KeyPackages.Push(e.ctx, tx, dk.deviceId, pkgs, last)
			return err
		})
		return n, err
	}
	good := func() []byte { return buildKeyPackage(t, dk, now, kpOpts{}) }
	_, err := push([][]byte{good(), buildKeyPackage(t, dk, now, kpOpts{before: func(k *keyPackage) { k.suite = 1 }})}, nil)
	e.wantCode(err, cgen.KEY009)
	if n := e.count(`SELECT count(*) FROM keypackages`); n != 0 {
		t.Fatalf("拒まれた Push で置かれた: %d", n)
	}
	other := e.member("太郎", boundary.RoleMember)
	_, err = push([][]byte{buildKeyPackage(t, other.dev(), now, kpOpts{})}, nil)
	e.wantCode(err, cgen.KEY009)
	var first [][]byte
	for range 15 {
		first = append(first, good())
	}
	n, err := push(first, good())
	e.must(err)
	if n != 15 {
		t.Fatalf("数: %d", n)
	}
	e.clk.Advance(time.Second)
	var more [][]byte
	for range 10 {
		more = append(more, good())
	}
	short := buildKeyPackage(t, dk, now, kpOpts{before: func(k *keyPackage) { k.leaf.notAfter = uint64(now.Add(30 * 24 * time.Hour).Unix()) }})
	n, err = push(append(more, short), good())
	e.must(err)
	if n != 20 {
		t.Fatalf("上限 20 にならない: %d", n)
	}
	if c := e.count(`SELECT count(*) FROM keypackages WHERE device_id = $1 AND last_resort`, dk.deviceId); c != 1 {
		t.Fatalf("最後の手段の数: %d", c)
	}
	var firstRefs [][]byte
	for _, p := range first {
		firstRefs = append(firstRefs, refOf(t, p))
	}
	if c := e.count(`SELECT count(*) FROM keypackages WHERE ref = ANY($1)`, firstRefs); c != 9 {
		t.Fatalf("古い物から消えない（最初の 15 のうち残り %d）", c)
	}
	var exp time.Time
	e.scalar(&exp, `SELECT expires_at FROM keypackages WHERE ref = $1`, refOf(t, short))
	if want := time.Unix(int64(uint64(now.Add(30*24*time.Hour).Unix())), 0).Add(-24 * time.Hour); !exp.Equal(want) {
		t.Fatalf("期限が not_after の 1 日前でない: %v %v", exp, want)
	}
	cnt, err := e.k.KeyPackages.Count(e.ctx, dk.deviceId)
	e.must(err)
	if cnt.Count != 20 || !cnt.LastResort {
		t.Fatalf("Count: %+v", cnt)
	}
	take := func(user, dev uuid.UUID) (Taken, error) {
		var tk Taken
		err := e.tx(func(tx *store.Tx) error {
			var err error
			tk, err = e.k.KeyPackages.Take(e.ctx, tx, user, dev)
			return err
		})
		return tk, err
	}
	seen := map[string]bool{}
	for range 20 {
		tk, err := take(a.id, dk.deviceId)
		e.must(err)
		if tk.LastResort || seen[string(tk.Package)] {
			t.Fatal("使い切りが重なるか最後の手段が先に出た")
		}
		seen[string(tk.Package)] = true
	}
	for range 2 {
		tk, err := take(a.id, dk.deviceId)
		e.must(err)
		if !tk.LastResort {
			t.Fatal("尽きた後に最後の手段にならない")
		}
	}
	un := e.addDevice(a, "花子の新しい電話", false)
	_, err = take(a.id, un.deviceId)
	e.wantCode(err, cgen.KEY001)
	_, err = take(other.id, dk.deviceId)
	e.wantCode(err, cgen.KEY001)
	_, err = take(other.id, other.dev().deviceId)
	e.wantCode(err, cgen.KEY001)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, other.id, a.id, false) }))
	_, err = take(other.id, other.dev().deviceId)
	e.wantCode(err, cgen.MEM001)
	e.clk.Advance(91 * 24 * time.Hour)
	gone, err := e.k.KeyPackages.PruneExpired(e.ctx)
	e.must(err)
	if gone != 1 || e.count(`SELECT count(*) FROM keypackages`) != 0 {
		t.Fatalf("期限を過ぎた物が消えない: %d", gone)
	}
	if !bytes.Equal(refOf(t, first[0]), keyPackageRef(keyPackageOf(t, first[0]).raw)) {
		t.Fatal("Ref の求め方が 2 通りで違う")
	}
}
