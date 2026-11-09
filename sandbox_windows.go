//go:build windows

package main

// Windows 沙箱后端：移植 DSH 官方 sandbox-windows-acl 的思路
// （受限令牌 + ACL 授权，非 1:1 代码）。
//
// 机制（对齐官方 AclSandbox）：
//   1. 从「路径」派生出两个合成 SID（workspace SID / temp SID）——
//      同一路径永远得到同一 SID，作为长期授权的稳定身份；
//   2. 在工作区目录和本次运行的私有临时目录上写入允许该 SID 写入的
//      ACE（含继承，子文件/子目录同样可写）；工作区 ACE 长期保留
//      （跨次复用，同官方 standing grant），临时目录随目录删除消失；
//   3. 用 CreateRestrictedToken 造出带这两个「仅限写检查」 restricting
//      SID 的受限令牌（并剥离全部特权），再用它启动子进程；
//   4. 之后子进程的每一次写访问都同时过「正常 DACL 检查」和「仅看
//      restricting SID 的检查」：除工作区/临时目录外，任何写都被内核
//      拒绝。读不受限（官方同款语义，WRITE_RESTRICTED）。
//   5. fail closed：任何一步 Win32 失败，子进程宁可不启动。
//
// 已知边界（DSH 官方同款，自报 partial）：Everyone 可写的位置与 NTFS 硬
// 链接可以绕路写（硬链接在受限进程打开 ws 内链接名时，NTFS 懒传播会把
// 工作区根的继承 ACE 应用到共享 SD，外部别名随之可写——曾以首授后全树
// 扫描摘 ACE 收尾，因懒传播在下次打开时重新应用而无效，已移除）；删除
// 走父目录 FILE_DELETE_CHILD（清柜权）路线时不触发写受限双检——
// %TEMP%/Documents/Desktop 等用户 FullControl 位置可被非 -Force 删除
// （判定实验固化在 sbx_delete_matrix_test.go）；只限写，不限读与联网。
//
// 历史注记：曾在官方之上叠加「密级关卡」（令牌降 Low 完整性 + 工作区打
// Low 强制标签）封堵上述删除路线，因常驻 Low 标签污染真实工作区（把程序
// 自身目录、数据文件一并降级，低级别进程链连锁）于 2026-09 整体撤除，
// 回到官方同款语义。历史残留标签由启动时 restoreStaleIntegrityLabels
// 自动还原，手动清理走 sandbox acl-audit --revoke。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	aclRevision      = 2
	accessAllowedACE = 0
	// ACE 继承标志
	objectInherit    = 0x01
	containerInherit = 0x02
	inheritedACE     = 0x10
	// CreateRestrictedToken Flags（官方 win32-abi.ts 同值 0x1|0x4|0x8）：
	// 剥全部特权 + LUA（标准用户态）+ 写受限模式
	restrictedTokenFlags = 0x1 | 0x4 | 0x8
	// 令牌组属性：登录会话 SID
	seGroupLogonId = 0xC0000000
	// 文件完全访问（官方默认 DACL 补授用 FILE_ALL_ACCESS）
	fileAllAccess = 0x1F01FF
	// Get/SetNamedSecurityInfo 的对象类型与信息位
	seFileObject     = 1
	daclSecurityInfo = 0x4
	// DACL 保护位：设置的 DACL 即最终 DACL，不再合并继承
	protectedDaclSecurityInfo = 0x80000000
	// 写授权掩码：官方 GRANT_MASK —— 写/追加/写属性/写EA/DELETE/DELETE_CHILD/
	// 同步，刻意剔除 READ_CONTROL（受限子进程不得改 DACL/夺所有权以逃逸）
	fileGenericWrite = 0x112156
	// 强制完整性标签（残留清理仍读写）：SYSTEM_MANDATORY_LABEL_ACE_TYPE：
	// SACL 里的密级标签 ACE；NO_WRITE_UP：低密进程不得写/删本对象；
	// LABEL_SECURITY_INFORMATION（winnt.h，x/sys 同值 0x10）：Get/
	// SetNamedSecurityInfo 读写强制密级标签专用信息位（独立于
	// SACL_SECURITY_INFORMATION，不要求 SeSecurityPrivilege；0x80 是
	// PROCESS_TRUST_LABEL，写入要特权，勿混）。
	systemMandatoryLabelAce   = 0x11
	mandatoryNoWriteUp        = 0x1
	labelSecurityInfo         = 0x10
	fileAttributeReparsePoint = 0x400
	// SetErrorMode 标志（子进程继承）：抑制关键错误弹窗/崩溃框/文件打开错误框。
	// 缺了它，DLL 初始化失败（0xC0000142）等硬错误会挂起等待 UI，命令看起来
	// 永远不返回（官方 win32-abi.ts 加 SetErrorMode 绑定同款用途）。
	semFailCriticalErrors = 0x0001
	semNoGPFaultErrorBox  = 0x0002
	semNoOpenFileErrorBox = 0x8000
)

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
	procGetNamedSecurityInfoW = advapi32.NewProc("GetNamedSecurityInfoW")
	procSetNamedSecurityInfoW = advapi32.NewProc("SetNamedSecurityInfoW")
	procGetTokenInformation   = advapi32.NewProc("GetTokenInformation")
	procSetTokenInformation   = advapi32.NewProc("SetTokenInformation")
	procSetErrorMode          = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetErrorMode")
)

