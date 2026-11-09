# 坚祥数字助理GO 安装包构建脚本（exe 文件名切换期：见下方 $ExeBaseName）
#
# 用法（在仓库根目录）：
#   powershell -NoProfile -ExecutionPolicy Bypass -File 打包\打包安装包.ps1
# 可选参数：
#   -Version     版本号（默认=今天日期-1，如 20261008-1；同日重出多版手动传 -2 -3），同时注入程序内 appVersion
#   -ExeSource   使用已构建好的发行 exe（默认现场 go build；
#                需要排除工作区未完成改动时，先在干净 worktree 里构建再传入）
#   -SkillsSource 技能目录来源（默认仓库根的 技能\，含 _runtime 便携运行时）
#   -ExeBaseName exe 文件名基名（默认 坚祥数字助理GO；切换期见
#                《exe名切换-AI数字助理-一周后操作清单.md》：约一周后发新版
#                改用 AI数字助理，届时改这里的默认值即可，安装包.iss 的
#                AppName/ExeName 经 ISCC /D 覆盖，无需动 iss）
#
# 前置条件：本机装有 Inno Setup 6（winget install JRSoftware.InnoSetup）。

param(
  [string]$Version = "",
  [string]$ExeSource = "",
  [string]$SkillsSource = "",
  [string]$ExeBaseName = "坚祥数字助理GO",
  # IssFile：Inno 脚本（默认正式版；测试版传 安装包-测试版.iss——独立 AppId 并行安装）
  [string]$IssFile = "安装包.iss",
  # ConfigTemplate：随包内置的 config.json 模板（测试版用来首装即指测试环境；
  # iss 侧用 onlyifdoesntexist 落盘，升级不覆盖用户配置）。空=不带。
  [string]$ConfigTemplate = ""
)
if (-not $Version) { $Version = (Get-Date -Format "yyyyMMdd") + "-1" }

$ErrorActionPreference = "Stop"
$packDir = $PSScriptRoot
$root = Split-Path -Parent $packDir
if ($SkillsSource -eq "") { $SkillsSource = Join-Path $root "技能" }
$stage = Join-Path $packDir "stage"

$pyExe = Join-Path $SkillsSource "_runtime\python\python.exe"
if (-not (Test-Path $pyExe)) {
  throw "技能便携运行时缺失：$pyExe（先在联网机器上跑一次任意技能触发装配，或手工 pip install）"
}

# 自包含原则：发行包必须内置全部依赖。逐个校验各技能 manifest 声明的
# 模块在便携运行时里真实可导入——装配是按需触发的，开发机上没跑过的
# 技能依赖可能没装，缺着出包会让最终用户在离线机器上踩坑，故缺一不出包。
$modules = @{}
# ★ 2026-10-12：递归扫描用户技能的依赖声明（实测曾漏 pywin32 全家；微信操作就在 user\ 下）
# ★ 2026-11-08 口径修正：只校验**内置技能**（技能\ 下非 user）——用户技能不随包
# 分发，其依赖也不该进发行包；此前连 user 技能的依赖一起校验，等于逼着把用户
# 技能装的包（AI 剪视频的 faster-whisper / ctranslate2 / nvidia 全家，2 GB+）
# 塞进共享运行时再随包发给客户，安装包体积直接失控。
Get-ChildItem $SkillsSource -Recurse -Depth 2 -Directory | Where-Object {
  $_.FullName -notmatch '\\_runtime(\\|$)' -and
  $_.FullName -notmatch '\\技能\\user(\\|$)' -and
  (Test-Path (Join-Path $_.FullName "manifest.json"))
} | ForEach-Object {
  # manifest 是 UTF-8：PS5.1 的 Get-Content 默认按 GBK 读会把中文读成
  # 乱码导致 JSON 解析失败、模块清单漏收（校验假通过），必须显式指定
  $m = Get-Content (Join-Path $_.FullName "manifest.json") -Raw -Encoding UTF8 | ConvertFrom-Json
  if ($m.python.modules) { $m.python.modules | ForEach-Object { $modules[$_] = $true } }
  if ($m.variants) {
    $m.variants.PSObject.Properties.Value | ForEach-Object {
      if ($_.modules) { $_.modules | ForEach-Object { $modules[$_] = $true } }
    }
  }
}
if ($modules.Count -eq 0) {
  throw "未从任何 manifest.json 收集到依赖模块——技能目录或解析有问题，拒绝出包"
}
$missing = @()
$ErrorActionPreference = "Continue"   # python 导入失败走 stderr，别被当成脚本错误中断
foreach ($mod in $modules.Keys) {
  & $pyExe -c "import $mod" *> $null
  if ($LASTEXITCODE -ne 0) { $missing += $mod }
}
$ErrorActionPreference = "Stop"
if ($missing.Count -gt 0) {
  throw "技能便携运行时依赖不完整，缺少: $($missing -join ', ')。请在联网机器上把对应技能各跑一遍（触发装配），或用该运行时的 python -m pip install 补装后再打包"
}
Write-Host "== 运行时依赖校验通过（$($modules.Count) 个模块全部可导入）"

