# 安卓壳（android/）

把 Go 引擎变成真正的安卓应用：图标、前台服务保活、通知栏开关、WebView 界面、
内置 Linux 运行时（Termux 快照）与公有目录交付。

## 结构

```
android/
├── app/build.gradle.kts          minSdk 26 / targetSdk 28（W^X 豁免，见下）/
│                                 arm64+x86_64 / useLegacyPackaging=true
├── app/src/main/
│   ├── AndroidManifest.xml       INTERNET / 前台服务(dataSync) / 通知 / 存储（交付用）
│   │                             + 无障碍服务声明（BIND_ACCESSIBILITY_SERVICE）
│   ├── assets/runtime/           runtime.tgz + runtime-MANIFEST.json（构建时拷入，不入库）
│   ├── java/com/jianxiang/assistant/
│   │   ├── MainActivity.kt       通知+存储权限引导 → 拉服务 → 轮询就绪 → WebView
│   │   ├── EngineService.kt      前台服务：拷贝运行时资产、拉起 libjx.so、
│   │   │                         守护重启、孤儿清理、通知栏「停止」、启动设备桥
│   │   ├── DeviceAccessibilityService.kt  无障碍执行体：截屏(takeScreenshot)/
│   │   │                         手势点按滑动/焦点输入/系统键/界面树
│   │   └── DeviceBridge.kt       设备桥：长轮询引擎 /api/device/poll 取命令、
│   │                             执行完 POST /api/device/result 回传
│   ├── res/xml/device_accessibility.xml   无障碍能力声明（读树/手势/截屏）
│   └── res/                      自适应图标（蓝底白气泡对勾）
├── app/src/main/jniLibs/<abi>/libjx.so  ← 构建脚本从 ../ 复制（不入库）
└── runtime/                      快照母本（runtime.tgz 不入库）+ MANIFEST.json
    ├── make-runtime.ps1          设备侧制作路径（arm64 真机上跑，-Serial 指定设备）
    └── pc-build/build-runtime-pc.sh   PC 侧组装（WSL Ubuntu，不依赖设备，首选）
```

## 引擎承载方式

引擎二进制**不入 assets**：预编译为 `libjx.so` 放 `app/src/main/jniLibs/<abi>/`，安装时
系统自动释放到 `nativeLibraryDir`——targetSdk 29+ 禁止执行应用数据目录里的
文件（W^X），`nativeLibraryDir` 是唯一豁免路径。因此 **targetSdk 锚在 28**：
与 Termux 同构的 W^X 豁免，内置运行时（exec 应用私有目录里的 bash/python）
依赖它。注意 jniLibs 必须开 `useLegacyPackaging = true`：引擎是子进程方式执行
而非 loadLibrary 加载，库必须解压落盘。引擎数据根 = 应用私有目录
`files/engine/`（config/data/技能），卸载即清、天然隔离其他应用。

## 内置 Linux 运行时（Termux 快照）

- **快照按 ABI 制作**：v1.1.0 起为 **arm64**（v1.0 误用 x86_64 模拟器构建，真机
  全部 ENOEXEC——引擎装配期已加 ELF 架构防呆，不匹配自动回落系统 sh，见
  termux.go 的 checkSnapshotArch）。**制作机的 ABI 就是快照的 ABI**。
- 内容：bash/python3/pip/apt/curl/ssh + Office 与 PDF 技能轮子（pypdfium2 用
  官方 android_23_arm64_v8a 轮子）+ USTC 镜像源烘焙。清单与校验和见
  `runtime/MANIFEST.json`；引擎 `jx runtime-selftest` 子命令可整链自检。
- **重制快照（首选 PC 路径）**：WSL Ubuntu 里
  `bash android/runtime/pc-build/build-runtime-pc.sh`（bootstrap → USTC deb
  依赖闭包 → 轮子直解 → 剪枝 → tar.gz + sha256），更新 MANIFEST 后拷为
  `android/runtime/runtime.tgz`，再重打 APK。设备路径（arm64 真机）：
  `android/runtime/make-runtime.ps1 -Serial <设备串号>`。