// sidForPath 从路径派生稳定合成 SID：S-1-80-<h0>-<h1>-<h2>-<h3>。
// 权威值 80 未被 Windows 语义占用，可安全用作私有授权身份。
// windows.SID 不开放构造，这里手工拼 SID 二进制（rev=1, count=4）后转型。
func sidForPath(p string) *windows.SID {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimRight(p, "\\/") + "\\")))
	b := make([]byte, 8+4*4)
	b[0] = 1 // SID_REVISION
	b[1] = 4 // SubAuthorityCount
	// IdentifierAuthority：6 字节大端，值 80
	b[2], b[3], b[4], b[5], b[6], b[7] = 0, 0, 0, 0, 0, 80
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint32(b[8+i*4:], binary.BigEndian.Uint32(sum[i*4:i*4+4]))
	}
	return (*windows.SID)(unsafe.Pointer(&b[0]))
}

// sidBytes 把 *windows.SID 序列化为自包含字节（用于拼 ACE）。
func sidBytes(sid *windows.SID) ([]byte, error) {
	n := windows.GetLengthSid(sid)
	if n == 0 || n > 68 {
		return nil, fmt.Errorf("非法 SID 长度 %d", n)
	}
	b := make([]byte, n)
	if err := windows.CopySid(uint32(n), (*windows.SID)(unsafe.Pointer(&b[0])), sid); err != nil {
		return nil, err
	}
	return b, nil
}

// lowIntegritySid 构造 Low 完整性 SID：S-1-16-4096
// （SECURITY_MANDATORY_LABEL_AUTHORITY + SECURITY_MANDATORY_LOW_RID）。
// 残留审计用（hasLowLabel）；密级机制已撤除，不再主动打标。
func lowIntegritySid() *windows.SID {
	b := make([]byte, 12) // rev(1)+count(1)+auth(6)+subauth(4)
	b[0] = 1
	b[1] = 1 // SubAuthorityCount = 1
	b[7] = 16
	binary.LittleEndian.PutUint32(b[8:], 0x1000) // RID = 4096 = Low
	return (*windows.SID)(unsafe.Pointer(&b[0]))
}

// mediumIntegritySid 构造 Medium 完整性 SID：S-1-16-8192（残留标签还原用）。
func mediumIntegritySid() *windows.SID {
	b := make([]byte, 12)
	b[0] = 1
	b[1] = 1
	b[7] = 16
	binary.LittleEndian.PutUint32(b[8:], 0x2000) // RID = 8192 = Medium
	return (*windows.SID)(unsafe.Pointer(&b[0]))
}

// stripLowLabel 把对象强制标签改回 Medium（S-1-16-8192）：历史密级沙箱
// 的残留清理用（auditSandboxGrants / restoreStaleIntegrityLabels）。
func stripLowLabel(path string) error {
	return setLabelAce(path, mediumIntegritySid(), false, false)
}

// setLabelAce 写对象的强制标签 ACE（labelSid + NO_WRITE_UP）。checkIdem 时
// 已有同 SID 标签即跳过。失败返回错误。
func setLabelAce(path string, labelSid *windows.SID, inherit, checkIdem bool) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sideb, err := sidBytes(labelSid)
	if err != nil {
		return err
	}
	if checkIdem {
		var sd *windows.SECURITY_DESCRIPTOR
		var sacl *windows.ACL
		r1, _, _ := procGetNamedSecurityInfoW.Call(
			uintptr(unsafe.Pointer(p)), seFileObject, labelSecurityInfo,
			0, 0, 0, uintptr(unsafe.Pointer(&sacl)), uintptr(unsafe.Pointer(&sd)))
		if r1 == 0 {
			defer windows.LocalFree(windows.Handle(unsafe.Pointer(sd)))
			if sb, err := aclBytes(sacl); err == nil {
				for i := 8; i+8 <= len(sb); {
					size := int(binary.LittleEndian.Uint16(sb[i+2:]))
					if size < 8 || i+size > len(sb) {
						break
					}
					if sb[i] == systemMandatoryLabelAce &&
						binary.LittleEndian.Uint32(sb[i+4:]) == mandatoryNoWriteUp &&
						string(sb[i+8:i+size]) == string(sideb) {
						return nil
					}
					i += size
				}
			}
		}
	}
	// 新标签 ACE：type(1) flags(1) size(2) mask(4) sid（与 allow ACE 同布局）
	ace := make([]byte, 8+len(sideb))
	ace[0] = systemMandatoryLabelAce
	if inherit {
		ace[1] = objectInherit | containerInherit
	}
	binary.LittleEndian.PutUint16(ace[2:], uint16(len(ace)))
	binary.LittleEndian.PutUint32(ace[4:], mandatoryNoWriteUp)
	copy(ace[8:], sideb)
	// 标签 ACE 单独成 ACL（LABEL_SECURITY_INFORMATION 只动强制标签部分）
	nacl := make([]byte, 8+len(ace))
	nacl[0] = aclRevision
	binary.LittleEndian.PutUint16(nacl[2:], uint16(len(nacl)))
	binary.LittleEndian.PutUint16(nacl[4:], 1)
	copy(nacl[8:], ace)
	if pad := (4 - len(nacl)%4) % 4; pad > 0 {
		nacl = append(nacl, make([]byte, pad)...)
		binary.LittleEndian.PutUint16(nacl[2:], uint16(len(nacl)))
	}
	nptr := (*windows.ACL)(unsafe.Pointer(&nacl[0]))
	// SetNamedSecurityInfo 直接返回 Win32 错误码（非 BOOL），errno 无意义。
	setLabel := func() uint32 {
		r1, _, _ := procSetNamedSecurityInfoW.Call(
			uintptr(unsafe.Pointer(p)), seFileObject, labelSecurityInfo,
			0, 0, 0, uintptr(unsafe.Pointer(nptr)))
		return uint32(r1)
	}
	r1 := setLabel()
	if r1 != 0 {
		// 真实工作区兼容：写强制标签（LABEL_SECURITY_INFORMATION）要求
		// WRITE_OWNER，而 Modify 档 ACL 与所有者隐式权限（READ_CONTROL|
		// WRITE_DAC）都不含它——曾导致真实环境每次沙箱启动都失败在
		// 「工作区打密级标签」（%TEMP% 的 FullControl 档测试目录测不出）。
		// 用 WRITE_DAC 临时并入一条当前用户的 WRITE_OWNER ACE 后重试，
		// 贴完还原。
		if added, derr := grantWriteOwnerAce(path, false); derr == nil && added {
			r1 = setLabel()
			_ = stripWriteOwnerAce(path)
		}
	}
	if r1 != 0 {
		return fmt.Errorf("SetNamedSecurityInfo(标签 %s) 失败: Win32 %d", path, r1)
	}
	return nil
}

