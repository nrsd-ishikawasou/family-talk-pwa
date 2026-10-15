// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
)

// IconVariant は manifest のアイコンの用途（any、maskable、monochrome）。
type IconVariant string

// アイコンの用途の 3 つ。
const (
	IconAny        IconVariant = "any"
	IconMaskable   IconVariant = "maskable"
	IconMonochrome IconVariant = "monochrome"
)

// Icon は同梱の絵柄を家族の色で着色した PNG を作る（第8章 3.1）。/data/www/family/icon-<size>-<variant>.png にキャッシュする。
// 絵柄はコードの中の多角形（家の形と窓と戸）で、画像の資産を持たない。
type Icon struct {
	dir string
	mu  sync.Mutex
}

// iconPath は manifest の icons の src（caddy が /data/www から配る）。
func iconPath(size int, variant IconVariant) string {
	return "/family/" + iconName(size, variant)
}

func iconName(size int, variant IconVariant) string {
	return "icon-" + strconv.Itoa(size) + "-" + string(variant) + ".png"
}

// 絵柄の箱の大きさ（アイコンの一辺に対する割合）。maskable は安全域（中心から半径 40% の円。W3C Web App Manifest）に
// 収まる正方形の一辺（0.8 / √2 ≒ 0.566）より小さく、any と monochrome は余白 10%。
const (
	glyphBoxAny      = 0.80
	glyphBoxMaskable = 0.56
	superSample      = 4
)

// inHouse は絵柄の箱の中の点（0〜1）が家の形の中か。屋根の三角、壁の四角から窓 2 つと戸を抜いた形。
func inHouse(x, y float64) bool {
	inRoof := y >= 0.08 && y <= 0.48 && x >= 0.5-(y-0.08)*1.1 && x <= 0.5+(y-0.08)*1.1
	inWall := x >= 0.16 && x <= 0.84 && y >= 0.44 && y <= 0.92
	if !inRoof && !inWall {
		return false
	}
	inDoor := x >= 0.42 && x <= 0.58 && y >= 0.66 && y <= 0.92
	inWin1 := x >= 0.23 && x <= 0.35 && y >= 0.55 && y <= 0.67
	inWin2 := x >= 0.65 && x <= 0.77 && y >= 0.55 && y <= 0.67
	return !inDoor && !inWin1 && !inWin2
}

// parseColor は #RRGGBB を読む。
func parseColor(s string) (color.NRGBA, error) {
	if len(s) != 7 || s[0] != '#' {
		return color.NRGBA{}, errors.New("identity: 色の形が #RRGGBB でない")
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return color.NRGBA{}, err
	}
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}, nil
}

// Render は 1 つの大きさと用途の PNG を返す。被覆率は 4×4 の超標本化で求め、アルファにする。
// any は透明の地に家族の色の絵柄、maskable は家族の色の地の全面に白の絵柄、monochrome は白の絵柄とアルファだけ（色は端末が付ける）。
func (i *Icon) Render(colorHex string, size int, variant IconVariant) ([]byte, error) {
	fg, err := parseColor(colorHex)
	if err != nil {
		return nil, fault.Wrap(err)
	}
	if size <= 0 {
		return nil, fault.Wrap(errors.New("identity: アイコンの大きさが 0 以下"))
	}
	box := glyphBoxAny
	ink := fg
	var bg color.NRGBA
	switch variant {
	case IconAny:
	case IconMaskable:
		box, ink, bg = glyphBoxMaskable, color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}, fg
	case IconMonochrome:
		ink = color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	default:
		return nil, fault.Wrap(errors.New("identity: アイコンの用途が違う"))
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	n := float64(size)
	off := (1 - box) / 2
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			hits := 0
			for sy := 0; sy < superSample; sy++ {
				for sx := 0; sx < superSample; sx++ {
					x := ((float64(px)+(float64(sx)+0.5)/superSample)/n - off) / box
					y := ((float64(py)+(float64(sy)+0.5)/superSample)/n - off) / box
					if x >= 0 && x <= 1 && y >= 0 && y <= 1 && inHouse(x, y) {
						hits++
					}
				}
			}
			cov := float64(hits) / (superSample * superSample)
			img.SetNRGBA(px, py, blend(bg, ink, cov))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fault.Wrap(err)
	}
	return buf.Bytes(), nil
}

// blend は地 bg の上に絵柄 ink を被覆率 cov で重ねる（ストレートのアルファ）。
func blend(bg, ink color.NRGBA, cov float64) color.NRGBA {
	if bg.A == 0 {
		return color.NRGBA{R: ink.R, G: ink.G, B: ink.B, A: uint8(float64(ink.A)*cov + 0.5)}
	}
	mix := func(a, b uint8) uint8 { return uint8(float64(a)*(1-cov) + float64(b)*cov + 0.5) }
	return color.NRGBA{R: mix(bg.R, ink.R), G: mix(bg.G, ink.G), B: mix(bg.B, ink.B), A: 0xff}
}

// ensure はキャッシュのファイルが無ければ作る。一時の名前に書いて改名する（半端なファイルを残さない）。
// 色は隣の icon-color に置き、違えば（バックアップから別の家族を戻した時など）全部の大きさを消して作り直す。
func (i *Icon) ensure(colorHex string, size int, variant IconVariant) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	colorFile := filepath.Join(i.dir, "icon-color")
	if cur, err := os.ReadFile(colorFile); err != nil || string(cur) != colorHex {
		for _, s := range iconSizes {
			if err := os.Remove(filepath.Join(i.dir, iconName(s.size, s.variant))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fault.Wrap(err)
			}
		}
		if err := writeAtomic(i.dir, "icon-color", []byte(colorHex)); err != nil {
			return fault.Wrap(err)
		}
	}
	if _, err := os.Stat(filepath.Join(i.dir, iconName(size, variant))); err == nil {
		return nil
	}
	b, err := i.Render(colorHex, size, variant)
	if err != nil {
		return err
	}
	if err := writeAtomic(i.dir, iconName(size, variant), b); err != nil {
		return fault.Wrap(err)
	}
	return nil
}

// read は 1 つのファイルを返す（無ければ作る）。GET /family/icon は 192 の any。
func (i *Icon) read(colorHex string, size int, variant IconVariant) ([]byte, error) {
	if err := i.ensure(colorHex, size, variant); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(i.dir, iconName(size, variant)))
	if err != nil {
		return nil, fault.Wrap(err)
	}
	return b, nil
}

// writeAtomic は dir に一時の名前で書いて name に改名する。
func writeAtomic(dir, name string, b []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}
