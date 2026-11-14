//go:build windows

package main

// 读 Windows 系统剪贴板（文本 + 图片 PNG + 文件列表），给 /api/clipboard 端点用。
//
// 为什么需要它：桌面壳里 navigator.clipboard.readText() 会永久挂起——
// WebView2 对剪贴板读权限不弹授权 UI，jchv/go-webview2 的
// SetPermission(ClipboardRead, Allow) 对该 API 也不生效（权限态停在
// prompt，promise 永不结算，真机诊断见 gui-test/ctxmenu-clipboard-diag.mjs）；
// document.execCommand('paste') 同样被拒——真机带用户激活实测一律返回 false
// 且不产生 paste 事件（判定见 gui-test/ctxmenu-paste-diag.mjs）。按项目惯例
// （浏览器侧做不到的交给本机后端，先例：pick-file 原生选框），由 Go 直接读
// 剪贴板，让右键「粘贴」与 Ctrl+V（原生 paste 事件，WebView2 里可用）同口径：
//   - CF_HDROP：资源管理器复制的文件列表（Ctrl+V 时 Chromium 会把同源文件
//     读成 File 对象交给页面，见 diag 的 ctrlV+files 场景）；
//   - PNG 注册格式（"PNG"）→ 原样字节；没有时解 CF_DIBV5/CF_DIB 转 PNG
//     （截图工具常见只放 DIB，diag 的 ctrlV+dib-image 证明 Chromium 原生
//     paste 也自己解码 DIB 给页面 image.png）；
//   - CF_UNICODETEXT：纯文本。
// 三样一次读全返回，优先级（文件>图片>文本）由前端按 Ctrl+V 的口径分流。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	clipUser32                     = windows.NewLazySystemDLL("user32.dll")
	clipKernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	clipShell32                    = windows.NewLazySystemDLL("shell32.dll")
	procOpenClipboard              = clipUser32.NewProc("OpenClipboard")
	procCloseClipboard             = clipUser32.NewProc("CloseClipboard")
	procIsClipboardFormatAvailable = clipUser32.NewProc("IsClipboardFormatAvailable")
	procGetClipboardData           = clipUser32.NewProc("GetClipboardData")
	procRegisterClipboardFormatW   = clipUser32.NewProc("RegisterClipboardFormatW")
	procGlobalLock                 = clipKernel32.NewProc("GlobalLock")
	procGlobalUnlock               = clipKernel32.NewProc("GlobalUnlock")
	procGlobalSize                 = clipKernel32.NewProc("GlobalSize")
	procDragQueryFileW             = clipShell32.NewProc("DragQueryFileW")
)

const (
	cfUnicodeText = 13
	cfHDROP       = 15
	cfDIB         = 8
	cfDIBV5       = 17
)

var errClipboardBusy = errors.New("系统剪贴板正被其他程序占用")

// sysPtr 把系统调用返回的句柄/地址（uintptr）还原成 unsafe.Pointer。vet 的
// unsafeptr 检查不认 LazyProc.Call 的返回值（这其实是 unsafe.Pointer 规则 (4)
// 的合法场景：系统调用结果立即转回），这里经一次双指针重解释完成等价转换——
// 各调用点紧邻 Call、中间无函数调用（无 GC 挂起窗口），与直接转换同样安全。
func sysPtr(u *uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(u))
}

// clipBytes 取某剪贴板格式的数据副本（GlobalLock → 拷贝 → GlobalUnlock）。
// 剪贴板必须已打开。取不到返回 nil。
func clipBytes(format uintptr) []byte {
	if r, _, _ := procIsClipboardFormatAvailable.Call(format); r == 0 {
		return nil
	}
	h, _, _ := procGetClipboardData.Call(format)
	if h == 0 {
		return nil
	}
	g, _, _ := procGlobalLock.Call(h)
	if g == 0 {
		return nil
	}
	defer procGlobalUnlock.Call(h)
	size, _, _ := procGlobalSize.Call(h)
	if size == 0 {
		return nil
	}
	return append([]byte(nil), unsafe.Slice((*byte)(sysPtr(&g)), int(size))...)
}