// writeOwnerRight = WRITE_OWNER：写强制密级标签所需的访问权。
const writeOwnerRight = 0x00080000

// currentUserSid 取当前进程令牌的用户 SID。
func currentUserSid() (*windows.SID, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return nil, err
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	n := windows.GetLengthSid(tu.User.Sid)
	b := make([]byte, n)
	if err := windows.CopySid(n, (*windows.SID)(unsafe.Pointer(&b[0])), tu.User.Sid); err != nil {
		return nil, err
	}
	return (*windows.SID)(unsafe.Pointer(&b[0])), nil
}

// grantWriteOwnerAce 给当前用户并入一条仅含 WRITE_OWNER 的 allow ACE。
// inherit=true 时带 (OI)(CI)：随继承铺到子树，用作密级标签还原的「WO 引导」
// （见 restoreStaleIntegrityLabels）；false 只授权本对象（setLabelAce 的
// 补授权重试用）。用户已有覆盖 WRITE_OWNER 位的 allow ACE 则不动（如
// %TEMP% 的 FullControl 档）。返回是否新增。不带 PROTECTED 位——保持目录
// 继承结构不变。剥离配对 stripWriteOwnerAce（摘掉所有掩码恰为 WRITE_OWNER
// 的当前用户 allow ACE，两种继承形态一并清）。
func grantWriteOwnerAce(path string, inherit bool) (bool, error) {
	us, err := currentUserSid()
	if err != nil {
		return false, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	var sd *windows.SECURITY_DESCRIPTOR
	var dacl *windows.ACL
	r1, _, _ := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(p)), seFileObject, daclSecurityInfo,
		0, 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd)))
	if r1 != 0 {
		return false, fmt.Errorf("GetNamedSecurityInfo(%s) 失败: Win32 %d", path, r1)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(sd)))
	sb, err := aclBytes(dacl)
	if err != nil {
		return false, err
	}
	sideb, err := sidBytes(us)
	if err != nil {
		return false, err
	}
	for i := 8; i+8 <= len(sb); {
		size := int(binary.LittleEndian.Uint16(sb[i+2:]))
		if size < 8 || i+size > len(sb) {
			break
		}
		if sb[i] == accessAllowedACE &&
			binary.LittleEndian.Uint32(sb[i+4:])&writeOwnerRight == writeOwnerRight &&
			string(sb[i+8:i+size]) == string(sideb) {
			return false, nil // 已持有
		}
		i += size
	}
	ace := make([]byte, 8+len(sideb))
	ace[0] = accessAllowedACE
	if inherit {
		ace[1] = objectInherit | containerInherit
	}
	binary.LittleEndian.PutUint16(ace[2:], uint16(len(ace)))
	binary.LittleEndian.PutUint32(ace[4:], writeOwnerRight)
	copy(ace[8:], sideb)
	// 插到第一个继承 ACE 之前（显式在前），否则末尾——与 grantWriteAce 同款
	pos := len(sb)
	for i := 8; i+8 <= len(sb); {
		size := int(binary.LittleEndian.Uint16(sb[i+2:]))
		if size < 8 || i+size > len(sb) {
			pos = len(sb)
			break
		}
		if sb[i+1]&inheritedACE != 0 {
			pos = i
			break
		}
		i += size
	}
	count := int(binary.LittleEndian.Uint16(sb[4:]))
	nacl := make([]byte, 0, len(sb)+len(ace))
	nacl = append(nacl, sb[:8]...)
	nacl = append(nacl, sb[8:pos]...)
	nacl = append(nacl, ace...)
	nacl = append(nacl, sb[pos:]...)
	if pad := (4 - len(nacl)%4) % 4; pad > 0 {
		nacl = append(nacl, make([]byte, pad)...)
	}
	binary.LittleEndian.PutUint16(nacl[2:], uint16(len(nacl)))
	binary.LittleEndian.PutUint16(nacl[4:], uint16(count+1))
	nptr := (*windows.ACL)(unsafe.Pointer(&nacl[0]))
	r1, _, _ = procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(p)), seFileObject, daclSecurityInfo,
		0, 0, uintptr(unsafe.Pointer(nptr)), 0)
	if r1 != 0 {
		return false, fmt.Errorf("并入 WRITE_OWNER(%s) 失败: Win32 %d", path, r1)
	}
	return true, nil
}

