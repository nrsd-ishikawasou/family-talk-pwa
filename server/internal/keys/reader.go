// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
)

// reader は srv-identity の DeviceKeyReader の実装（B-081。Keys.Reader）。
type reader struct {
	devices *DeviceKeys
	signing *UserSigningKeys
}

var _ identity.DeviceKeyReader = reader{}

// VerifyProof は DeviceKeys.VerifyProof（第12章 5.3 の device_sig）。
func (r reader) VerifyProof(ctx context.Context, deviceId uuid.UUID, message, sig []byte) (bool, error) {
	return r.devices.VerifyProof(ctx, deviceId, message, sig)
}

// SigningKeys は構成員ごとの fusr/1（/users の user_sig_pub。行の無い構成員は入れない）。
func (r reader) SigningKeys(ctx context.Context, userIds []uuid.UUID) (map[uuid.UUID]gen.Fusr1, error) {
	out := make(map[uuid.UUID]gen.Fusr1, len(userIds))
	for _, u := range userIds {
		f, err := r.signing.Get(ctx, u)
		if err != nil {
			return nil, err
		}
		if f != nil {
			out[u] = *f
		}
	}
	return out, nil
}

// DevicesOf は承認済みと休眠の端末の fdev/1（/users/{id}）。
func (r reader) DevicesOf(ctx context.Context, userId uuid.UUID) ([]gen.Fdev1, error) {
	return r.devices.ListOfUser(ctx, userId, true)
}

// KeysOf は登録のある全端末の fdev/1 を端末の識別子から引く（GET /me/devices の keys）。
func (r reader) KeysOf(ctx context.Context, userId uuid.UUID) (map[uuid.UUID]gen.Fdev1, error) {
	list, err := r.devices.ListOfUser(ctx, userId, false)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]gen.Fdev1, len(list))
	for _, f := range list {
		id, err := uuid.FromString(f.DeviceId)
		if err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, nil
}