// clipFiles 读 CF_HDROP 的文件路径列表（资源管理器里复制的文件）。DragQueryFileW
// 直接吃 GetClipboardData 返回的句柄；剪贴板必须已打开。
func clipFiles() []string {
	if r, _, _ := procIsClipboardFormatAvailable.Call(cfHDROP); r == 0 {
		return nil
	}
	h, _, _ := procGetClipboardData.Call(cfHDROP)
	if h == 0 {
		return nil
	}
	n, _, _ := procDragQueryFileW.Call(h, 0xFFFFFFFF, 0, 0)
	if n == 0 {
		return nil
	}
	var out []string
	for i := uintptr(0); i < n; i++ {
		l, _, _ := procDragQueryFileW.Call(h, i, 0, 0)
		if l == 0 {
			continue
		}
		buf := make([]uint16, l+1)
		if r2, _, _ := procDragQueryFileW.Call(h, i, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r2 != 0 {
			out = append(out, windows.UTF16ToString(buf))
		}
	}
	return out
}

// readClipboardWindows 读系统剪贴板三样：文本、图片 PNG 字节、文件路径列表。
// 剪贴板被他人占用时小退避重试几次。图片优先 PNG 注册格式（原样无损），
// 没有再解 CF_DIBV5/CF_DIB（见 dibToPNG 的口径说明）。
func readClipboardWindows() (text string, png []byte, files []string, err error) {
	var opened bool
	for i := 0; i < 4; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !opened {
		return "", nil, nil, errClipboardBusy
	}
	defer procCloseClipboard.Call()

	files = clipFiles()

	if b := clipBytes(cfUnicodeText); len(b) >= 2 {
		text = windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), len(b)/2))
	}

	name, _ := windows.UTF16PtrFromString("PNG")
	if fid, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(name))); fid != 0 {
		png = clipBytes(fid)
	}
	if len(png) == 0 {
		for _, cf := range []uintptr{cfDIBV5, cfDIB} { // V5 信息更全，优先
			if b := clipBytes(cf); len(b) > 0 {
				if p := dibToPNG(b); p != nil {
					png = p
					break
				}
			}
		}
	}
	return text, png, files, nil
}

// maskShift 求位掩码的右移位数，并校验它是连续位段（非连续掩码返回 false）。
func maskShift(m uint32) (int, bool) {
	if m == 0 {
		return 0, false
	}
	s := 0
	for m&1 == 0 {
		m >>= 1
		s++
	}
	for m&1 == 1 {
		m >>= 1
	}
	return s, m == 0
}

// dibToPNG 把 CF_DIB/CF_DIBV5 的设备无关位图转成 PNG。只处理 24/32bpp 无压缩
// （BI_RGB）与 BI_BITFIELDS 掩码（GDI+/截图工具的常见形态）；32bpp 没有显式
// alpha 掩码时按不透明处理——GDI DIB 的第四字节常是 0x00 未用，照搬会把整张
// 图变全透明。其余形态（调色板、RLE 压缩等）返回 nil，让「粘贴」退回文本。
func dibToPNG(dib []byte) []byte {
	le := binary.LittleEndian
	if len(dib) < 40 {
		return nil
	}
	sz := le.Uint32(dib[0:4])
	switch sz {
	case 40, 52, 56, 108, 124: // BITMAPINFOHEADER 及其扩展、BITMAPV5HEADER
	default:
		return nil
	}
	w := int(int32(le.Uint32(dib[4:8])))
	hv := int(int32(le.Uint32(dib[8:12])))
	topDown := hv < 0
	if topDown {
		hv = -hv
	}
	if w <= 0 || hv <= 0 || w > 20000 || hv > 20000 {
		return nil
	}
	bpp := int(le.Uint16(dib[14:16]))
	comp := le.Uint32(dib[16:20])
	if bpp != 24 && bpp != 32 {
		return nil
	}
	// 像素区起点：头之后（BI_BITFIELDS 且是 40 字节老头时，掩码跟在头后面）
	off := int(sz)
	var rm, gm, bm, am uint32
	switch comp {
	case 0: // BI_RGB：BGR(X) 固定排布；32bpp 的第四字节按未用处理（不透明）
		rm, gm, bm = 0xFF0000, 0xFF00, 0xFF
		am = 0
	case 3: // BI_BITFIELDS：老头的 3/4 个掩码在头后，V5 头的掩码在头内
		if sz == 40 {
			if len(dib) < off+12 {
				return nil
			}
			rm, gm, bm = le.Uint32(dib[off:off+4]), le.Uint32(dib[off+4:off+8]), le.Uint32(dib[off+8:off+12])
			if len(dib) >= off+16 {
				am = le.Uint32(dib[off+12 : off+16])
			}
			off += 12
		} else {
			rm, gm, bm, am = le.Uint32(dib[40:44]), le.Uint32(dib[44:48]), le.Uint32(dib[48:52]), le.Uint32(dib[52:56])
		}
	default:
		return nil
	}
	rs, ok1 := maskShift(rm)
	gs, ok2 := maskShift(gm)
	bs, ok3 := maskShift(bm)
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	as, ok4 := maskShift(am)
	if am == 0 {
		ok4 = true // 没有 alpha 掩码：不透明
	}
	if !ok4 {
		return nil
	}
	stride := (w*bpp/8 + 3) &^ 3
	if len(dib) < off+stride*hv {
		return nil
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, hv))
	for y := 0; y < hv; y++ {
		src := y
		if !topDown { // 默认存储自下而上，翻成自上而下
			src = hv - 1 - y
		}
		row := dib[off+src*stride:]
		for x := 0; x < w; x++ {
			var r, g, b, a uint8 = 0, 0, 0, 255
			if bpp == 24 {
				p := row[x*3:]
				b, g, r = p[0], p[1], p[2]
			} else {
				v := le.Uint32(row[x*4:])
				r = uint8((v & rm) >> rs)
				g = uint8((v & gm) >> gs)
				b = uint8((v & bm) >> bs)
				if am != 0 {
					a = uint8((v & am) >> as)
				}
			}
			q := img.Pix[y*img.Stride+x*4:]
			q[0], q[1], q[2], q[3] = r, g, b, a
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
