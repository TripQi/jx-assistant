//go:build windows

package main

// dibToPNG 的针对性单测：只测本次改动新增的 DIB 解码（24/32bpp、上下行序、
// 掩码、无 alpha 按不透明）。真剪贴板全链路（/api/clipboard + 右键菜单）由
// gui-test/ctxmenu-paste-verify.mjs 真机验证，不在这里硬造。

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"image/png"
	"testing"
)

// buildDIB 造一个最小 DIB（BITMAPINFOHEADER 40 字节或 V5 124 字节）
func buildDIB(hdr int, w int, h int, bpp int, comp uint32, v5Masks *[4]uint32, rows [][]byte) []byte {
	le := binary.LittleEndian
	hdrBuf := make([]byte, hdr)
	le.PutUint32(hdrBuf[0:4], uint32(hdr))
	le.PutUint32(hdrBuf[4:8], uint32(w))
	le.PutUint32(hdrBuf[8:12], uint32(h)) // 负值 = 自上而下
	le.PutUint16(hdrBuf[12:14], 1)
	le.PutUint16(hdrBuf[14:16], uint16(bpp))
	le.PutUint32(hdrBuf[16:20], comp)
	if v5Masks != nil { // V5 头：红绿蓝 alpha 掩码在头内固定位移
		le.PutUint32(hdrBuf[40:44], v5Masks[0])
		le.PutUint32(hdrBuf[44:48], v5Masks[1])
		le.PutUint32(hdrBuf[48:52], v5Masks[2])
		le.PutUint32(hdrBuf[52:56], v5Masks[3])
	}
	out := append([]byte(nil), hdrBuf...)
	for _, r := range rows {
		out = append(out, r...)
		if pad := (w*bpp/8 + 3) &^ 3; pad > len(r) {
			out = append(out, make([]byte, pad-len(r))...)
		}
	}
	return out
}

func pngPixel(t *testing.T, data []byte, x, y int) color.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

func near(a, b uint8) bool { d := int(a) - int(b); return d >= -2 && d <= 2 }

func TestDibToPNG24BppBottomUp(t *testing.T) {
	// 2×2、24bpp、自下而上存储：首行存的是底行（蓝、白），次行是顶行（红、绿）
	dib := buildDIB(40, 2, 2, 24, 0, nil, [][]byte{
		{255, 0, 0, 255, 255, 255}, // y=1: (0,1)蓝 (1,1)白
		{0, 0, 255, 0, 255, 0},     // y=0: (0,0)红 (1,0)绿
	})
	p, _ := dibToPNG(dib)
	if p == nil {
		t.Fatal("24bpp DIB 转换返回 nil")
	}
	for _, c := range []struct {
		x, y    int
		r, g, b uint8
	}{
		{0, 0, 255, 0, 0}, {1, 0, 0, 255, 0},
		{0, 1, 0, 0, 255}, {1, 1, 255, 255, 255},
	} {
		got := pngPixel(t, p, c.x, c.y)
		if got.A != 255 || !near(got.R, c.r) || !near(got.G, c.g) || !near(got.B, c.b) {
			t.Errorf("像素(%d,%d) = %+v，期望 rgb(%d,%d,%d) a=255", c.x, c.y, got, c.r, c.g, c.b)
		}
	}
}

func TestDibToPNG32BppAlphaUnusedIsOpaque(t *testing.T) {
	// 32bpp BI_RGB、自上而下：GDI DIB 第四字节常是 0x00 未用，必须按不透明出图
	dib := buildDIB(40, 1, -1, 32, 0, nil, [][]byte{
		{255, 0, 0, 0}, // 蓝，alpha 字节 0
	})
	p, _ := dibToPNG(dib)
	if p == nil {
		t.Fatal("32bpp BI_RGB DIB 转换返回 nil")
	}
	got := pngPixel(t, p, 0, 0)
	if got.A != 255 || !near(got.R, 0) || !near(got.G, 0) || !near(got.B, 255) {
		t.Errorf("像素 = %+v，期望不透明纯蓝", got)
	}
}

func TestDibToPNGV5BitFieldsWithAlpha(t *testing.T) {
	// V5 头 + BI_BITFIELDS，且通道排布故意用「红在最低字节」的非默认序：
	// 掩码解析必须各就各位，alpha 掩码必须被尊重
	masks := [4]uint32{0x000000FF, 0x0000FF00, 0x00FF0000, 0xFF000000}
	le := binary.LittleEndian
	px := make([]byte, 4)
	le.PutUint32(px, 0x80FF4020) // r=0x20 g=0x40 b=0xFF a=0x80
	dib := buildDIB(124, 1, -1, 32, 3, &masks, [][]byte{px})
	p, _ := dibToPNG(dib)
	if p == nil {
		t.Fatal("V5 BI_BITFIELDS DIB 转换返回 nil")
	}
	got := pngPixel(t, p, 0, 0)
	if !near(got.R, 0x20) || !near(got.G, 0x40) || !near(got.B, 0xFF) || !near(got.A, 0x80) {
		t.Errorf("像素 = %+v，期望 NRGBA(0x20,0x40,0xFF,0x80)", got)
	}
}

func TestDibToPNGUnsupported(t *testing.T) {
	if got, _ := dibToPNG([]byte{1, 2, 3}); got != nil {
		t.Errorf("截断输入应返回 nil，得到 %d 字节", len(got))
	}
	// 8bpp 调色板形态：不支持，返回 nil（前端退回文本路径）
	dib := buildDIB(40, 1, 1, 8, 0, nil, [][]byte{{0}})
	if got, _ := dibToPNG(dib); got != nil {
		t.Errorf("8bpp DIB 应返回 nil，得到 %d 字节", len(got))
	}
	// 行数据不足：nil
	dib = buildDIB(40, 4, 4, 24, 0, nil, [][]byte{{0, 0, 255}})
	if got, _ := dibToPNG(dib); got != nil {
		t.Errorf("数据截断应返回 nil，得到 %d 字节", len(got))
	}
}
