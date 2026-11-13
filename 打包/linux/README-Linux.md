# jx-assistant Linux (amd64)

坚祥数字助理（GO 版）Linux amd64 发行包：单个静态链接二进制（Go 编译，无 CGo、
无动态库依赖），内嵌全部前端资源与 Linux 沙箱 helper，浏览器访问即完整使用。

## 系统要求

- Linux amd64，内核 ≥ 5.19 推荐（命令沙箱用 Landlock，5.13+ 可用；更老的内核
  自动退化为不限制执行）
- curl + tar（安装脚本用）；systemd 可选（无 systemd 也可手动运行）
- 不需要 Python：核心功能开箱即用；需要 run_python / 技能脚本时系统装 python3
  即可（程序会自动探测 /usr/bin/python3）

## 一键安装（服务端已发布 Linux 附件时）

```sh
curl -fsSL https://jxaizz.com:2227/linux/install.sh | sh
```

或下载本目录的 install.sh 后：

```sh
sh install.sh            # 安装/升级到 /opt/jx-assistant 并注册 systemd 服务
```

离线安装（已有 tar.gz，不走网络）：

```sh
sh install.sh --file jx-assistant-linux-amd64.tar.gz
```

常用参数：`--base <URL>` 更新源（默认 https://jxaizz.com:2227）、
`--dest <目录>` 安装位置（默认 /opt/jx-assistant）、`--no-service` 只放文件
不注册 systemd、`--insecure` 允许自签证书下载源。

安装器做的事：下载最新 Release 里的 `jx-assistant-linux-amd64.tar.gz` →
解压到安装目录（只覆盖二进制与 README，`config.json` / `data/` / `技能/` /
工作区一概不动，重复执行即为升级）→ 写入 `/etc/systemd/system/jx-assistant.service`
→ `systemctl daemon-reload && systemctl enable --now jx-assistant`。

## 手动安装

```sh
tar -xzf jx-assistant-linux-amd64.tar.gz        # 得到 jx-assistant-linux-amd64 等文件
mkdir -p /opt/jx-assistant
install -m 0755 jx-assistant-linux-amd64 /opt/jx-assistant/jx-assistant
install -m 0644 jx-assistant.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now jx-assistant
```

不使用 systemd 时直接运行（前台）：

```sh
cd /opt/jx-assistant && ./jx-assistant -noopen
```

## 配置

配置文件是二进制旁的 `config.json`，首次启动自动生成。常用项：

| 项 | 默认 | 说明 |
|---|---|---|
| `port` | 8788 | 监听端口 |
| `bindHost` | 127.0.0.1 | 改 `0.0.0.0` 后局域网/远程可访问（注意防火墙） |
| `baseURL` | DeepSeek 官方 | OpenAI 兼容接口；填 `mock` 进离线演示模式（无需 Key） |
| `apiKey` | 空 | 模型密钥，明文落盘（Linux 无系统密钥库，文件权限 0600） |
| `workspace` | 安装目录 | 模型的工作目录 |

改完配置重启服务：`systemctl restart jx-assistant`。

## 服务管理

```sh
systemctl status jx-assistant      # 运行状态
journalctl -u jx-assistant -f      # 跟踪日志（也可看 安装目录/data/desktop.log）
systemctl stop jx-assistant        # 停止（SIGTERM 优雅退出）
```

## 目录布局（都在安装目录下，升级不丢）

```
/opt/jx-assistant/jx-assistant    主程序
/opt/jx-assistant/config.json     配置（含 API Key，0600）
/opt/jx-assistant/data/           会话库（jx.db）等运行数据
/opt/jx-assistant/技能/            技能安装区
/opt/jx-assistant/workspace/      默认工作区（config.workspace 可改）
```

## Linux 版功能边界（相对 Windows 桌面版）

| 能力 | Linux 状态 |
|---|---|
| 网页端（对话/工具/子任务编排/技能市场/远程接入/更新检查） | ✅ 完整可用（浏览器访问形态） |
| run_command / run_python / 文件工具 | ✅ 可用（shell 走 sh；沙箱为 Landlock） |
| 桌面窗口壳（-desktop）、系统托盘、关窗驻留 | ❌ 仅 Windows，Linux 明确报错并按浏览器模式跑 |
| 语音听写（本地 ASR 组件、独立语音输入法 voicetype） | ❌ Windows 专属；听写可走云端渠道 |
| 密钥保护 | ❌ 无 DPAPI，明文落盘（0600），注意目录权限 |
| 内置组件（危险命令护栏/待办/Git 面板/语音等随包组件） | ❌ 本包不含；后续可经组件市场/服务端分块分发 |
| 程序内静默自更新 | ❌ Windows 机制不适用；用 install.sh 重跑升级 |
| WebView 依赖 | 无任何 GUI 依赖，纯 headless 服务 |

## 已验证环境

- WSL2 Ubuntu 24.04（内核 6.18，systemd）：启动/网页端全链路（发消息→派发子任务
  →工具执行→摘要回传）/headless 驻留/SIGTERM 优雅退出，详见仓库
  `打包/linux/`。