if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
New-Item -ItemType Directory -Path $stage | Out-Null

$exe = Join-Path $stage ($ExeBaseName + ".exe")
if ($ExeSource -ne "") {
  Copy-Item $ExeSource $exe -Force
} else {
  Write-Host "== 编译主程序（注入桌面模式默认值）"
  # 先把组件里的独立语音输入法编出来（独立 Go 模块，主程序 build 不带它）
  & (Join-Path $PSScriptRoot "编译语音输入法.ps1")
  if ($LASTEXITCODE -ne 0) { throw "语音输入法编译失败" }
  go build -trimpath -ldflags "-s -w -H=windowsgui -X main.desktopDefault=1 -X main.appVersion=$Version" -o $exe $root
  if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
}

# 出包前硬校验：stage 里那个「exe」必须真是 AMD64 的 PE 可执行文件。
# 教训（20261025-3/4/5 三个安装包全废）：出包时把 engine 的 zip 当成 exe 传给了
# -ExeSource（`-ExeSource 打包\静默更新\X\engine-X.zip`），Copy-Item 照抄，stage 里
# 那个 .exe 其实是 PK 开头的压缩包，装到用户机器上 CreateProcess 直接报 216
# （该版本与你的 Windows 不兼容）。这种东西必须在出包前拦住，不能等用户装。
$exeItem = Get-Item $exe
$fs = [IO.File]::OpenRead($exe)
try { $hdr = New-Object byte[] 4096; $hn = $fs.Read($hdr, 0, $hdr.Length) } finally { $fs.Close() }
$isPE = $false; $machine = 0
if ($hn -gt 0x40 -and $hdr[0] -eq 0x4D -and $hdr[1] -eq 0x5A) {
  $peOff = [BitConverter]::ToInt32($hdr, 0x3C)
  if ($peOff -ge 0 -and ($peOff + 6) -lt $hn -and $hdr[$peOff] -eq 0x50 -and $hdr[$peOff + 1] -eq 0x45) {
    $isPE = $true
    $machine = [BitConverter]::ToUInt16($hdr, $peOff + 4)
  }
}
if (-not $isPE) {
  $head = ($hdr[0..3] | ForEach-Object { '{0:X2}' -f $_ }) -join ' '
  throw ("stage\{0}.exe 不是 PE 可执行文件（头部 $head；zip 是 50 4B 03 04），拒绝出包。检查 -ExeSource 是否误传了 engine 的 .zip" -f $ExeBaseName)
}
if ($machine -ne 0x8664) {
  throw ("stage exe 的 PE 机器类型是 0x{0:X4}，不是 AMD64(0x8664)，拒绝出包：{1}" -f $machine, $exe)
}
if ($exeItem.Length -lt 30MB) {
  throw ("stage exe 只有 {0:N1} MB，远小于正常发行体积（约 50 MB），拒绝出包：{1}" -f ($exeItem.Length / 1MB), $exe)
}
Write-Host ("== stage exe 校验通过：PE AMD64 / {0:N1} MB" -f ($exeItem.Length / 1MB))

