@echo off
rem ============================================================
rem  二开版桌面端（日常使用入口，双击即开）
rem  1) 设置专属互斥体 JX-Desktop-Dev —— 不与安装版
rem     D:\坚祥数字助理GO\坚祥数字助理GO.exe 抢单实例锁，两个可同时开
rem  2) 固定端口 8790（安装版默认 8788），配置与数据都在本目录：
rem     .\config.json + .\data\jx.db —— 重编 exe、切分支都不影响对话
rem  （本文件须保存为 ANSI/GBK 编码 + CRLF：cmd 按 GBK 解析批处理）
rem ============================================================
setlocal
cd /d "%~dp0"
set JX_DESKTOP_MUTEX=JX-Desktop-Dev
if not exist "%~dp0开发桌面版.exe" (
  echo [缺少程序] 开发桌面版.exe 不在本目录。
  echo 请先编译：双击「编译并启动开发版.bat」或运行 打包\开发桌面版-并行.ps1
  echo.
  pause
  exit /b 1
)
start "" "%~dp0开发桌面版.exe" -desktop -port 8790 -noopen
exit /b 0