// stripWriteOwnerAce 摘掉 grantWriteOwnerAce 并入的临时 ACE（掩码恰为
// WRITE_OWNER 的当前用户 allow ACE；用户原有更大权限的 ACE 不动）。
// 找不到即视为已还原。
func stripWriteOwnerAce(path string) error {
	us, err := currentUserSid()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var sd *windows.SECURITY_DESCRIPTOR
	var dacl *windows.ACL
	r1, _, _ := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(p)), seFileObject, daclSecurityInfo,
		0, 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd)))
	if r1 != 0 {
		return fmt.Errorf("GetNamedSecurityInfo(%s) 失败: Win32 %d", path, r1)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(sd)))
	sb, err := aclBytes(dacl)
	if err != nil {
		return err
	}
	sideb, err := sidBytes(us)
	if err != nil {
		return err
	}
	out := append([]byte(nil), sb[:8]...)
	removed := 0
	for i := 8; i+8 <= len(sb); {
		size := int(binary.LittleEndian.Uint16(sb[i+2:]))
		if size < 8 || i+size > len(sb) {
			break
		}
		if sb[i] == accessAllowedACE &&
			binary.LittleEndian.Uint32(sb[i+4:]) == writeOwnerRight &&
			string(sb[i+8:i+size]) == string(sideb) {
			removed++
		} else {
			out = append(out, sb[i:i+size]...)
		}
		i += size
	}
	if removed == 0 {
		return nil
	}
	if pad := (4 - len(out)%4) % 4; pad > 0 {
		out = append(out, make([]byte, pad)...)
	}
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	binary.LittleEndian.PutUint16(out[4:], uint16(int(binary.LittleEndian.Uint16(sb[4:]))-removed))
	nptr := (*windows.ACL)(unsafe.Pointer(&out[0]))
	r1, _, _ = procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(p)), seFileObject, daclSecurityInfo,
		0, 0, uintptr(unsafe.Pointer(nptr)), 0)
	if r1 != 0 {
		return fmt.Errorf("摘除 WRITE_OWNER(%s) 失败: Win32 %d", path, r1)
	}
	return nil
}

// stripWriteAce（已移除）：曾配 fixupMultiLinked 摘多链接文件的 wsSid
// ACE；随硬链接收尾扫描一并删除（懒传播使其无效，见文件头「已知边界」）。

// ---------- Job Object（官方 createKillOnCloseJob 同款） ----------

type joBasicLimitInfo struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type joExtendedLimitInfo struct {
	Basic               joBasicLimitInfo
	ReadOperationCount  int64
	WriteOperationCount int64
	OtherOperationCount int64
	ReadTransferCount   int64
	WriteTransferCount  int64
	OtherTransferCount  int64
	ProcessMemoryLimit  uintptr
	JobMemoryLimit      uintptr
	PeakProcessMemory   uintptr
	PeakJobMemory       uintptr
}

// createKillOnCloseJob 建 kill-on-close Job：句柄关闭即终止整棵进程树。
// 这是生命周期兜底（超时/取消 TerminateJobObject 整树杀；主进程崩溃时
// 句柄被系统关闭 → 残留孙进程自动清场），不涉文件权限。
func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var info joExtendedLimitInfo
	info.Basic.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// grantWriteAce 把「允许 writeSID 写入」的 ACE 合入目录 DACL（幂等）。
// 任何失败都返回错误——调用方必须 fail closed。
func grantWriteAce(dir string, writeSID *windows.SID) error {
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	var sd *windows.SECURITY_DESCRIPTOR
	var dacl *windows.ACL
	r1, _, _ := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(dirPtr)), seFileObject, daclSecurityInfo,
		0, 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd)))
	if r1 != 0 {
		return fmt.Errorf("GetNamedSecurityInfo(%s) 失败: %d", dir, r1)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(sd)))

	sb, err := aclBytes(dacl)
	if err != nil {
		return err
	}
	sideb, err := sidBytes(writeSID)
	if err != nil {
		return err
	}
	// 已存在同 SID 的 allow ACE 则幂等返回（ACE: type0 flags mask sid）
	for i := 0; i+8+len(sideb) <= len(sb); {
		size := int(binary.LittleEndian.Uint16(sb[i+2:]))
		if size < 8 || i+size > len(sb) {
			break
		}
		if sb[i] == accessAllowedACE && binary.LittleEndian.Uint32(sb[i+4:]) == fileGenericWrite &&
			string(sb[i+8:i+size]) == string(sideb) {
			return nil
		}
		i += size
	}
	// 新 ACE：type(1) flags(1) size(2) mask(4) sid
	ace := make([]byte, 8+len(sideb))
	ace[0] = accessAllowedACE
	ace[1] = objectInherit | containerInherit
	binary.LittleEndian.PutUint16(ace[2:], uint16(len(ace)))
	binary.LittleEndian.PutUint32(ace[4:], fileGenericWrite)
	copy(ace[8:], sideb)

	// 插入位置：第一个继承 ACE 之前（保持显式 ACE 在前），否则末尾
	pos := len(sb)
	for i := 8; i+8 <= len(sb); {
		size := int(binary.LittleEndian.Uint16(sb[i+2:]))
		if size < 8 || i+size > len(sb) {
			pos = len(sb)
			break
		}
		if sb[i+1]&inheritedACE != 0 {
			pos = i
			break
		}
		i += size
	}
	count := int(binary.LittleEndian.Uint16(sb[4:]))
	nacl := make([]byte, 8+len(sb)-8+len(ace))
	nacl[0], nacl[1] = aclRevision, 0
	binary.LittleEndian.PutUint16(nacl[2:], uint16(len(nacl)))
	binary.LittleEndian.PutUint16(nacl[4:], uint16(count+1))
	copy(nacl[8:], sb[8:pos])
	copy(nacl[8+pos-8:], ace)
	copy(nacl[8+pos-8+len(ace):], sb[pos:])
	// 对齐到 DWORD
	if pad := (4 - len(nacl)%4) % 4; pad > 0 {
		nacl = append(nacl, make([]byte, pad)...)
		binary.LittleEndian.PutUint16(nacl[2:], uint16(len(nacl)))
	}

	nptr := (*windows.ACL)(unsafe.Pointer(&nacl[0]))
	r1, _, _ = procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(dirPtr)), seFileObject,
		daclSecurityInfo|protectedDaclSecurityInfo,
		0, 0, uintptr(unsafe.Pointer(nptr)), 0)
	if r1 != 0 {
		return fmt.Errorf("SetNamedSecurityInfo(%s) 失败: %d", dir, r1)
	}
	return nil
}

