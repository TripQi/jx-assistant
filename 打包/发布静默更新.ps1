# 坚祥数字助理GO 静默更新（分块）发布脚本
#
# 用法（在仓库根目录）：
#   powershell -NoProfile -ExecutionPolicy Bypass -File 打包\发布静默更新.ps1
# 可选参数：
#   -Version   版本号（默认=今天日期+V1；同日重出多版手动传 V2 V3）
#   -组件      要发布的组件块，逗号分隔的组件目录名（如 voice,todo-scheduler；
#              默认不发组件——大多数版本只更新主程序）
#   -安装包    附带完整安装包的路径（可选；同时保留整包更新路）
#   -ExeSource 使用已构建好的发行 exe（默认现场 go build）
#   -Runtime   同时发共享运行时块（技能\_runtime 整目录包，压缩后约 100+ MB；
#              版本串默认取 python.exe -V，如 py3.14.8）
#   -Skills    同时发内置技能块（只压白名单内置技能目录 pdf/ocr/office/
#              downloader/apkinfo/web-search-local；用户技能 技能\user 永不入包）
#   -RuntimeVersion 手工指定运行时块版本串（缺省=本次发布版本号，日期式）
#
# 产出：打包\静默更新\<版本>\ 目录——
#   engine-<版本>.zip       主程序块（zip 内单个 exe，客户端换名落位）
#   <组件>-<manifest版本>.zip 组件块（整目录包，版本读 manifest.json）
#   发布命令.txt            在服务器上执行的 publish-update 命令 + sha256 校验表
#
# 服务器端发布（jxai-server，在服务端机器上、数据目录旁执行）：
#   jxai-server publish-update -version <版本> -engine engine-<版本>.zip [-component voice=voice-x.zip …]
# 快照式清单：只传本版改动的块，没传的沿用上一版；客户端落后多版一步对齐每块最新。

param(
  [string]$Version = "",
  [string]$组件 = "",
  [string]$安装包 = "",
  [string]$ExeSource = "",
  # exe 文件名切换期（→ AI数字助理，约一周后）：engine zip 内的文件名其实
  # 无关紧要——客户端换装时按自身路径改名落位（update_blocks.go），这里只是
  # 保持与当期发行版一致，源头在 打包安装包.ps1 的 -ExeBaseName。
  [string]$ExeBaseName = "坚祥数字助理GO",
  # 技能区两块（客户端 update_runtime.go 消费）：
  #   -Runtime 发共享便携 Python 运行时（技能\_runtime 整目录包）
  #   -Skills  发内置技能包（只压白名单内置技能目录，用户技能永不入包）
  # 都不传 = 本版不动这两块（快照沿用上一版）。
  [switch]$Runtime,
  [switch]$Skills,
  [string]$RuntimeVersion = ""
)
if (-not $Version) { $Version = (Get-Date -Format "yyyyMMdd") + "V1" }

$ErrorActionPreference = "Stop"
$packDir = $PSScriptRoot

# ---- 打 zip：条目名自己写，一律用 "/" --------------------------------------
# 为什么不用 Compress-Archive：PS 5.1 那版是 .NET Framework 实现，条目名用反
# 斜杠、目录条目以反斜杠结尾、external attrs 连目录位都没有（.NET Framework 不做
# ZIP 规范要求的 "\"→"/" 转换）。客户端 Go 的 unzip 因此认不出这种目录条目，先把
# 它解成一个同名文件，下一层条目再要它当目录时整包失败——2026-11-13 线上
# voice-1.3.8.zip 让每台安装版的组件静默更新全灭，就是这条链路：
#   mkdir ...\voicetype\go-webview2\.github: The system cannot find the path specified.
# 客户端侧已补规范化（skills.go unzip），包里也不该再产出这种条目名。
Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem

function New-CleanZip {
  param(
    [Parameter(Mandatory = $true)][string]$SourceDir,
    [Parameter(Mandatory = $true)][string]$ZipPath,
    [string[]]$ExcludeDirs = @(".git", ".github", ".vscode", ".idea", "__pycache__", "node_modules"),
    [string[]]$ExcludeFiles = @("*.log", "*.exe~", "*.bmp", "*.tmp")
  )
  if (Test-Path $ZipPath) { Remove-Item $ZipPath -Force }
  $src = (Resolve-Path $SourceDir).Path.TrimEnd('\')
  $fs = [System.IO.File]::Open($ZipPath, [System.IO.FileMode]::CreateNew, [System.IO.FileAccess]::Write)
  try {
    $zip = New-Object System.IO.Compression.ZipArchive($fs, [System.IO.Compression.ZipArchiveMode]::Create)
    try {
      Get-ChildItem -LiteralPath $src -Recurse -Force -File | ForEach-Object {
        $rel = $_.FullName.Substring($src.Length + 1).Replace('\', '/')
        $skip = $false
        foreach ($seg in $rel.Split('/')) { if ($ExcludeDirs -contains $seg) { $skip = $true } }
        foreach ($pat in $ExcludeFiles) { if ($_.Name -like $pat) { $skip = $true } }
        if ($skip) { return }
        $entry = $zip.CreateEntry($rel, [System.IO.Compression.CompressionLevel]::Optimal)
        $es = $entry.Open()
        $ins = [System.IO.File]::OpenRead($_.FullName)
        try { $ins.CopyTo($es) } finally { $ins.Dispose(); $es.Dispose() }
      }
    } finally { $zip.Dispose() }
  } finally { $fs.Dispose() }
}

# 产物自检：条目名带反斜杠的包，客户端解压会出各种怪问题（目录条目必挂）
function Assert-CleanZip {
  param([Parameter(Mandatory = $true)][string]$ZipPath)
  $zip = [System.IO.Compression.ZipFile]::OpenRead($ZipPath)
  try {
    $bad = @($zip.Entries | Where-Object { $_.FullName.Contains('\') })
    if ($bad.Count -gt 0) {
      throw ("zip 条目名含反斜杠，客户端解压会整包失败：{0}（首条 {1}）" -f $ZipPath, $bad[0].FullName)
    }
  } finally { $zip.Dispose() }
}
$root = Split-Path -Parent $packDir
$outDir = Join-Path $packDir "静默更新\$Version"
if (Test-Path $outDir) { Remove-Item $outDir -Recurse -Force }
New-Item -ItemType Directory -Path $outDir -Force | Out-Null

# ---- 主程序块：构建 exe（与打包安装包.ps1 同参数）→ 单 exe zip ----
$work = Join-Path $outDir "_build"
New-Item -ItemType Directory -Path $work | Out-Null
$exe = Join-Path $work ($ExeBaseName + ".exe")
if ($ExeSource -ne "") {
  Copy-Item $ExeSource $exe -Force
} else {
  Write-Host "== 编译主程序（注入桌面模式默认值与版本号——engine 块进程重启后要靠 appVersion 认出自己已是新版，漏注入会让待重启横幅永远不消）"
  # 先把组件里的独立语音输入法编出来（独立 Go 模块，主程序 build 不带它）
  & (Join-Path $PSScriptRoot "编译语音输入法.ps1")
  if ($LASTEXITCODE -ne 0) { throw "语音输入法编译失败" }
  go build -trimpath -ldflags "-s -w -H=windowsgui -X main.desktopDefault=1 -X main.appVersion=$Version" -o $exe $root
  if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
}
$engineZip = Join-Path $outDir "engine-$Version.zip"
New-CleanZip -SourceDir $work -ZipPath $engineZip
Remove-Item $work -Recurse -Force

# ---- 组件块：整目录打包，版本读 manifest.json ----
$compArgs = @()
if ($组件 -ne "") {
  foreach ($id in ($组件 -split "," | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne "" })) {
    # 用户组件区（组件/user）不随包、不热更新：它是用户自己从市场/发行渠道装的，
    # 版本与去留归用户——发了组件块也不该覆盖它（update_blocks 也会跳过）。
    if ($id -eq "user") { throw "组件/user 是用户组件区，不参与热更新；要发的是具体组件 id（组件\<id>）" }
    $dir = Join-Path $root "组件\$id"
    $manifest = Join-Path $dir "manifest.json"
    if (-not (Test-Path $manifest)) { throw "组件 $id 没有 manifest.json：$manifest" }
    # manifest 是 UTF-8：PS5.1 默认按 GBK 读会乱码，必须显式指定
    $m = Get-Content $manifest -Raw -Encoding UTF8 | ConvertFrom-Json
    $ver = $m.version
    if (-not $ver) { throw "组件 $id 的 manifest.json 没有 version" }
    $zipName = "$id-$ver.zip"
    Write-Host "== 打包组件块 $id v$ver"
    # 组件目录里可能混着开发机的运行时残留（边跑边写的产物，webview2 profile
    # 还被运行中的实例锁着，先复制后删会在 Copy-Item 上卡死）：复制到临时
    # 目录时就按 打包\组件运行时排除.txt 跳过属于本组件的行，再压临时目录，
    # 源目录不动。清单相对仓库根、/ 分隔、# 注释；robocopy /XD /XF 传完整
    # 路径精确排除（纯名会误伤任意层级同名项）。
    $clean = Join-Path $env:TEMP ("jx-comp-" + $id)
    if (Test-Path $clean) { Remove-Item $clean -Recurse -Force }
    $rtList = Join-Path $root "打包\组件运行时排除.txt"
    $rx = "^组件/" + [regex]::Escape($id) + "/"
    $xd = @(); $xf = @(); $dropped = @()
    if (Test-Path $rtList) {
      Get-Content $rtList -Encoding UTF8 | ForEach-Object {
        $rel = $_.Trim()
        if ($rel -eq "" -or $rel.StartsWith("#")) { return }
        if ($rel -notmatch $rx) { return }
        $src = Join-Path $root ($rel -replace "/", "\")
        if (-not (Test-Path $src)) { return }
        if ((Get-Item $src).PSIsContainer) { $xd += $src } else { $xf += $src }
        $dropped += ($rel -replace $rx, "")
      }
    }
    $rcArgs = @($dir, $clean, "/E", "/R:0", "/W:0", "/NFL", "/NDL", "/NJH", "/NJS")
    if ($xd.Count -gt 0) { $rcArgs += "/XD"; $rcArgs += $xd }
    if ($xf.Count -gt 0) { $rcArgs += "/XF"; $rcArgs += $xf }
    robocopy @rcArgs
    if ($LASTEXITCODE -ge 8) { throw "复制组件 $id 失败（robocopy 退出码 $LASTEXITCODE；可能是清单外的新运行时残留被锁，把它加进 组件运行时排除.txt）" }
    if ($dropped.Count -gt 0) {
      Write-Host ("== 已剔除组件运行时残留：{0}" -f ($dropped -join '、'))
    }
    New-CleanZip -SourceDir $clean -ZipPath (Join-Path $outDir $zipName)
    Remove-Item $clean -Recurse -Force
    $compArgs += "-component", "$id=$zipName"
  }
}

# ---- 技能区块 1：共享运行时（技能\_runtime 整目录包）----
$runtimeArgs = @()
if ($Runtime) {
  $rtDir = Join-Path $root "技能\_runtime"
  if (-not (Test-Path $rtDir)) { throw "运行时目录不存在：$rtDir" }
  $pyExe = Join-Path $rtDir "python\python.exe"
  if (-not (Test-Path $pyExe)) { throw "运行时里没有 python.exe：$pyExe" }
  # ★ 块版本 = 本次发布版本号（日期式，如 20261101-1）。2026-11-01 起不再用
  # "py3.14.8" 这种 Python 版本当块版本——运行时包里除了 Python 还有我们装的库
  # （paramiko/PIL/comtypes…），装了库哈希就变，Python 版本号却不动，就会"同号不同
  # 哈希"（10-20→10-31 事故）。复合串 + 文件名带版本（runtime-<版本>.zip），重打
  # 运行时必然伴随新发布版本号，天然防同号。客户端按"标记相等才跳过"判断，与格式
  # 无关；Python 本体版本仍以 skills.go 的 pythonVersion 常量为准（与此无关）。
  $pyVer = ((& $pyExe -V 2>&1) -replace '^Python\s+', '').Trim()
  if (-not $RuntimeVersion) { $RuntimeVersion = "py" + $pyVer + "-" + $Version }
  Write-Host "== 打包运行时块 $RuntimeVersion（$rtDir）"
  # 版本标记写进包内：客户端据此判断"已经是最新，不必重下几百兆"
  Set-Content -Path (Join-Path $rtDir ".version") -Value $RuntimeVersion -Encoding ASCII
  $runtimeZip = Join-Path $outDir "runtime-$RuntimeVersion.zip"
  # 与其它块同一套打包器：条目名一律 "/"，点开头的 .version 也要进包
  New-CleanZip -SourceDir $rtDir -ZipPath $runtimeZip
  $runtimeArgs += "-runtime", (Split-Path -Leaf $runtimeZip), "-runtime-version", $RuntimeVersion
}

# ---- 技能区块 2：内置技能（白名单目录；用户技能绝不入包）----
$skillsArgs = @()
if ($Skills) {
  # 与客户端内置技能白名单一致（skill_manager.go: builtinSkillOrder）
  $builtin = @("pdf", "ocr", "office", "downloader", "apkinfo", "web-search-local")
  $stageSkills = Join-Path $outDir "_skills_stage"
  New-Item -ItemType Directory -Path $stageSkills -Force | Out-Null
  $packed = @()
  foreach ($name in $builtin) {
    $src = Join-Path $root "技能\$name"
    if (-not (Test-Path $src)) { throw "内置技能目录不存在：$src" }
    Copy-Item $src (Join-Path $stageSkills $name) -Recurse -Force
    $packed += $name
  }
  # 防呆：暂存区里绝不能出现 user / _runtime（白名单之外的目录一律不入包）
  Get-ChildItem $stageSkills -Directory | Where-Object { $builtin -notcontains $_.Name } | ForEach-Object {
    throw "暂存区出现非内置技能目录：$($_.Name)（拒绝入包）"
  }
  $skillsZip = Join-Path $outDir "skills-builtin.zip"
  Write-Host ("== 打包内置技能块（{0}）" -f ($packed -join "、"))
  New-CleanZip -SourceDir $stageSkills -ZipPath $skillsZip
  Remove-Item $stageSkills -Recurse -Force
  # 文件名带上内容哈希前缀：内容变了就是新文件，客户端中途下载不会撞上"同名但内容已换"
  $skHash = (Get-FileHash $skillsZip -Algorithm SHA256).Hash.ToLower().Substring(0, 16)
  $skillsNamed = Join-Path $outDir "skills-builtin-$skHash.zip"
  Move-Item $skillsZip $skillsNamed -Force
  $skillsArgs += "-skills", (Split-Path -Leaf $skillsNamed)
}

# ---- 完整安装包（可选，整包路与静默更新并存） ----
$installerArg = @()
if ($安装包 -ne "") {
  if (-not (Test-Path $安装包)) { throw "安装包不存在：$安装包" }
  Copy-Item $安装包 $outDir -Force
  $installerArg += "-installer", (Split-Path -Leaf $安装包)
}

# ---- 出包自检：所有 zip 的条目名必须是规范正斜杠（带反斜杠的目录条目客户端解压必挂）----
Get-ChildItem $outDir -File -Filter *.zip | ForEach-Object {
  Assert-CleanZip -ZipPath $_.FullName
  Write-Host ("   zip 自检通过 {0}" -f $_.Name)
}

# ---- 发布命令 + 校验表 ----
$lines = @()
$lines += "# 在服务器上（jxai-server 数据目录旁、上传本目录全部文件后）执行："
$cmd = "jxai-server publish-update -version $Version -engine engine-$Version.zip"
if ($compArgs.Count -gt 0) { $cmd += " " + ($compArgs -join " ") }
if ($runtimeArgs.Count -gt 0) { $cmd += " " + ($runtimeArgs -join " ") }
if ($skillsArgs.Count -gt 0) { $cmd += " " + ($skillsArgs -join " ") }
if ($installerArg.Count -gt 0) { $cmd += " " + ($installerArg -join " ") }
$lines += $cmd
$lines += ""
$lines += "# 文件清单与 sha256（传输后核对，客户端下载也会校验）："
Get-ChildItem $outDir -File | ForEach-Object {
  $h = Get-FileHash $_.FullName -Algorithm SHA256
  $lines += ("{0}  {1,10:N1} MB  {2}" -f $_.Name, ($_.Length / 1MB), $h.Hash.ToLower())
}
$lines | Out-File (Join-Path $outDir "发布命令.txt") -Encoding UTF8

Write-Host ""
Write-Host "== 静默更新产物就绪：$outDir" -ForegroundColor Green
Get-ChildItem $outDir -File | ForEach-Object {
  Write-Host ("   {0}  {1,10:N1} MB" -f $_.Name, ($_.Length / 1MB))
}
Write-Host "== 把整个目录上传到服务器后，按 发布命令.txt 执行 publish-update"
