// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package deps

import "net/netip"

// nonGlobal は IANA の IPv4 と IPv6 の Special-Purpose Address Registry（RFC 6890）で
// 「Globally Reachable」が真でない範囲の全部（2026-10-14 に CSV を取得。37 行。第1章 10.6、調査資料 調査 H 1.16）。
// 6to4（2002::/16）と Teredo（2001::/32）は N/A なので含める。
var nonGlobal = mustPrefixes(
	"0.0.0.0/8", "0.0.0.0/32", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.0.0/29", "192.0.0.8/32", "192.0.0.170/32", "192.0.0.171/32",
	"192.0.2.0/24", "192.88.99.0/24", "192.88.99.2/32", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32",
	"::1/128", "::/128", "::ffff:0:0/96", "64:ff9b:1::/48", "100::/64", "100:0:0:1::/64",
	"2001::/23", "2001::/32", "2001:2::/48", "2001:10::/28", "2001:db8::/32", "2002::/16",
	"3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10",
)

// multicast はマルチキャストの範囲（special-purpose の登録の外）。
var multicast = mustPrefixes("224.0.0.0/4", "ff00::/8")

// nat64 は IPv4 と IPv6 の変換の範囲（登録で真）。中の IPv4 を確かめる。
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// mustPrefixes は範囲の表を作る。書き誤りは起動の前の試験で見つかる。
func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(s))
	for _, x := range s {
		out = append(out, netip.MustParsePrefix(x))
	}
	return out
}

// isInternal は a が内部の範囲か（第1章 10.6）。self は家族のサーバー自身のアドレス。
func isInternal(a netip.Addr, self []netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() {
		return true
	}
	if a.Is6() && nat64.Contains(a) {
		b := a.As16()
		return isInternal(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), self)
	}
	for _, p := range nonGlobal {
		if p.Contains(a) {
			return true
		}
	}
	for _, p := range multicast {
		if p.Contains(a) {
			return true
		}
	}
	for _, s := range self {
		if s.Unmap() == a {
			return true
		}
	}
	return false
}