// aclHeader 读 ACL 头部 8 字节（rev, sbz1, size u16, count u16, sbz2）。
// x/sys 的 windows.ACL 字段未导出，只能按内存布局读。
func aclHeader(acl *windows.ACL) *[8]byte { return (*[8]byte)(unsafe.Pointer(acl)) }

// aclBytes 把 *windows.ACL 指向的内存读成字节切片。
func aclBytes(acl *windows.ACL) ([]byte, error) {
	if acl == nil {
		b := make([]byte, 8)
		b[0] = aclRevision
		binary.LittleEndian.PutUint16(b[2:], 8)
		return b, nil
	}
	size := binary.LittleEndian.Uint16(aclHeader(acl)[2:4])
	if size < 8 {
		b := make([]byte, 8)
		b[0] = aclRevision
		binary.LittleEndian.PutUint16(b[2:], 8)
		return b, nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(acl)), size), nil
}

// confinedChild 是一个已启动的受限子进程（前台与后台作业共用）。
type confinedChild struct {
	proc, thread windows.Handle
	job          windows.Handle // kill-on-close Job：整树生命周期兜底
	stdoutR      *os.File
	stderrR      *os.File
	tempDir      string
}

// kill 终止子进程（job_kill / 超时用）。
func (c *confinedChild) kill() {
	if c.job != 0 {
		windows.TerminateJobObject(c.job, 1) // 整树终止（含孙进程）
		return
	}
	windows.TerminateProcess(c.proc, 1)
}

// cleanup 释放句柄并删除私有临时目录（授权 ACE 随目录消失）。
// 关闭 Job 句柄即触发 kill-on-close：子进程派生的残留孙进程一并清场。
func (c *confinedChild) cleanup() {
	windows.CloseHandle(c.proc)
	windows.CloseHandle(c.thread)
	if c.job != 0 {
		windows.CloseHandle(c.job)
	}
	c.stdoutR.Close()
	c.stderrR.Close()
	os.RemoveAll(c.tempDir)
}

