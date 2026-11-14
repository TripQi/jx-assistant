//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"image/color"
	"testing"
)

func dibMaskedFixture(sz, w, h int, comp uint32, masks [4]uint32, pixels []uint32) []byte {
	b := buildDIB(sz, w, h, 32, comp, nil, nil)
	if sz == 40 {
		n := 12
		if comp == 6 {
			n = 16
		}
		b = append(b, make([]byte, n)...)
	}
	for i, m := range masks {
		if i == 3 && (sz == 52 || sz == 40 && comp == 3) {
			break
		}
		binary.LittleEndian.PutUint32(b[40+i*4:], m)
	}
	for _, p := range pixels {
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], p)
		b = append(b, buf[:]...)
	}
	return b
}

func TestDIBHeaderMasksAndPixels(t *testing.T) {
	rgb := [4]uint32{0xff0000, 0xff00, 0xff, 0}
	for _, sz := range []int{40, 52, 56, 108, 124} {
		for _, alpha := range []bool{false, true} {
			if alpha && sz == 52 {
				continue
			}
			t.Run(fmt.Sprintf("header%d/alpha%v", sz, alpha), func(t *testing.T) {
				masks := rgb
				comp := uint32(3)
				if alpha {
					masks[3] = 0xff000000
					if sz == 40 {
						comp = 6
					}
				}
				b := dibMaskedFixture(sz, 1, 1, comp, masks, []uint32{0x80808080})
				p, err := dibToPNG(b)
				if err != nil || p == nil {
					t.Fatalf("bytes=%d err=%v", len(p), err)
				}
				want := color.NRGBA{128, 128, 128, 255}
				if alpha {
					want.A = 128
				}
				if got := pngPixel(t, p, 0, 0); got != want {
					t.Fatalf("pixel=%+v want=%+v", got, want)
				}
			})
		}
		t.Run(fmt.Sprintf("header%d/2x2", sz), func(t *testing.T) {
			b := dibMaskedFixture(sz, 2, -2, 3, rgb, []uint32{0xffff0000, 0xff00ff00, 0xff0000ff, 0xffffffff})
			// 尾部额外数据不改变掩码与像素起点。
			b = append(b, 1, 2, 3, 4)
			p, err := dibToPNG(b)
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range []color.NRGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {255, 255, 255, 255}} {
				if got := pngPixel(t, p, i%2, i/2); got != want {
					t.Fatalf("pixel%d=%+v want=%+v", i, got, want)
				}
			}
		})
	}
	// 连续但非 8 位的通道要归一化，不能直接 uint8 截断。
	b := dibMaskedFixture(56, 1, 1, 3, [4]uint32{0x3ff00000, 0xffc00, 0x3ff, 0xc0000000}, []uint32{0xffffffff})
	p, err := dibToPNG(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := pngPixel(t, p, 0, 0); got != (color.NRGBA{255, 255, 255, 255}) {
		t.Fatal(got)
	}
}

func TestDIBMalformedMasks(t *testing.T) {
	for _, m := range [][4]uint32{
		{0, 0xff00, 0xff, 0}, {0xff0000, 0xff00, 0xff, 0xff808080},
		{0xff0000, 0xff0000, 0xff, 0}, {0xff0000, 0xff00, 0xff, 0xff0000},
	} {
		if p, err := dibToPNG(dibMaskedFixture(56, 1, 1, 3, m, []uint32{0})); p != nil || err == nil {
			t.Fatalf("mask=%x bytes=%d err=%v", m, len(p), err)
		}
	}
	b := dibMaskedFixture(40, 1, 1, 6, [4]uint32{0xff0000, 0xff00, 0xff, 0xff000000}, nil)
	if _, err := dibToPNG(b[:55]); err != errDIBIncomplete {
		t.Fatal(err)
	}
	if _, err := dibToPNG(dibMaskedFixture(52, 1, 1, 6, [4]uint32{0xff0000, 0xff00, 0xff, 0}, []uint32{0})); err == nil {
		t.Fatal("missing alpha accepted")
	}
}
func TestDIBIncompleteAndBudget(t *testing.T) {
	for _, sz := range []uint32{40, 52, 56, 108, 124} {
		t.Run(fmt.Sprint(sz), func(t *testing.T) {
			b := buildDIB(40, 1, 1, 32, 3, nil, nil)
			binary.LittleEndian.PutUint32(b, sz)
			if p, err := dibToPNG(b); p != nil || err != errDIBIncomplete {
				t.Fatalf("bytes=%d err=%v", len(p), err)
			}
		})
	}
	for _, dims := range [][2]int{{0, 1}, {-1, 1}, {1, 0}, {1, -2147483648}, {2147483647, 1}, {20000, 20000}, {4097, 4096}} {
		t.Run(fmt.Sprint(dims), func(t *testing.T) {
			b := buildDIB(40, dims[0], dims[1], 32, 0, nil, nil)
			p, err := dibToPNG(b)
			want := errDIBDimensions
			if dims[0] > 0 && dims[0] <= 20000 && dims[1] > 0 && dims[1] <= 20000 {
				want = errDIBBudget
			}
			if p != nil || err != want {
				t.Fatalf("bytes=%d err=%v want=%v", len(p), err, want)
			}
		})
	}
	// 预算边界本身允许：没有像素时应报残缺，而非预算超限。
	for _, bpp := range []int{24, 32} {
		b := buildDIB(40, 4096, 4096, bpp, 0, nil, nil)
		if _, err := dibToPNG(b); err != errDIBIncomplete {
			t.Fatalf("budget boundary %dbpp: %v", bpp, err)
		}
	}
	// 头只缺最后一个字节时，也必须在读掩码前拒绝。
	for _, sz := range []int{52, 56, 108, 124} {
		b := buildDIB(sz, 1, 1, 32, 3, nil, nil)
		if _, err := dibToPNG(b[:sz-1]); err != errDIBIncomplete {
			t.Fatalf("header %d: %v", sz, err)
		}
	}
	b := buildDIB(40, 2, 2, 24, 0, nil, [][]byte{{0, 0, 255}})
	if _, err := dibToPNG(b); err != errDIBIncomplete {
		t.Fatalf("truncated pixels: %v", err)
	}
}