Write-Host "== 暂存技能目录（含便携 Python 运行时，体积较大）"
Copy-Item $SkillsSource (Join-Path $stage "技能") -Recurse -Force

# 迁移账本（技能\.migrations.json）是「本机跑过哪些一次性迁移」的记账，绝不能进包：
# 随包发给客户等于替客户声称全部迁移早已跑过，新装机器会跳过首次运行该跑的任务
# （典型：释放内置 MCP 连接器 cua / windows-mcp 的任务），功能凭空缺一块。
Remove-Item (Join-Path $stage "技能\.migrations.json") -Force -ErrorAction SilentlyContinue

# 技能\_runtime 按发行白名单收敛：开发机上跑用户技能时 pip 装进共享运行时的
# 第三方包（实测 AI 剪视频技能带来 nvidia/ctranslate2/faster-whisper 等 2.5 GB）
# 绝不能随包发给客户——用户技能的依赖由对方机器装技能时自己装配。
# 基准是 打包\runtime-基线清单.txt：服务器当前发布中的 runtime 块（客户装的
# 就是它）的条目清单。**不能用 git 索引**——运行时主体（python.exe / Lib /
# DLL）从未入库，按索引收敛会把整个运行时掏空（2026-11-08 实测踩过，包从
# 270 MB 掉到 165 MB、stage 里 python.exe 都没了）。清单缺失或运行时核心缺失
# 一律拒绝出包。
$rtStage = Join-Path $stage "技能\_runtime"
if (Test-Path $rtStage) {
  $rtList = Join-Path $packDir "runtime-基线清单.txt"
  if (-not (Test-Path $rtList)) {
    throw "缺少 打包\runtime-基线清单.txt（_runtime 发行白名单），拒绝出包：请按当前发布中的 runtime 块条目重生清单"
  }
  $tracked = @{}
  Get-Content $rtList -Encoding UTF8 | ForEach-Object {
    $rel = $_.Trim()
    if ($rel -eq "" -or $rel.StartsWith("#")) { return }
    $tracked[$rel] = $true
  }
  if ($tracked.Count -lt 500) { throw "runtime 基线清单条目太少（$($tracked.Count) 条），清单异常，拒绝出包" }
  $rtPrefix = "技能\_runtime\"
  $removedCnt = 0; [long]$removedBytes = 0
  Get-ChildItem $rtStage -Recurse -File -Force | ForEach-Object {
    $rel = ($_.FullName.Substring($rtStage.Length + 1) -replace "\\", "/")
    if (-not $tracked.ContainsKey($rel)) {
      $removedBytes += $_.Length
      Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue
      $removedCnt++
    }
  }
  Get-ChildItem $rtStage -Recurse -Directory -Force | Sort-Object { $_.FullName.Length } -Descending | ForEach-Object {
    if (-not (Get-ChildItem $_.FullName -Force -ErrorAction SilentlyContinue)) {
      Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue
    }
  }
  Write-Host ("== 运行时按发行白名单收敛：剔除清单外 {0} 个文件 / {1:N0} MB（用户技能装的依赖不随包）" -f $removedCnt, ($removedBytes / 1MB))
  $rtPy = Join-Path $rtStage "python\python.exe"
  if (-not (Test-Path $rtPy)) { throw "收敛后运行时缺 python.exe（白名单或源目录异常），拒绝出包：$rtPy" }
  # 自检用 stage 自己的 python 导入**内置技能 manifest 声明的模块集合**（与前面
  # 源运行时校验同一份 $modules），确认白名单没把内置依赖剔掉。python 的报错
  # 走 stderr，先降 ErrorActionPreference 免得被当成脚本错误中断。
  $oldEAP = $ErrorActionPreference
  $ErrorActionPreference = "Continue"
  $bad = @()
  foreach ($rm in $modules.Keys) {
    $out = & $rtPy -c "import $rm" 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0) {
      $bad += ($rm + " -> " + (($out.Trim() -split "`n") | Select-Object -Last 1))
    }
  }
  $ErrorActionPreference = $oldEAP
  if ($bad.Count -gt 0) {
    throw ("收敛后 stage 运行时导入失败：" + ($bad -join "；") + "（白名单缺内置依赖，拒绝出包）")
  }
  Write-Host ("== 收敛后 stage 运行时自检通过（python.exe + {0} 个内置依赖全部可导入）" -f $modules.Count)
}