// startConfinedWindows 用受限令牌在沙箱内启动命令（官方 AclSandbox 思路）。
// 任何准备步骤失败都返回错误且绝不以无沙箱状态放行子进程（fail closed）。
func startConfinedWindows(r sandboxRun) (*confinedChild, error) {
	// 嵌套沙箱 fail fast：当前进程若已运行在沙箱内（如在沙箱里跑 go test
	// 再起沙箱），写受限令牌拿不到对目标目录的 WRITE_DAC（pass-2 拒），
	// 授权必然失败——与其报莫名的「授权工作区失败」不如直接说清。
	if v := os.Getenv("JX_SANDBOX"); v != "" {
		return nil, fmt.Errorf("sandbox unavailable: 不支持嵌套沙箱（本进程已运行在 %s 档沙箱内，属预期限制）", v)
	}
	// 1) 私有临时目录（本次运行独占，官方为每会话；此处每运行一次，等价更强）
	tempDir, err := os.MkdirTemp("", "jx-sbx-")
	if err != nil {
		return nil, fmt.Errorf("sandbox unavailable: 创建私有临时目录失败: %w", err)
	}

	// 2) 写授权：工作区（长期 ACE，跨次复用）+ 私有临时目录（随目录消失）
	wsSid := sidForPath(r.Workspace)
	tempSid := sidForPath(tempDir)
	if err := grantWriteAce(r.Workspace, wsSid); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: 授权工作区失败: %w", err)
	}
	if err := grantWriteAce(tempDir, tempSid); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: 授权临时目录失败: %w", err)
	}
	// 2b) （已移除）曾在此对工作区做硬链接共享 SD 收尾扫描（fixupMultiLinked
	// 摘 ACE）——NTFS 懒传播会在下次打开时把继承 ACE 重新应用到共享 SD，
	// 扫描不构成有效防线，徒增每工作区一次的全树遍历，已删（见文件头
	// 「已知边界」）。

	// 3) 受限令牌（官方 token.ts 配方）：
	//   OpenProcessToken 要含 TOKEN_ASSIGN_PRIMARY —— CreateRestrictedToken
	//   返回句柄继承源句柄的访问权，缺了它 CreateProcessAsUser 直接拒绝访问。
	//   restricting 列表按档位：
	//     read-only:       [logon SID, Everyone]
	//     workspace-write: [logon SID, Everyone, 工作区SID, 临时目录SID]
	//   logon SID 保住 WinSta0/桌面等每登录对象（缺了子进程 0xC0000142 崩）；
	//   Everyone 保住系统目录的读/执行；写只认我们授了 ACE 的合成 SID，
	//   Everyone 可写的位置就是官方文档承认的 partial 缺口。
	//   INTERACTIVE/LOCAL 刻意不进列表（与 DSH 终版配方一致）：C:\Users\Public
	//   默认授 INTERACTIVE 写权，放行即逃逸——DSH 官方修复的同款盲区
	//   （其 runner.spec 'ambient-writable escape regression'），回归测试见
	//   sbx_adversarial_test.go 的 Public 用例。
	worldSid, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: 构造 Everyone SID 失败: %w", err)
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|
			windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_ADJUST_SESSIONID, &tok); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: 打开进程令牌失败: %w", err)
	}
	defer tok.Close()
	logonSid, err := tokenLogonSid(tok)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: %w", err)
	}
	restricting := []windows.SIDAndAttributes{
		{Sid: logonSid, Attributes: 0},
		{Sid: worldSid, Attributes: 0},
	}
	if r.Policy == permWorkspace {
		restricting = append(restricting,
			windows.SIDAndAttributes{Sid: wsSid, Attributes: 0},
			windows.SIDAndAttributes{Sid: tempSid, Attributes: 0})
	}
	var restricted windows.Token
	r1, _, callErr := procCreateRestrictedToken.Call(
		uintptr(tok), restrictedTokenFlags, 0, 0, 0, 0,
		uintptr(len(restricting)), uintptr(unsafe.Pointer(&restricting[0])), uintptr(unsafe.Pointer(&restricted)))
	if r1 == 0 {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: CreateRestrictedToken 失败: %v", callErr)
	}
	defer restricted.Close()

	// 3b) 令牌默认 DACL 补授（官方 setTokenDefaultDaclGrant）：受限令牌创建的
	// 新对象（管道、临时文件）默认 DACL 里没有任何 restricting SID 的 ACE，
	// pass-2 检查会在创建时就拒绝。给默认 DACL 并入一条该 SID 的完全访问 ACE，
	// 对象创建改由父容器的 DACL 把关（工作区外的文件依旧创建不出来）。
	grantSid := worldSid
	if r.Policy == permWorkspace {
		// 官方 index.ts:294 同款优先级：私有临时 SID 优先——新建对象的
		// 默认 DACL 只带本次运行的临时能力，不携带共享的工作区能力。
		grantSid = tempSid
	}
	if err := grantTokenDefaultDacl(restricted, grantSid); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: %w", err)
	}

	// 3c) （已撤除）曾在此把受限令牌降为 Low 完整性（密级关卡「门」侧），
	// 配套工作区打 Low 标签——常驻标签污染真实工作区，2026-09 整体移除，
	// 见文件头「历史注记」。

	// 4) 解析 shell 与命令行（父进程解析，子进程环境可能不全）
	app, cmdLine, ps51, err := buildShellCmdline(r.Shell, r.Command, r.EncodingMode)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: %w", err)
	}
	appPtr, err := windows.UTF16PtrFromString(app)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("app UTF16: %w", err)
	}
	cmdPtr, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("cmdline UTF16: %w", err)
	}
	dirPtr, err := windows.UTF16PtrFromString(r.Dir)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("dir UTF16: %w", err)
	}
	env := sandboxEnv(false)
	// 白名单会拷来父进程的真实 TEMP/TMP，而环境块去重「先见为准」会静默
	// 丢弃追加的重定向——曾导致私有临时目录重定向从未生效，go build 等
	// 工具链全落在真实 %TEMP% 被拒（会话里模型反复与沙箱打架的主因）。
	// 先摘除再重定向。
	filtered := env[:0]
	for _, kv := range env {
		u := strings.ToUpper(kv)
		if strings.HasPrefix(u, "TEMP=") || strings.HasPrefix(u, "TMP=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	env = filtered
	// 工具链引导：go 的临时目录进私有临时目录，构建缓存放工作区
	// .jxai/gocache（跨命令复用，首次构建时 go 自建；否则要么每次全量
	// 重编、要么写用户目录被拒）。JX_SANDBOX 标记供嵌套检测与模型自识。
	env = append(env,
		"TEMP="+tempDir, "TMP="+tempDir,
		"GOTMPDIR="+tempDir,
		"GOCACHE="+filepath.Join(r.Workspace, ".jxai", "gocache"),
		"JX_SANDBOX="+r.Policy)
	// PS 5.1 编码注入（同 runUnconfinedLocal，仅 powershell 路径；先摘后加纪律见 appendEnvUnique）
	if ps51 {
		env = appendEnvUnique(env, ps51Utf8Env()...)
	}
	envBlock, err := envBlockPtr(env)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("env block: %w", err)
	}

	// 5) stdio：NUL 输入 + 两根继承管道
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		os.RemoveAll(tempDir)
		return nil, err
	}
	if err := windows.SetHandleInformation(windows.Handle(stdoutW.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		stdoutR.Close()
		stderrR.Close()
		os.RemoveAll(tempDir)
		return nil, err
	}
	if err := windows.SetHandleInformation(windows.Handle(stderrW.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		stdoutR.Close()
		stderrR.Close()
		os.RemoveAll(tempDir)
		return nil, err
	}
	nul, err := os.Open(os.DevNull)
	if err != nil {
		stdoutR.Close()
		stderrR.Close()
		os.RemoveAll(tempDir)
		return nil, err
	}
	defer nul.Close()
	// Go 打开的句柄默认不带继承位，显式补上（否则 STARTF_USESTDHANDLES
	// 挂的是无效句柄，靠子进程不读 stdin 才没出事）。
	if err := windows.SetHandleInformation(windows.Handle(nul.Fd()),
		windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		stdoutR.Close()
		stderrR.Close()
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("NUL 句柄标记继承失败: %w", err)
	}
	// SetErrorMode 子进程继承（官方 win32-abi.ts 同款）：硬错误不弹框不挂起。
	procSetErrorMode.Call(uintptr(semFailCriticalErrors | semNoGPFaultErrorBox | semNoOpenFileErrorBox))
	si := &windows.StartupInfo{
		StdInput:  windows.Handle(nul.Fd()),
		StdOutput: windows.Handle(stdoutW.Fd()),
		StdErr:    windows.Handle(stderrW.Fd()),
		Flags:     windows.STARTF_USESTDHANDLES, // 必须显式声明使用这三个句柄
	}
	var pi windows.ProcessInformation

	// 5b) kill-on-close Job（官方 createKillOnCloseJob 同款）：进程树生命周期兜底。
	job, err := createKillOnCloseJob()
	if err != nil {
		stdoutR.Close()
		stderrR.Close()
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: 创建 Job 对象失败: %w", err)
	}

	// 6) CreateProcessAsUser 以挂起态启动（官方 spawnJobProcess 同款），
	// 带 CREATE_NO_WINDOW：桌面形态没有控制台可复用，否则 run_command 每执行
	// 一次就在桌面上闪一个控制台窗口（读写句柄照走管道，取输出不受影响）。
	// 分配进 Job 后再恢复初始线程——保证不会出现「已跑起来但不在 Job 里」
	// 的逃逸窗口。所需主令牌特权来自源句柄的 TOKEN_ASSIGN_PRIMARY。
	// 创建标志：默认压掉子进程的控制台窗口（桌面形态自身没有控制台可复用，不压
	// 就是一个个黑窗）；Visible=true 时不压——调用方明确要用户看见这个窗口。
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if !r.Visible {
		flags |= windows.CREATE_NO_WINDOW
	}
	err = windows.CreateProcessAsUser(restricted, appPtr, cmdPtr, nil, nil, true,
		flags, envBlock, dirPtr, si, &pi)
	// 父进程侧的写端立即关闭，避免读不完
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		windows.CloseHandle(job)
		stdoutR.Close()
		stderrR.Close()
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: CreateProcess(受限令牌) 失败: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1) // fail closed：不以失控状态放行
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(job)
		stdoutR.Close()
		stderrR.Close()
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: AssignProcessToJobObject 失败: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateJobObject(job, 1)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(job)
		stdoutR.Close()
		stderrR.Close()
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: ResumeThread 失败: %w", err)
	}
	return &confinedChild{proc: pi.Process, thread: pi.Thread, job: job,
		stdoutR: stdoutR, stderrR: stderrR, tempDir: tempDir}, nil
}

