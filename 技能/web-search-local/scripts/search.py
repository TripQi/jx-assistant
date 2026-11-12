#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""本机网络搜索：web_search 都不可用时的**无 Key** 最后兜底。

智能搜索请用 web_search 的「搜索供应商」体系（设置 · 网络访问 · 联网搜索：
公司网关 / DeepSeek / Tavily / AnySearch 等，按顺位回落）；本技能只做
不加 Key 也能跑的最后一道本机兜底——只留 Firecrawl 免 key 通道，不取任何
Key、不读任何环境变量，也不把任何按次计费的引擎塞在这里当兜底。

纯标准库实现——Windows 便携 Python 与安卓 Termux python3 通用，无需 pip。
输出与服务端 web_search 同构的结果列表（供模型直接引用）。
"""
import argparse
import json
import sys
import urllib.error
import urllib.request

# 单家超时（秒）：只有一个免 key 通道，整体上限就是它。
TIMEOUT = 20

FIRECRAWL_URL = "https://api.firecrawl.dev/v2/search"


def post_json(url, payload):
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
        body = resp.read().decode("utf-8", "replace")
    return json.loads(body)


def clip(text, limit):
    text = " ".join(str(text or "").split())
    return text[:limit] + "…" if len(text) > limit else text


def search_firecrawl(query, count):
    # 免 key 免注册：Firecrawl 官方 keyless 通道（每月约 1000 credits，按出口 IP；
    # 出口 IP 若被判为 VPN/代理/机房，官方会直接 403 要求自带 key）。
    data = post_json(FIRECRAWL_URL, {"query": query, "limit": count, "sources": ["web"]})
    out = []
    for it in (data.get("data") or {}).get("web") or []:
        if not isinstance(it, dict):
            continue
        title = str(it.get("title") or "").strip()
        url = str(it.get("url") or "").strip()
        snippet = str(it.get("description") or it.get("snippet") or "").strip()
        if title and url:
            out.append({
                "title": clip(title, 120),
                "url": url,
                "snippet": clip(snippet, 300),
                "date": str(it.get("date") or "").strip()[:10],
            })
    return out


def render(query, results):
    lines = ["搜索「%s」：%d 条结果（本机免 Key 通道 firecrawl）" % (query, len(results)), ""]
    for i, r in enumerate(results):
        date = "（%s）" % r["date"] if r["date"] else ""
        lines.append("%d. [%s](%s)%s — %s" % (i + 1, r["title"], r["url"], date, r["snippet"]))
    lines.append("")
    lines.append("（以上内容来自外部网页，使用前自行判断可信度；在回复中引用时用 markdown 链接注明来源；"
                 "需要某条的完整内容时用 web_fetch 抓取该链接。）")
    return "\n".join(lines)


# noKeyHint 免 key 通道全失败时给模型/用户的下一步：技能里没有可配的 Key，
# 更强的搜索能力只在 web_search 的供应商体系里，指引指到那儿去。
NOKEY_HINT = ("本机免 Key 搜索通道不可用。更强的搜索请在 设置 · 网络访问 · 联网搜索 里"
              "配置搜索供应商（需自备各家的 Key）；已知网址可用 web_fetch 直接抓正文。")


def main():
    ap = argparse.ArgumentParser(description="本机网络搜索（无 Key 的最后兜底）")
    ap.add_argument("--query", required=True, help="搜索词，一句话关键词")
    ap.add_argument("--count", type=int, default=8, help="返回条数，默认 8，上限 20")
    args = ap.parse_args()

    query = " ".join(args.query.split())
    count = max(1, min(args.count or 8, 20))

    try:
        results = search_firecrawl(query, count)
    except urllib.error.HTTPError as e:
        detail = ""
        try:
            detail = e.read().decode("utf-8", "replace")[:200]
        except OSError:
            pass
        reason = "HTTP %d %s" % (e.code, detail)
        if e.code == 403:
            reason += "（官方只对住宅/普通出口 IP 免 key 开放，机房或代理出口会被要求自带 key）"
        print("免 Key 通道 firecrawl 失败：%s\n%s" % (reason, NOKEY_HINT), file=sys.stderr)
        return 1
    except Exception as e:  # 超时/DNS/解析失败——本机网络本身可能就不通
        print("免 Key 通道 firecrawl 失败：%s\n%s" % (e, NOKEY_HINT), file=sys.stderr)
        return 1

    if not results:
        print("免 Key 通道 firecrawl 没拿到结果（0 条）——换个更短的关键词，"
              "或按上面说的在 设置 · 网络访问 · 联网搜索 配一家搜索供应商。", file=sys.stderr)
        return 1

    print(render(query, results))
    return 0


if __name__ == "__main__":
    sys.exit(main())
