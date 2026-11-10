# Linux amd64 发行包打包脚本（发版用，仓库根目录运行）
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File 打包\linux\打包-linux.ps1
#
# 产物：打包\linux\jx-assistant-linux-amd64.tar.gz
#   （内含 jx-assistant-linux-amd64 / README-Linux.md / jx-assistant.service 平铺）
# 发版时把 tar.gz 一并作为附件上传（见 AGENT.md「Linux 版发行与发版」节）；
# install.sh 与源文件同目录入库，包体已 gitignore。
param(
  [string]$OutDir = "$PSScriptRoot"
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
Set-Location $repoRoot

$stage = Join-Path $OutDir 'stage-linux'
if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
New-Item $stage -ItemType Directory | Out-Null

Write-Host "== 交叉编译 linux/amd64"
$env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
go build -trimpath -ldflags "-s -w" -o (Join-Path $stage 'jx-assistant-linux-amd64') .
if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
$env:GOOS = ''; $env:GOARCH = ''; $env:CGO_ENABLED = ''

Copy-Item (Join-Path $OutDir 'README-Linux.md') $stage
Copy-Item (Join-Path $OutDir 'jx-assistant.service') $stage

$pkg = Join-Path $OutDir 'jx-assistant-linux-amd64.tar.gz'
if (Test-Path $pkg) { Remove-Item $pkg -Force }
Write-Host "== 打包 tar.gz"
tar -czf $pkg -C $stage jx-assistant-linux-amd64 README-Linux.md jx-assistant.service
if ($LASTEXITCODE -ne 0) { throw "tar 打包失败" }
Remove-Item $stage -Recurse -Force

$size = (Get-Item $pkg).Length
Write-Host ("完成: {0}（{1:N1} MB）" -f $pkg, ($size / 1MB))
Write-Host "sha256: $((Get-FileHash $pkg -Algorithm SHA256).Hash.ToLower())"