// drain 把两路输出合并写入 w（阻塞到流关闭，适合放 goroutine）。
func (c *confinedChild) drain(w io.Writer) {
	var wg sync.WaitGroup
	wg.Add(2)
	for _, f := range []*os.File{c.stdoutR, c.stderrR} {
		go func(f *os.File) {
			defer wg.Done()
			_, _ = io.Copy(w, f)
		}(f)
	}
	wg.Wait()
}

// wait 阻塞等待子进程退出；timeout 到点或 ctx 取消时终止它。
// timeout<=0 表示不限时（后台作业），终止只经 ctx（job_kill）。
func (c *confinedChild) wait(ctx context.Context, timeout time.Duration) (timedOut bool) {
	waitDone := make(chan struct{}, 1)
	go func() {
		_, _ = windows.WaitForSingleObject(c.proc, windows.INFINITE)
		close(waitDone)
	}()
	var timeoutCh <-chan time.Time // 不限时（timeout<=0）时保持 nil：从 nil 通道接收永久阻塞
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	select {
	case <-waitDone:
	case <-ctx.Done():
		c.kill()
		<-waitDone
		timedOut = true
	case <-timeoutCh:
		c.kill()
		<-waitDone
		timedOut = true
	}
	return timedOut
}

// spawnConfinedWindows 用受限令牌在沙箱内执行一次命令，返回合并输出。
func spawnConfinedWindows(ctx context.Context, r sandboxRun) (string, error) {
	child, err := startConfinedWindows(r)
	if err != nil {
		return "", err
	}
	defer child.cleanup()
	var buf syncBuffer
	go child.drain(&buf)
	timedOut := child.wait(ctx, r.Timeout)
	res := strings.TrimRight(decodeCmdOutput(buf.Bytes()), "\r\n")
	if len(res) > 64*1024 {
		res = res[:64*1024] + "\n[输出过长已截断]"
	}
	if timedOut {
		res += "\n[命令超时或被取消，已终止]"
	}
	if strings.TrimSpace(res) == "" {
		res = "（命令执行完成，无输出）"
	}
	return res, nil
}

// syncBuffer 是并发安全的输出缓冲。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Bytes 取原始输出字节（GBK 等非 UTF-8 控制台输出必须在字节层交给
// decodeCmdOutput 解码；String() 的隐式转换会先把非法字节永久替换成 U+FFFD）
func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Bytes()
}

// buildShellCmdline 解析 shell 名并构造 (appName, commandLine, ps51)。
// ps51 = 解析为 Windows PowerShell 5.1（空 shell 缺省与显式 powershell）：
// 命令前置编码设置（mode 决定 UTF-8 或 GBK 读面），调用方据此注入配套环境
// 变量；pwsh 本就 UTF-8、cmd 不在此路径注入。
func buildShellCmdline(shell, command, encMode string) (string, string, bool, error) {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "cmd":
		comspec := os.Getenv("COMSPEC")
		if comspec == "" {
			comspec = `C:\Windows\system32\cmd.exe`
		}
		return comspec, `"` + comspec + `" /C ` + command, false, nil
	case "", "pwsh", "powershell": // 空 shell 缺省 powershell（曾报「不支持的 shell: 」；空串直接 LookPath 会失败报「找不到 」）
		if shell == "" {
			shell = "powershell"
		}
		exe, err := exec.LookPath(shell)
		if err != nil {
			return "", "", false, fmt.Errorf("找不到 %s", shell)
		}
		ps51 := strings.ToLower(shell) != "pwsh"
		if ps51 {
			command = ps51Prologue(encMode) + command
		}
		return exe, `"` + exe + `" -NoProfile -NonInteractive -Command ` + syscall.EscapeArg(command), ps51, nil
	default:
		return "", "", false, fmt.Errorf("不支持的 shell: %s", shell)
	}
}

