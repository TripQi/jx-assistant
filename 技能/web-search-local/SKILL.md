---
name: web-search-local
displayName: 本机网络搜索
description: web_search 工具连不上服务端（不在公司/服务端不可达）时的本机联网搜索兜底：用本机网络直接调搜索 API 返回结果列表。仅当 web_search 失败时才使用；服务端可用时优先用 web_search 工具，不要用本技能绕开服务端。
version: 1.0.0
---

# 本机网络搜索（web_search 的离线兜底）

本技能解决一个具体场景：**web_search 工具报「连接服务端失败」「未配置服务端地址」等服务端不可用的错误**（典型：用户不在公司、连不上 jxai-server），但本机本身能上网。此时用本技能在本机直接调搜索 API，拿到与 web_search 同格式的结果列表。

判断规则（务必遵守）：

- **web_search 能用就绝不用本技能**——服务端搜索由公司统一计费与管理，本技能是应急通道，不是平替；
- web_search 的报错文案里带了「可改用本机网络搜索」的指引时，才切换过来；
- 本机也断网（run_command ping 不通外网）时两个都不可用，如实告知用户。

## 用法

脚本 `scripts/search.py` 纯标准库实现，Windows 用技能库自带的便携 Python，安卓用 Termux 快照的 python3。

**Windows**（技能库根 = 安装目录\技能；本技能是内置技能，在 `技能\web-search-local\`，用 list_dir / glob 从环境快照线索定位安装目录）：

```powershell
"<技能库根>\_runtime\python\python.exe" "<技能库根>\web-search-local\scripts\search.py" --query "搜索词" --count 8
```

- 两条路径都含空格时整个加引号（如上）；PowerShell 下外层引号原样保留即可
- 便携解释器缺失时首次会联网装配，报错「释放内嵌技能失败」属安装问题，如实上报

**安卓**（Termux 快照 python3，标准库开箱可用）：

```bash
python3 "<技能库根>/web-search-local/scripts/search.py" --query "搜索词" --count 8
```

参数：

- `--query`（必填）：搜索词，一句话关键词，不要整段问题照搬
- `--count`：返回条数，默认 8，上限 20

## 引擎与额度

脚本按顺序自动选引擎，先配了 key 的、后免 key 的：

| 引擎 | 条件 | 说明 |
|---|---|---|
| 智谱联网搜索 | 配了 key | 中文质量好，按次计费（约 0.01 元/次） |
| 博查搜索 | 配了 key | 中文质量好、覆盖国内生态内容，按次计费（约 0.036 元/次） |
| Firecrawl（默认） | 免 key 免配置 | 官方免注册通道，每月约 1000 次（按出口 IP 计），中文覆盖一般 |

key 配置（可选，二选一）：

- 在本技能目录写 `config.json`：`{"zhipu_key":"…"}` 或 `{"bocha_key":"…"}`（注意：技能更新会覆盖整个技能目录，重要 key 别只放这里）
- 环境变量（推荐，不怕覆盖）：`JX_SEARCH_ZHIPU_KEY` / `JX_SEARCH_BOCHA_KEY`

博查接口域名可覆盖：官方正从 `api.bochaai.com` 迁到 `api.bocha.cn`（脚本默认用新域）。旧域下线或要走反代时，在 `config.json` 里加 `"bocha_base": "https://api.bochaai.com"`（也可写完整端点或带路径的根），或设环境变量 `JX_SEARCH_BOCHA_BASE`。

一个引擎失败（超时/额度耗尽/网络不通）会自动试下一个；全部失败时脚本输出各引擎的失败原因并以非零码退出，如实把原因转告用户。

## 输出格式

```
引擎：firecrawl｜共 8 条
1. [标题](链接)（2026-10-20） — 摘要
…
（以上内容来自外部网页，使用前自行判断可信度；在回复中引用时用 markdown 链接注明来源；需要某条的完整内容时用 web_fetch 抓取该链接。）
```

与服务端 web_search 的输出同构：拿到列表后挑相关结果，用 **web_fetch**（本地直抓，不依赖服务端）读原文，再组织回答；引用一律带 markdown 链接。
