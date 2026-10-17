// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"os"
	"regexp"
	"testing"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
)

// R: R-12-3-2
// 端末宛メールボックスの種類（第12章 5.2 の表）が境界の 2 つの列挙と一致する：InboxItem.kind は 9 つ全部、putDeviceInbox.kind は
// サーバーだけが置く welcome と share を除いた 7 つ。生成のファイルの値の数も数え、列挙に足された種類を本ブロックが見落とさない。
func TestInboxKindsMatchBoundary(t *testing.T) {
	all := []InboxKind{InboxWelcome, InboxKeybundle, InboxKeyBackup, InboxKeyRequest, InboxShare, InboxCallKeyRequest, InboxCallKey,
		InboxStickerRemoveRequest, InboxStickerRemoveWithdrawal}
	for _, k := range all {
		if !gen.InboxItemKind(k).Valid() {
			t.Errorf("InboxItem.kind に無い: %s", k)
		}
		if put := k != InboxWelcome && k != InboxShare; gen.PutDeviceInboxRequestKind(k).Valid() != put {
			t.Errorf("putDeviceInbox.kind の扱いが違う: %s", k)
		}
	}
	src, err := os.ReadFile("../boundary/gen/boundary.gen.go")
	if err != nil {
		t.Fatal(err)
	}
	for re, want := range map[string]int{`(?m)^\s+InboxItemKind\w+\s+InboxItemKind = "`: len(all),
		`(?m)^\s+PutDeviceInboxRequestKind\w+\s+PutDeviceInboxRequestKind = "`: len(all) - 2} {
		if n := len(regexp.MustCompile(re).FindAll(src, -1)); n != want {
			t.Errorf("生成の列挙の数 %d、本ブロックの種類から %d: %s", n, want, re)
		}
	}
}
