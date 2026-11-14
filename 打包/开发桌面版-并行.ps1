# 开发桌面版（并行实例 + 数据隔离）：编译到仓库根的 开发桌面版.exe，用专属
# 数据目录 + 专属互斥体 + 随机端口启动 WebView 桌面窗口——与安装版 /
# 坚祥数字助理GO.exe、以及仓库根的正式数据（config.json、data\）全不相干：
#   - 数据：-config 指向 开发环境\config.json（其中 dataDir=开发环境\data、
#     workspace=开发环境\工作区），开发环境的会话/数据库/技能状态/WebView2
#     profile/兼容模式标记全落在这里（profile 与标记跟随 dataDir，见
#     desktop_windows.go 的 desktopDataDir），正式数据一个字节都不碰
#   - 互斥体：JX_DESKTOP_MUTEX 覆盖为开发专属名（desktop_windows.go 内置
#     支持），不与正式实例互斥；kill 旧实例只按本脚本产物的完整路径匹配，
#     绝不误杀 D:\123\ 等处的安装版
#   - 端口：-port 传随机高位端口；窗口 URL 用实际监听端口，极端碰撞时
#     main.go 监听失败自动回退系统分配（:0），永远起得来
#   - 控制台子系统（不注入 -H=windowsgui）：日志/panic 直接落在开发版
#     自己的控制台窗口，启动期自检炸了也看得见原因（桌面 GUI 版闪退无感）
# 配置只在首次生成：想沿用正式版的模型通道/API Key，把仓库根的 config.json
# 复制到 开发环境\config.json 后再跑一次（脚本只把 dataDir/workspace 改回
# 隔离目录，其余字段原样保留）。想清空开发数据重来：删 开发环境\data 与
# 开发环境\工作区，或运行 编译并启动开发版.bat clean。
# 用法：powershell -NoProfile -ExecutionPolicy Bypass -File 打包\开发桌面版-并行.ps1
#       或直接双击根目录「编译并启动开发版.bat」（调用的就是本脚本）
$ErrorActionPreference = "Stop"
$root    = Split-Path -Parent $PSScriptRoot
$exe     = Join-Path $root "开发桌面版.exe"
$devRoot = Join-Path $root "开发环境"
$devCfg  = Join-Path $devRoot "config.json"
$devData = Join-Path $devRoot "data"
$devWs   = Join-Path $devRoot "工作区"

# JSON 字符串字面量（转义反斜杠与引号）：Windows 路径要按 JSON 规则写进配置
function To-JsonString([string]$s) { '"' + ($s -replace '\\', '\\' -replace '"', '\"') + '"' }

# 把配置文本里某个顶层键的值改成 $value；键不存在时插到最外层对象开头。
# 为什么用正则改文本而不是 ConvertFrom-Json 往返：往返会重排格式、把不认得
# 的东西（非 ASCII 转义等）改写一遍，用户手动复制来的配置看着一团糟。
function Set-DevConfigValue([string]$text, [string]$key, [string]$value) {
  $lit = To-JsonString $value
  $pat = '"' + [regex]::Escape($key) + '"\s*:\s*"(?:[^"\\]|\\.)*"'
  if ([regex]::IsMatch($text, $pat)) {
    return [regex]::Replace($text, $pat, { param($m) '"' + $key + '": ' + $lit })
  }
  $brace = $text.IndexOf('{')
  if ($brace -lt 0) { return $text }
  return $text.Substring(0, $brace + 1) + "`r`n  `"$key`": $lit," + $text.Substring($brace + 1)
}

# 1) 只关本脚本产物的旧实例（按完整路径匹配：安装版与其他桌面 exe 不受影响）
$old = @(Get-CimInstance Win32_Process -Filter "Name='开发桌面版.exe'" -ErrorAction SilentlyContinue |
  Where-Object { $_.ExecutablePath -eq $exe })
foreach ($p in $old) {
  Write-Host ("关闭旧开发实例 pid=" + $p.ProcessId)
  try { Stop-Process -Id $p.ProcessId -Force -ErrorAction Stop } catch { }
}
if ($old.Count -gt 0) { Start-Sleep -Seconds 2 }

# 2) 隔离数据目录与配置：开发环境的会话、数据库、技能状态、浏览器 profile
#    全部落在 开发环境\ 下，与正式数据（仓库根 config.json / data\）无关
New-Item -ItemType Directory -Force -Path $devRoot, $devData, $devWs | Out-Null
$utf8NoBom = New-Object System.Text.UTF8Encoding $false  # Go 的 json.Unmarshal 不吃 BOM
if (-not (Test-Path $devCfg)) {
  $text = "{`r`n  `"dataDir`": $(To-JsonString $devData),`r`n  `"workspace`": $(To-JsonString $devWs)`r`n}`r`n"
  [IO.File]::WriteAllText($devCfg, $text, $utf8NoBom)
  Write-Host "已生成开发配置（首次）：$devCfg"
}
# 每次启动都校验一次隔离路径：手改过的配置（或从正式版复制来的 config）里
# dataDir/workspace 必须落在 开发环境\ 下，否则开发版会写进正式数据
$cfgText = [IO.File]::ReadAllText($devCfg)
$fixedText = Set-DevConfigValue (Set-DevConfigValue $cfgText "dataDir" $devData) "workspace" $devWs
if ($fixedText -ne $cfgText) {
  [IO.File]::WriteAllText($devCfg, $fixedText, $utf8NoBom)
  Write-Host "已把开发配置的数据目录/工作区校正到 开发环境\ 下"
}

# 3) 先编译语音组件里的独立输入法（独立 Go 模块，主程序 build 不会带上它）
& (Join-Path $PSScriptRoot "编译语音输入法.ps1")
if ($LASTEXITCODE -ne 0) { throw "语音输入法编译失败" }

# 4) 编译主程序（失败即抛出，调用方按退出码提示；桌面形态由 -desktop 参数开启）
go build -trimpath -ldflags "-s -w -X main.appVersion=20991231-1" -o $exe $root  # appVersion 注入未来版本号：开发版永远比服务器新，不弹自动更新
if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
Write-Host "完成：$exe（开发桌面版·隔离数据，gitignore 的本地产物）"

# 5) 启动：专属互斥体 + 隔离配置 + 随机高位端口（子进程继承本进程环境变量）
$env:JX_DESKTOP_MUTEX = "JX-Desktop-Dev"
$port = Get-Random -Minimum 20000 -Maximum 60000
Write-Host ("启动：互斥体 JX-Desktop-Dev（不与正式实例互斥），端口 $port（碰撞时自动改系统分配）")
Write-Host "开发数据目录：$devData"
Write-Host "开发工作区：  $devWs（想直接改本仓库代码，可在设置里把工作目录换成仓库根）"
Start-Process -FilePath $exe -ArgumentList @("-desktop", "-config", "`"$devCfg`"", "-port", "$port", "-noopen") -WorkingDirectory $root
Write-Host "已启动开发桌面版：日志在它自己的控制台窗口（关闭本窗口不影响它）。"