- 快照升级流程：改 MANIFEST version → 替换 runtime.tgz → 重打 APK → 首启
  引擎按版本水位自动重装配。

## 工作区与权限模型

- 工作区锚定：壳以 `JX_WORKSPACE_DEFAULT=files/engine/workspace` 注入，引擎
  DefaultConfig 消费；旧配置（工作区=引擎根）首启自动迁移并把历史会话改指
  （`/data/data/<pkg>` 与 `/data/user/0/<pkg>` 是同一目录的两种写法，比较按
  别名归一）。手机端不提供「选择工作目录」入口（窄屏隐藏）。
- 标准模式 = 命令在内置运行时里执行，始终处于安卓应用沙箱内（应用 uid +
  SELinux）；文件工具边界由引擎围栏把守（config/data 永久禁访问）。
- **完全访问三件套**（2026-09-30 接入，模拟器 API 35 全链验证）：
  1. **无障碍**（device_screenshot/device_ui/device_tap/device_swipe/
     device_type/device_key）：Kotlin 无障碍服务执行，用户在
     设置→无障碍→已下载的应用→数字助理 开启；截屏走 takeScreenshot
     （API 30+，免 MediaProjection 弹窗）。
  2. **无线 ADB**（adb_connect/adb_shell/adb_screencap）：引擎内置纯 Go
     ADB 主机客户端（adb.go）。真机用法：开发者选项→无线调试 查端口后
     `adb_connect host=127.0.0.1 port=<端口>`；首连会在设备上弹「允许调试」
     （点允许永久记住；模拟器 adbd 不安全版免授权）。协议要点（API 35
     实测）：报文字段小端、CNXN 校验和置 0、v1 shell:/exec: 已移除——
     全走 shell,v2,raw（帧 [id][len:u32le][data]，含退出码，二进制安全）。
  3. **打开应用**（device_app）：引擎直接 am/monkey 启动，无需无障碍。
  - 三件套统一挂「操作手机」能力（perm.go capDevice，完全访问档捆绑点亮，
    桌面端权限芯片自动隐藏）；环境快照实时公告无障碍/ADB 就绪状态。
  - 桥通道：壳 DeviceBridge 长轮询引擎 `/api/device/poll`（仅回环可达），
    结果 POST `/api/device/result`；本机可 `curl /api/device/selftest`
    一键诊断全链（截屏/界面树/ADB 连接/shell/截屏 PNG）。
- 交付出口：`deliver_result` 在手机端把成品落到
  `/sdcard/Download/坚祥数字助理/`（targetSdk 28 legacy storage，运行时申请
  WRITE_EXTERNAL_STORAGE 即可；未授权时报错自带引导文案）。
- API Key：首启引擎**不预置任何模型通道**（与桌面/网页版一致，2026-11-13 起），
  壳里打开就是空的——用户在向导里接入公司通道，或到设置·模型添加提供方后落盘
  （0600，应用私有目录）；想离线演示就把某家提供方的 API 地址填成 `mock`。

## 构建

一条脚本（Windows Git Bash）：

```sh
sh android/build-android.sh
```

流程：交叉编译 arm64+x86_64 引擎（x86_64 需 NDK 链接器）→ 拷入 jniLibs →
快照入 assets（`noCompress "tgz"`，**别用 .gz 后缀**——aapt 会自动解包撑爆
APK）→ 生成开发签名（首次）→ gradlew assembleDebug → 输出 APK 路径。安装：
`adb install -r android/app/build/outputs/apk/debug/app-debug.apk`。

APK 体积 ≈ 103MB（快照 76MB Stored + 双 ABI 引擎压缩后 24MB + 杂项）。
**发布打包前先 `rm -rf android/app/build` 再跑脚本**：增量打包在条目变大
（引擎重编译）时只清零旧区域不压实，zip 里会留出数十至上百 MB 空洞。
