// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 上のブロックが実装する interface の偽物（srv-keys、srv-push、srv-admin の代わり）。

// fakeKeyReader は srv-keys の代わり。端末の署名鍵の公開部を pubs に置けば VerifyProof が ECDSA で確かめる。
type fakeKeyReader struct {
	pubs *sync.Map // uuid.UUID から *ecdsa.PublicKey
}

func (k fakeKeyReader) VerifyProof(_ context.Context, _ store.Querier, deviceId uuid.UUID, message, sig []byte) (bool, error) {
	if k.pubs == nil {
		return false, nil
	}
	v, ok := k.pubs.Load(deviceId)
	if !ok {
		return false, nil
	}
	h := sha256.Sum256(message)
	return ecdsa.VerifyASN1(v.(*ecdsa.PublicKey), h[:], sig), nil
}

func (fakeKeyReader) SigningKeys(context.Context, store.Querier, []uuid.UUID) (map[uuid.UUID]gen.Fusr1, error) {
	return map[uuid.UUID]gen.Fusr1{}, nil
}

func (fakeKeyReader) DevicesOf(context.Context, store.Querier, uuid.UUID) ([]gen.Fdev1, error) {
	return nil, nil
}

func (fakeKeyReader) KeysOf(context.Context, store.Querier, uuid.UUID) (map[uuid.UUID]gen.Fdev1, error) {
	return map[uuid.UUID]gen.Fdev1{}, nil
}

// fakePush は srv-push の代わり（購読は無い）。
type fakePush struct{}

func (fakePush) SubscribedDevices(context.Context, uuid.UUID) ([]uuid.UUID, error) { return nil, nil }

// auditRow は監査の 1 件。
type auditRow struct {
	actor  uuid.UUID
	action string
	target string
}

// fakeAudit は srv-admin の監査の代わり（記録を覚える）。
type fakeAudit struct {
	mu   sync.Mutex
	rows []auditRow
}

func (a *fakeAudit) Record(_ context.Context, _ *store.Tx, actor uuid.UUID, action, target string, _ map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, auditRow{actor: actor, action: action, target: target})
	return nil
}

// fakeState は srv-admin のサーバーの状態の代わり。
type fakeState struct {
	s boundary.ServerState
}

func (f fakeState) State() boundary.ServerState {
	if f.s == "" {
		return boundary.StateNormal
	}
	return f.s
}

func (fakeState) ReadOnlyReason() string { return "" }

// wall は試験の壁の時計（応答の時間の計測と、試験の時計の始まり。time.Now を直接呼ばない：ftlint）。
func wall() time.Time { return clock.WallClock{}.Now() }