# 技能\user\ 是「用户自己装的技能」（本机开发机上是我们的内部技能，含服务器地址、
# 部署命令），绝不随安装包发给客户——静默更新块早就是这个口径（发布静默更新.ps1
# 白名单，用户技能永不入包）。这里在 stage 里直接删掉，安装包.iss 那行 Excludes
# 是第二道保险：两层都在，才不会因为将来误改其中一处把内部技能发出去。
$stageUser = Join-Path $stage "技能\user"
if (Test-Path $stageUser) {
  $excluded = (Get-ChildItem $stageUser -Directory -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name)
  Remove-Item $stageUser -Recurse -Force
  Write-Host ("== 已排除用户技能（不随安装包分发）：{0}" -f ($excluded -join '、'))
}

# 内置组件（默认组件必须随包）：组件/ 目录整体暂存，缺失或没有任何
# 带 manifest.json 的组件都拒绝出包——安装包交付离线全功能，默认组件
# （危险命令护栏/待办定时/Git 面板/语音）是其中一部分，不能少。
Write-Host "== 暂存内置组件（默认组件必须随包）"
$compDir = Join-Path $root "组件"
if (-not (Test-Path $compDir)) { throw "内置组件目录缺失：$compDir（默认组件必须随包分发）" }
$compCount = (Get-ChildItem $compDir -Directory | Where-Object {
  Test-Path (Join-Path $_.FullName "manifest.json")
}).Count
if ($compCount -eq 0) { throw "组件目录里没有任何带 manifest.json 的组件，拒绝出包" }
# 组件运行时残留（开发机上边跑边写的产物，如独立语音输入法的录音留档与
# 浏览器 profile）：绝不进发行包，且复制阶段就要跳过——webview2 profile 被
# 运行中的实例锁着，先复制后删除会在 Copy-Item 上卡死。清单见
# 打包\组件运行时排除.txt（相对仓库根、/ 分隔、# 注释）；robocopy 的
# /XD /XF 传完整路径可精确排除（纯目录名会误伤任意层级同名项）。
$rtList = Join-Path $packDir "组件运行时排除.txt"
$rtSkip = @()
if (Test-Path $rtList) {
  $rtSkip = Get-Content $rtList -Encoding UTF8 | ForEach-Object {
    $rel = $_.Trim()
    if ($rel -eq "" -or $rel.StartsWith("#")) { return }
    $rel
  }
}
$stageComp = Join-Path $stage "组件"
$xd = @(); $xf = @()
foreach ($rel in $rtSkip) {
  $src = Join-Path $root ($rel -replace "/", "\")
  if (-not (Test-Path $src)) { continue }
  if ((Get-Item $src).PSIsContainer) { $xd += $src } else { $xf += $src }
}
# 用户组件区（组件/user/）绝不进包：那是用户自己装的东西（市场/发行渠道单个
# 安装），与用户技能 技能/user 同款——不进打包、不上 git、不参与静默更新。
$userCompDir = Join-Path $compDir "user"
if (Test-Path $userCompDir) { $xd += $userCompDir }
$rcArgs = @($compDir, $stageComp, "/E", "/R:0", "/W:0", "/NFL", "/NDL", "/NJH", "/NJS")
if ($xd.Count -gt 0) { $rcArgs += "/XD"; $rcArgs += $xd }
if ($xf.Count -gt 0) { $rcArgs += "/XF"; $rcArgs += $xf }
robocopy @rcArgs
if ($LASTEXITCODE -ge 8) { throw "复制组件目录失败（robocopy 退出码 $LASTEXITCODE；可能是清单外的新运行时残留被锁，把它加进 组件运行时排除.txt）" }
if ($rtSkip.Count -gt 0) {
  Write-Host ("== 组件复制排除清单：{0}" -f ($rtSkip -join '、'))
}
Write-Host "== 内置组件 $compCount 个已暂存"

# 内置 MCP 组件（「操作电脑」能力的来源）：cua-driver 与 windows-mcp 依赖。
# 与上面的「组件/」是两回事——那个是程序内的默认组件（护栏/待办/Git/语音），
# 这个只被内置 MCP 连接器按 {app}\内置组件 解析。缺任一项，装上以后
# 「操作电脑」就是残废，所以缺一不出包。
Write-Host "== 暂存内置 MCP 组件（Cua 驱动 / Windows-MCP 依赖）"
$builtinMcpDir = Join-Path $root "内置组件"
if (-not (Test-Path $builtinMcpDir)) { throw "内置组件目录缺失：$builtinMcpDir（操作电脑能力必须随包）" }
Copy-Item $builtinMcpDir (Join-Path $stage "内置组件") -Recurse -Force
# __pycache__ 跨机器无效、白占体积，不入包
Get-ChildItem (Join-Path $stage "内置组件") -Recurse -Directory -Filter "__pycache__" -ErrorAction SilentlyContinue |
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
# 依赖去重：与共享运行时版本一致的第三方包剔掉（numpy/Pillow/cryptography/pywin32…）。
# Windows-MCP 跑的就是同一个便携运行时，这些是纯重复——实测 138.1→54.7 MB，
# 去重后 MCP 握手 21 个工具、Snapshot 实调均正常。版本不一致的包一律保留。
$stripPy = Join-Path $packDir "精简内置组件.py"
$siteDir = Join-Path $SkillsSource "_runtime\python\Lib\site-packages"
if (-not (Test-Path $stripPy)) { throw "缺少去重脚本：$stripPy" }
& $pyExe $stripPy --site $siteDir --target (Join-Path $stage "内置组件\wmcppkg")
if ($LASTEXITCODE -ne 0) { throw "内置组件依赖去重失败（退出码 $LASTEXITCODE）" }

foreach ($must in @("cua-driver\cua-driver.exe", "wmcppkg\windows_mcp", "windows-mcp-boot.py")) {
  # 嵌套 Join-Path：三参形态只有 PowerShell 7 认，5.1 会当位置参数错误（20261104 实测）
  if (-not (Test-Path (Join-Path (Join-Path $stage "内置组件") $must))) { throw "内置组件不完整，缺少 $must，拒绝出包" }
}
Write-Host "== 内置 MCP 组件已暂存"

# 本地语音识别 worker 已随「组件/voice」自包含（worker 与模型迁入组件本体，
# 见提交 623f93cb77），上方「暂存内置组件」已把它整体带入 stage\组件\voice；
# 这里只做防呆校验：组件里缺 worker 同样拒绝出包（缺文件时语音组件的
# 「本地语音识别模型」档一按听写就报「未找到 asr_worker.py」）。
$asrWorker = Join-Path $compDir "voice\asr_worker.py"
if (-not (Test-Path $asrWorker)) {
  throw "组件\voice\asr_worker.py 缺失：$asrWorker（本地语音识别 worker 随组件分发，必须存在）"
}

# MD 查看器（装完即有）：单文件 exe 全自包含（WebView2 托管程序集与查看器
# 模板均已内嵌），随包只带 exe + 使用说明两个文件；.md 关联由安装器装完时
# 以 /register 静默写入（见 安装包.iss [Run]），缺 exe 拒绝出包。
Write-Host "== 暂存 MD 查看器（md-viewer/）"
$mdvDir = Join-Path $root "md-viewer"
if (-not (Test-Path (Join-Path $mdvDir "MD查看器.exe"))) {
  throw "md-viewer\MD查看器.exe 缺失：$mdvDir（装完即有的 MD 查看器必须随包）"
}
New-Item -ItemType Directory -Path (Join-Path $stage "md-viewer") | Out-Null
Copy-Item (Join-Path $mdvDir "MD查看器.exe") (Join-Path $stage "md-viewer") -Force
Copy-Item (Join-Path $mdvDir "使用说明.txt") (Join-Path $stage "md-viewer") -Force

Copy-Item (Join-Path $packDir "使用说明.md") $stage -Force

# 第三方组件声明（开源合规）：随安装包分发，装到 {app}\THIRD-PARTY-NOTICES.md；
# 与 aria2c 的 COPYING/LICENSE.OpenSSL（bin 目录自带）共同覆盖分发义务。
$notices = Join-Path $root "THIRD-PARTY-NOTICES.md"
if (-not (Test-Path $notices)) { throw "缺少第三方组件声明：$notices（仓库根目录必须有 THIRD-PARTY-NOTICES.md）" }
Copy-Item $notices $stage -Force

# 测试版：内置配置模板 → stage\config.json（iss 以 onlyifdoesntexist 落盘）
if ($ConfigTemplate -ne "") {
  if (-not (Test-Path $ConfigTemplate)) { throw "ConfigTemplate 不存在：$ConfigTemplate" }
  Copy-Item $ConfigTemplate (Join-Path $stage "config.json") -Force
  Write-Host "== 已内置配置模板：$ConfigTemplate"
}

# ---- 安装期清理清单（清理清单.iss，被 安装包.iss 的 [InstallDelete] 段包含）----
# 随包目录在升级安装时整目录重装（先删后装），包内已移除的文件不再残留——
# 旧组件文件混进新版本是真会出错的（v1.0.2 的 asr\ 残留案例）。
# 只列「随包发的」目录：技能\_runtime、内置技能、内置组件、md-viewer；
# 技能\user（用户技能）与市场装的组件（id 不在 stage\组件 清单里）天然保留，
# data\、config.json、默认工作区\、插件\ 不经此清单，永不触碰。
$cleanLines = New-Object System.Collections.Generic.List[string]
$cleanLines.Add('Type: filesandordirs; Name: "{app}\技能\_runtime"')
Get-ChildItem (Join-Path $stage "技能") -Directory | Where-Object {
  $_.Name -ne "_runtime" -and $_.Name -ne "user"
} | ForEach-Object {
  $cleanLines.Add('Type: filesandordirs; Name: "{app}\技能\' + $_.Name + '"')
}
if (Test-Path (Join-Path $stage "组件")) {
  # 只清随包发的内置组件；用户组件区（组件\user）与用户技能一样天然保留
  Get-ChildItem (Join-Path $stage "组件") -Directory | Where-Object {
    $_.Name -ne "user"
  } | ForEach-Object {
    $cleanLines.Add('Type: filesandordirs; Name: "{app}\组件\' + $_.Name + '"')
  }
}
if (Test-Path (Join-Path $stage "md-viewer")) {
  $cleanLines.Add('Type: filesandordirs; Name: "{app}\md-viewer"')
}
# PS5.1 的 utf8 带 BOM——ISCC 认 BOM 才能正确读中文路径（无 BOM 会被当
# ANSI 码页读成乱码，清单失配只是清不掉，不会误删，但仍要写对）
$cleanLines | Out-File (Join-Path $packDir "清理清单.iss") -Encoding utf8
Write-Host "== 安装期清理清单：$($cleanLines.Count) 个随包目录升级时整目录重装（用户数据/用户装的组件插件技能不受影响）"

Write-Host "== Inno Setup 编译安装包（LZMA2 压缩，耐心等待）"
$iscc = Join-Path $env:LOCALAPPDATA "Programs\Inno Setup 6\ISCC.exe"
if (-not (Test-Path $iscc)) { $iscc = "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" }
if (-not (Test-Path $iscc)) { throw "找不到 ISCC.exe，请先安装 Inno Setup 6" }
& $iscc "/DMyAppVersion=$Version" "/DMyAppName=$ExeBaseName" "/DMyAppExeName=$ExeBaseName.exe" (Join-Path $packDir $IssFile)
if ($LASTEXITCODE -ne 0) { throw "ISCC 编译失败" }

$out = Join-Path $packDir "输出\$ExeBaseName-安装包-$Version.exe"
Write-Host "完成：$out"