// envBlockPtr 构造 CreateProcessAsUser 的环境变量块（UTF-16，双 NUL 结尾）。
func envBlockPtr(env []string) (*uint16, error) {
	blocks := make([]string, 0, len(env))
	seen := map[string]bool{}
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k := strings.ToUpper(kv[:i])
		if seen[k] {
			continue
		}
		seen[k] = true
		blocks = append(blocks, kv)
	}
	// Windows 期望环境块按名排序（不区分大小写）
	sortStrings(blocks)
	s := strings.Join(blocks, "\x00") + "\x00\x00"
	// UTF16PtrFromString 拒绝内部 NUL，环境块必须手工编码 UTF-16
	u := utf16.Encode([]rune(s))
	return &u[0], nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.ToLower(s[j]) < strings.ToLower(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// tokenLogonSid 从令牌组里提取登录会话 SID（S-1-5-5-x-y）。
// WinSta0/桌面等每登录对象的 DACL 授的是它；缺了它受限子进程启动即
// 0xC0000142 崩溃（官方注释同款结论）。
func tokenLogonSid(tok windows.Token) (*windows.SID, error) {
	tg, err := tok.GetTokenGroups()
	if err != nil {
		return nil, fmt.Errorf("GetTokenGroups 失败: %w", err)
	}
	for _, g := range tg.AllGroups() {
		if g.Attributes&seGroupLogonId == seGroupLogonId {
			n := windows.GetLengthSid(g.Sid)
			b := make([]byte, n)
			if err := windows.CopySid(n, (*windows.SID)(unsafe.Pointer(&b[0])), g.Sid); err != nil {
				return nil, err
			}
			return (*windows.SID)(unsafe.Pointer(&b[0])), nil
		}
	}
	return nil, fmt.Errorf("令牌里没有登录会话 SID")
}

// grantTokenDefaultDacl 向受限令牌的默认 DACL 并入一条 grantSid 的完全
// 访问 ACE（官方 setTokenDefaultDaclGrant）。默认 DACL 决定令牌持有者
// 新建对象的初始安全描述符——不补授，受限子进程创建管道/临时文件会在
// 受限检查（pass-2）被拒。任何失败都返回错误（fail closed）。
func grantTokenDefaultDacl(tok windows.Token, grantSid *windows.SID) error {
	dacl, err := tokenDefaultDaclBytes(tok)
	if err != nil {
		return err
	}
	sideb, err := sidBytes(grantSid)
	if err != nil {
		return err
	}
	ace := make([]byte, 8+len(sideb))
	ace[0] = accessAllowedACE
	ace[1] = 0 // 对象 DACL 不需要继承标志
	binary.LittleEndian.PutUint16(ace[2:], uint16(len(ace)))
	binary.LittleEndian.PutUint32(ace[4:], fileAllAccess)
	copy(ace[8:], sideb)
	nacl := mergeAce(dacl, ace)

	// SetTokenInformation(TokenDefaultDacl=6, &TOKEN_DEFAULT_DACL{PACL}, 8)
	info := make([]byte, 8)
	binary.LittleEndian.PutUint64(info, uint64(uintptr(unsafe.Pointer(&nacl[0]))))
	r2, _, callErr := procSetTokenInformation.Call(
		uintptr(tok), 6, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
	if r2 == 0 {
		return fmt.Errorf("SetTokenInformation(TokenDefaultDacl) 失败: %v", callErr)
	}
	return nil
}

// tokenDefaultDaclBytes 读令牌默认 DACL 的字节（缺省时给空 ACL 头）。
func tokenDefaultDaclBytes(tok windows.Token) ([]byte, error) {
	var need uint32
	procGetTokenInformation.Call(uintptr(tok), 6, 0, 0, uintptr(unsafe.Pointer(&need)))
	if need == 0 {
		b := make([]byte, 8)
		b[0] = aclRevision
		binary.LittleEndian.PutUint16(b[2:], 8)
		return b, nil
	}
	buf := make([]byte, need)
	r1, _, callErr := procGetTokenInformation.Call(
		uintptr(tok), 6, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&need)))
	if r1 == 0 {
		return nil, fmt.Errorf("GetTokenInformation(TokenDefaultDacl) 失败: %v", callErr)
	}
	// TOKEN_DEFAULT_DACL 结构 = 一个 PACL 指针
	pacl := *(**windows.ACL)(unsafe.Pointer(&buf[0]))
	if pacl == nil {
		b := make([]byte, 8)
		b[0] = aclRevision
		binary.LittleEndian.PutUint16(b[2:], 8)
		return b, nil
	}
	return aclBytes(pacl)
}

// mergeAce 把一条 ACE 插入 ACL 字节（首个继承 ACE 之前），返回新 ACL。
func mergeAce(dacl []byte, ace []byte) []byte {
	pos := len(dacl)
	for i := 8; i+8 <= len(dacl); {
		size := int(binary.LittleEndian.Uint16(dacl[i+2:]))
		if size < 8 || i+size > len(dacl) {
			pos = len(dacl)
			break
		}
		if dacl[i+1]&inheritedACE != 0 {
			pos = i
			break
		}
		i += size
	}
	count := int(binary.LittleEndian.Uint16(dacl[4:]))
	nacl := make([]byte, 0, len(dacl)+len(ace))
	nacl = append(nacl, dacl[:8]...)
	nacl = append(nacl, dacl[8:pos]...)
	nacl = append(nacl, ace...)
	nacl = append(nacl, dacl[pos:]...)
	if pad := (4 - len(nacl)%4) % 4; pad > 0 {
		nacl = append(nacl, make([]byte, pad)...)
	}
	binary.LittleEndian.PutUint16(nacl[2:], uint16(len(nacl)))
	binary.LittleEndian.PutUint16(nacl[4:], uint16(count+1))
	return nacl
}
