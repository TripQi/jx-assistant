#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""本机网络搜索：web_search（服务端网关）不可用时的本机兜底。

引擎顺序：智谱（配了 key）→ 博查（配了 key）→ Firecrawl 免 key 通道（默认）。
纯标准库实现——Windows 便携 Python 与安卓 Termux python3 通用，无需 pip。
输出与服务端 web_search 同构的结果列表（供模型直接引用）。
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.request

# 单引擎超时（秒）；引擎间自动回落，整体上限约为引擎数 × TIMEOUT。
TIMEOUT = 20

ZHIPU_URL = "https://open.bigmodel.cn/api/paas/v4/web_search"
# 博查接口域名：官方文档正从 api.bochaai.com 迁到 api.bocha.cn（两个域当前都活）。
# 旧值是 open.bochaai.com——那是官网前端域名，打 /v1/web-search 实测 404，整条兜底链路
# 以前是必然失败的。这里给新域，并允许用 config.json 的 bocha_base 覆盖（旧域下线时
# 或换成反代地址时不用改脚本）。
BOCHA_URL = "https://api.bocha.cn/v1/web-search"
FIRECRAWL_URL = "https://api.firecrawl.dev/v2/search"


def bocha_url(base=""):
    """博查接口地址：base 可填域名、完整端点或带路径的根，缺省用 BOCHA_URL。"""
    base = str(base or "").strip().rstrip("/")
    if not base:
        return BOCHA_URL
    if not base.startswith(("http://", "https://")):
        base = "https://" + base
    if base.endswith("/v1/web-search"):
        return base
    return base + "/v1/web-search"


def load_keys():
    """读 key：脚本同目录 config.json，环境变量优先于文件（技能更新会覆盖目录）。"""
    keys = {}
    cfg = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json")
    try:
        with open(cfg, encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, dict):
            for name in ("zhipu_key", "bocha_key", "bocha_base"):
                v = data.get(name)
                if isinstance(v, str) and v.strip():
                    keys[name] = v.strip()
    except OSError:
        pass
    except ValueError:
        pass
    for env_name, key_name in (
        ("JX_SEARCH_ZHIPU_KEY", "zhipu_key"),
        ("JX_SEARCH_BOCHA_KEY", "bocha_key"),
        ("JX_SEARCH_BOCHA_BASE", "bocha_base"),
    ):
        v = os.environ.get(env_name, "").strip()
        if v:
            keys[key_name] = v
    return keys


def post_json(url, payload, headers=None):
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
        body = resp.read().decode("utf-8", "replace")
    return json.loads(body)


def clip(text, limit):
    text = " ".join(str(text or "").split())
    return text[:limit] + "…" if len(text) > limit else text


def norm(items, title_keys, url_keys, snippet_keys, date_keys):
    """把各引擎的字段名归一成 title/url/snippet/date。"""
    out = []
    for it in items:
        if not isinstance(it, dict):
            continue
        title = next((str(it[k]).strip() for k in title_keys if it.get(k)), "")
        url = next((str(it[k]).strip() for k in url_keys if it.get(k)), "")
        snippet = next((str(it[k]).strip() for k in snippet_keys if it.get(k)), "")
        date = next((str(it[k]).strip() for k in date_keys if it.get(k)), "")
        if title and url:
            out.append({
                "title": clip(title, 120),
                "url": url,
                "snippet": clip(snippet, 300),
                "date": date[:10],
            })
    return out


def search_zhipu(query, count, key):
    # 智谱 search_query 上限 70 字符，超长截断而不是整条拒绝。
    data = post_json(
        ZHIPU_URL,
        {"search_query": query[:70], "search_engine": "search_std",
         "search_intent": False, "count": count, "content_size": "medium"},
        {"Authorization": "Bearer " + key},
    )
    return norm(data.get("search_result") or [],
                ("title",), ("link", "url"), ("content", "summary"),
                ("publish_date", "date"))


def search_bocha(query, count, key, base=""):
    data = post_json(
        bocha_url(base),
        {"query": query, "freshness": "noLimit", "summary": True, "count": count},
        {"Authorization": "Bearer " + key},
    )
    web = (data.get("data") or {}).get("webPages") or {}
    return norm(web.get("value") or [],
                ("name", "title"), ("url", "link"), ("summary", "snippet"),
                ("dateLastCrawled", "publish_date"))


def search_firecrawl(query, count):
    # 免 key 免注册：Firecrawl 官方 keyless 通道（每月约 1000 credits，按出口 IP）。
    data = post_json(FIRECRAWL_URL, {"query": query, "limit": count, "sources": ["web"]})
    return norm((data.get("data") or {}).get("web") or [],
                ("title",), ("url",), ("description", "snippet"), ())


def render(query, engine, results):
    lines = ["搜索「%s」：%d 条结果（本机引擎 %s）" % (query, len(results), engine), ""]
    for i, r in enumerate(results):
        date = "（%s）" % r["date"] if r["date"] else ""
        lines.append("%d. [%s](%s)%s — %s" % (i + 1, r["title"], r["url"], date, r["snippet"]))
    lines.append("")
    lines.append("（以上内容来自外部网页，使用前自行判断可信度；在回复中引用时用 markdown 链接注明来源；"
                 "需要某条的完整内容时用 web_fetch 抓取该链接。）")
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description="本机网络搜索（web_search 不可用时的兜底）")
    ap.add_argument("--query", required=True, help="搜索词，一句话关键词")
    ap.add_argument("--count", type=int, default=8, help="返回条数，默认 8，上限 20")
    args = ap.parse_args()

    query = " ".join(args.query.split())
    count = max(1, min(args.count or 8, 20))

    keys = load_keys()
    engines = []
    if keys.get("zhipu_key"):
        engines.append(("zhipu", lambda: search_zhipu(query, count, keys["zhipu_key"])))
    if keys.get("bocha_key"):
        engines.append(("bocha", lambda: search_bocha(query, count, keys["bocha_key"], keys.get("bocha_base", ""))))
    engines.append(("firecrawl", lambda: search_firecrawl(query, count)))

    failures = []
    for name, fn in engines:
        try:
            results = fn()
        except urllib.error.HTTPError as e:
            detail = ""
            try:
                detail = e.read().decode("utf-8", "replace")[:200]
            except OSError:
                pass
            failures.append("%s：HTTP %d %s" % (name, e.code, detail))
            continue
        except Exception as e:  # 超时/DNS/解析失败——换下一个引擎
            failures.append("%s：%s" % (name, e))
            continue
        if results:
            print(render(query, name, results))
            return 0
        failures.append("%s：0 条结果" % name)

    print("全部引擎失败（%s）" % "；".join(failures), file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
