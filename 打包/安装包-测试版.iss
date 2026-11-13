; 测试版安装包脚本（与正式版 安装包.iss 的差异）：
;   1) AppId 独立 → 与正式版完全并行安装（不同目录、不同卸载项、互不升级覆盖）;
;   2) 不 taskkill 正式版进程（正式版脚本里的迁移清理对测试版无意义且会误杀）;
;   3) 不注册 .md 文件关联（避免抢正式版的关联指向测试版路径）;
;   4) 随包带 config.json（onlyifdoesntexist：首次安装即指向测试环境
;      210:2229，升级安装不覆盖用户改过的配置）。
; 由 打包安装包.ps1 -IssFile 安装包-测试版.iss -ExeBaseName AI数字助理测试版
;   -ConfigTemplate config-测试环境.json 调用。

#ifndef MyAppVersion
#define MyAppVersion "1.0.0"
#endif

#ifndef MyAppName
#define MyAppName "AI数字助理测试版"
#endif
#ifndef MyAppExeName
#define MyAppExeName "AI数字助理测试版.exe"
#endif
#if Pos("V", MyAppVersion) > 0
  #define MIVYear Copy(MyAppVersion, 1, 4)
  #define MIVMonth Copy(MyAppVersion, 5, 2)
  #define MIVDay Copy(MyAppVersion, 7, 2)
  #define MIVRev Copy(MyAppVersion, Pos("V", MyAppVersion) + 1, 8)
  #define MyAppFileVersion MIVYear + "." + MIVMonth + "." + MIVDay + "." + MIVRev
#elif Pos("-", MyAppVersion) > 0
  #define MIVYear Copy(MyAppVersion, 1, 4)
  #define MIVMonth Copy(MyAppVersion, 5, 2)
  #define MIVDay Copy(MyAppVersion, 7, 2)
  #define MIVRev Copy(MyAppVersion, Pos("-", MyAppVersion) + 1, 8)
  #define MyAppFileVersion MIVYear + "." + MIVMonth + "." + MIVDay + "." + MIVRev
#else
  #define MyAppFileVersion MyAppVersion + ".0"
#endif

[Setup]
AppId={{7A3C1F52-8B9E-4D6A-9C21-5E0FAB12CD34}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher=测试公司
DefaultDirName={localappdata}\Programs\{#MyAppName}
DefaultGroupName={#MyAppName}
PrivilegesRequired=lowest
DisableProgramGroupPage=yes
OutputDir=输出
OutputBaseFilename={#MyAppName}-安装包-{#MyAppVersion}
SetupIconFile=app.ico
UninstallDisplayName={#MyAppName}
UninstallDisplayIcon={app}\{#MyAppExeName}
VersionInfoVersion={#MyAppFileVersion}
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
CloseApplications=no

[Languages]
Name: "chinesesimplified"; MessagesFile: "ChineseSimplified.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: checkedonce

[Files]
Source: "stage\{#MyAppExeName}"; DestDir: "{app}"; Flags: ignoreversion
; 测试环境配置：首次安装即指向 210:2229 测试服务端;用户改过就不再覆盖
Source: "stage\config.json"; DestDir: "{app}"; Flags: onlyifdoesntexist
Source: "stage\技能\*"; DestDir: "{app}\技能"; Flags: ignoreversion recursesubdirs createallsubdirs; Excludes: "user\*,user"
Source: "stage\组件\*"; DestDir: "{app}\组件"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "stage\内置组件\*"; DestDir: "{app}\内置组件"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "stage\md-viewer\*"; DestDir: "{app}\md-viewer"; Flags: ignoreversion
Source: "stage\使用说明.md"; DestDir: "{app}"; Flags: ignoreversion

[InstallDelete]
Type: filesandordirs; Name: "{app}\asr"
#if FileExists(AddBackslash(SourcePath) + "清理清单.iss")
  #include "清理清单.iss"
#endif

[Icons]
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; WorkingDir: "{app}"; Parameters: "-desktop"
Name: "{group}\卸载{#MyAppName}"; Filename: "{uninstallexe}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; WorkingDir: "{app}"; Parameters: "-desktop"; Tasks: desktopicon

[Run]
; 测试版不注册 .md 关联（不抢正式版的关联），只保留装完启动
Filename: "{app}\{#MyAppExeName}"; WorkingDir: "{app}"; Description: "运行{#MyAppName}"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "taskkill"; Parameters: "/IM {#MyAppExeName} /F"; Flags: runhidden; RunOnceId: "KillApp"

[UninstallDelete]
Type: filesandordirs; Name: "{app}\data\webview"
Type: filesandordirs; Name: "{app}\data\webview-browser"

[Code]
procedure TaskKillApp();
var
  rc: Integer;
begin
  // 只结束测试版自身（绝不动正式版进程：坚祥数字助理GO.exe / MD查看器 可能是
  // 正式版在用的，测试版没有迁移清理需求）
  Exec(ExpandConstant('{cmd}'), '/C taskkill /IM {#MyAppExeName} /F', '', SW_HIDE, ewWaitUntilTerminated, rc);
end;

function WebView2Installed(): Boolean;
var
  pv: String;
begin
  Result :=
    RegQueryStringValue(HKLM, 'SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}', 'pv', pv) and (pv <> '') and (pv <> '0.0.0.0');
  if not Result then
    Result :=
      RegQueryStringValue(HKLM, 'SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}', 'pv', pv) and (pv <> '') and (pv <> '0.0.0.0');
  if not Result then
    Result :=
      RegQueryStringValue(HKCU, 'Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}', 'pv', pv) and (pv <> '') and (pv <> '0.0.0.0');
end;

function InitializeSetup(): Boolean;
var
  rc: Integer;
begin
  Result := True;
  if not WebView2Installed() then begin
    if MsgBox(
        '未检测到 Microsoft WebView2 运行时（Windows 10 / 11 一般自带）。' + #13#10 + #13#10 +
        '是否现在打开微软官方下载页安装 WebView2？',
        mbConfirmation, MB_YESNO) = IDYES then
      ShellExec('open', 'https://go.microsoft.com/fwlink/p/?LinkId=2124703', '', '', SW_SHOWNORMAL, ewNoWait, rc);
  end;
  TaskKillApp();
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  TaskKillApp();
  Sleep(1500);
  Result := '';
end;
