# exe 名切换：坚祥数字助理GO → AI数字助理（一周后操作清单）

时间：2026-10-30 立项（本轮换 logo + 双名机制已落地）
节奏：**约一周内发行的版本 exe 文件名保持「坚祥数字助理GO.exe」；一周后发新版
换用「AI数字助理.exe」**。本文档是切换日的操作清单——改完下面几处、出包、
发布，即完成全量迁移。

## 背景：双名兼容机制（已就位，切换日无需再做）

- **热更新（engine 原地交换）天然支持任意 exe 名**：update_blocks.go 按
  `os.Executable()` 自身真实路径做两次原子改名（旧→.old、新→本机当前名），
  zip 里的文件名无关紧要。老用户热更新照常，只是文件名保留各自机器上的名字。
- **单实例互斥体名不变**（desktop_windows.go 的 `Local\JX-Digital-Assistant-GO-SingleInstance`
  刻意保留旧串）：切换期内新旧名 exe 互相认识，不会双开。
- **换名版安装包自带迁移清理**（打包/安装包.iss 的 `#if MyAppExeName != "坚祥数字助理GO.exe"`
  段）：装「AI数字助理.exe」时自动删旧名 exe / 旧开始菜单组 / 旧桌面快捷方式，
  TaskKillApp 同时腾新旧名进程。
- **重启/开发脚本双名探测**：restart-assistant.ps1（新名优先探测）、
  打包/开发桌面版.ps1（新旧名进程都关）。

## 切换日操作清单（一周后）

1. **打包/打包安装包.ps1**：`$ExeBaseName` 默认值 `'坚祥数字助理GO'` → `'AI数字助理'`
   （发布静默更新.ps1 的 `$ExeBaseName` 同步改，保持一致）。
2. **go-winres.json**：`OriginalFileName` 两处 `"坚祥数字助理GO.exe"` →
   `"AI数字助理.exe"`（ProductName/FileDescription 本轮已是新名，不用动），
   然后重新生成 syso：
   `go run github.com/tc-hib/go-winres@latest make --in go-winres.json --out rsrc`
   （需 GOPROXY=https://goproxy.cn,direct；--out 是**前缀**，不是文件名）。
3. **打包/使用说明.md**：全文「坚祥数字助理GO」→「AI数字助理」。
4. **打包/开发桌面版.ps1**：输出文件名改为 `AI数字助理.exe`（两处）。
5. `go build` + 跑一遍打包脚本出安装包，装一台旧版机器验证迁移：旧 exe 消失、
   新 exe + 新快捷方式就位、数据（data/）保留。
6. 发布：安装包 + 静默更新 engine 同版本一起发（engine 让老用户先升内容，
   想换文件名的用户装一次安装包即完成）。

## 本轮（2026-10-30）已落地的改动

- 全部 logo 换新图（品牌素材/app logo 图标.png）：web/logo.png（原图）、
  web/favicon.png（128×128）、打包/app.ico（16–256 七档，make_icon.py 同步
  改名）、rsrc_windows_amd64.syso 重新生成（exe 图标=窗口/任务栏图标）。
- exe 版本资源（go-winres.json）：ProductName/FileDescription/manifest =
  「AI数字助理 / AI Digital Assistant」；窗口标题（desktop_windows.go）、
  启动日志（main.go）同步。OriginalFileName 本周仍写旧名（与实际文件名一致）。
- 界面品牌名**保留**「坚祥数字助理」（关于页去「（GO版）」后缀，
  app-core.js applyBrandTexts 同步）；左栏「跟长期助理对话」不再加粗。
- 界面其余文字（欢迎语/通知标题/各模式提示词里的「坚祥数字助理」）不动：
  界面名走服务端品牌配置（brand_sync），提示词属软件内部。

## 验证记录（2026-10-30）

| 项 | 结果 |
|---|---|
| 新 exe 版本资源（UTF-16 串搜二进制） | AI数字助理 / AI Digital Assistant ✓，OriginalFileName=坚祥数字助理GO.exe（本周口径）✓ |
| 新 ico 嵌入 | 7/7 尺寸条目全部命中新 exe，与旧 exe 0 重叠 ✓ |
| 界面（-webdir 实测） | 关于页「坚祥数字助理」✓；首页/侧栏/关于 logo 全部 1024×1024 新图 ✓；侧栏「跟长期助理对话」无加粗 ✓ |
| go build | 通过（console 与 GUI 两种形态） |
