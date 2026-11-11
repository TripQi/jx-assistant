# -*- coding: utf-8 -*-
"""title_worker.py —— 「AI 自动会话标题」组件的后端 worker（C 形态，帧协议）。

节奏（用户拍板：起码 3 轮一改）：
  · 第 1 轮结束（首条用户消息处理完）命名一次；
  · 之后每满 3 轮（第 4、7、10…轮结束）修订一次——按用户消息条数 n 判定，
    n % 3 == 1 才动手；中间的轮次完全不动标题，也不调模型（省 token）。

输入极简：每次只送「当前标题 + 最近 3 条用户消息」（各截 400 字）——旧标题
本身就是累积状态的载体（核心段「最近主题|最初主题」近期在前——侧栏窄，
靠前的字先被看到；冒号后完整描述），每步只看增量；多条一起看，主题延续
还是漂移判断得更稳。

模型请求是组件机制的通用服务：worker 经组件桥的反向调用（host 帧）调主程序
POST /api/llm/ask——一次发送、一次输出（单轮，不与模型多轮对话），不进会话
上下文；worker 自身不持有任何模型密钥（密钥与路由由主程序统一管理，该端点
向全部组件开放，git 面板摘要同款路径）。

钉住：用户手动改过标题的会话（主程序 PATCH 打钉）不再自动改：改标题请求带
autoTitle 标记，主程序对钉住会话回 409，本 worker 静默跳过。

帧协议（见 组件/README.md）——stdin 逐行收：
  {"type":"event","id":..,"event":"run.end","data":{"sessionID":..,"status":..}}  钩子事件
  {"type":"hostres","id":..,"status":200,"body":{..}}                            反向调用应答
worker 发：
  {"type":"host","id":..,"method":"GET|POST|PATCH","path":"/api/..","body":{..}}  反向调用
通知型事件不需要回 decision。全部业务在工作线程异步串行：同一会话只保留
最新一次待处理（去抖），任何失败只记 stderr（桥转主程序日志），绝不影响
主流程。
"""
import json
import sys
import threading

try:  # Windows 管道下默认编码不确定，显式钉死 UTF-8（桥按 UTF-8 逐行读写）
    sys.stdin.reconfigure(encoding="utf-8", errors="replace")
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

# ---------- 提示词（增量演化规则在这里，改规则只改本组件） ----------

TITLE_SYSTEM_PROMPT = """你是会话标题撰写员。根据会话的当前标题与最近的几条用户消息（最后一条最新），输出该会话的新标题。标题显示在侧栏会话列表：开头一小段是列表里一眼可见的核心（列表窄、越靠前的字越先被看到），冒号后是完整描述（鼠标悬停可见全文，也用于搜索）。
输出格式：「核心：描述」
- 核心（冒号前，8-12 个字）：格式「最近主题|最初主题」，半角竖线 | 分隔、两侧不加空格，示例：发票问题|报销流程。单一主题时只写一段 4-8 个字、无竖线。竖线后的最初主题在整个会话中保持不变；最新消息仍是前段主题的延续就保留原核心；最新消息引入新主题时只更新前段——单段升两段、两段换前段，不叠加第三段。
- 描述（冒号后）：一句话概括这个会话从开始到现在在做什么（含最初诉求与最近进展），信息密度要高、方便搜索；长度不设上限，但尽量精炼，一般不超过 40 个字；随会话进展重写（吸收新信息、删掉已被覆盖的旧表述）。
- 用用户消息的主要语言书写；中文用全角冒号「：」，英文用半角「: 」。
- 首次命名（当前标题为空）：核心段概括用户消息的核心意图，描述段概括用户消息的完整任务。
- 不要引号、不要书名号、不要句末标点、不要解释、不要任何多余文字，只输出标题本身。"""

# 改写节奏：用户消息条数 n 满足 n % CADENCE == 1 才动手——
# 首轮（n=1）命名，之后第 4、7、10…轮（间隔恰 3 轮）修订。
CADENCE = 3
RECENT_MSGS = 3            # 每次随标题一起送的最近用户消息条数（覆盖一个修订周期的增量）
# 输入截断（字符）：当前标题与每条用户消息各按此截断。
PER_MSG_CHARS = 400

# ---------- 帧协议 ----------

_out_lock = threading.Lock()
_waiters = {}          # host 帧 id -> [threading.Event, hostres dict]
_waiters_lock = threading.Lock()
_seq = 0


def _next_id():
    global _seq
    with _waiters_lock:
        _seq += 1
        return "h%d" % _seq


def log(msg):
    print("[title-worker] %s" % msg, file=sys.stderr, flush=True)


def send_frame(obj):
    line = json.dumps(obj, ensure_ascii=False)
    with _out_lock:
        sys.stdout.write(line + "\n")
        sys.stdout.flush()


def host_call(method, path, body=None, timeout=60):
    """反向调用主程序 API（桥侧 60 秒兜底，这里同档）。返回 hostres dict 或 None。"""
    hid = _next_id()
    ev = threading.Event()
    with _waiters_lock:
        _waiters[hid] = [ev, None]
    try:
        send_frame({"type": "host", "id": hid, "method": method, "path": path,
                    "body": body if body is not None else {}})
        if not ev.wait(timeout):
            log("反向调用超时: %s %s" % (method, path))
            return None
        with _waiters_lock:
            return _waiters.pop(hid)[1]
    finally:
        with _waiters_lock:
            _waiters.pop(hid, None)


def dispatch_hostres(frame):
    hid = frame.get("id")
    with _waiters_lock:
        rec = _waiters.get(hid)
    if rec:
        rec[1] = frame
        rec[0].set()


# ---------- 业务 ----------

_pending = {}            # sessionID -> True（去抖：同会话只留一次待处理）
_pending_lock = threading.Lock()
_wake = threading.Condition()


def enqueue(sid):
    with _pending_lock:
        _pending[sid] = True
    with _wake:
        _wake.notify()


def take_one():
    with _wake:
        while not _pending:
            _wake.wait()
    with _pending_lock:
        sid = next(iter(_pending))
        del _pending[sid]
    return sid


def clean_title(raw):
    # 只清洗、不截断：长度靠提示词引导（核心段 8-12 字、描述尽量精炼），模型
    # 偶发超长由主程序 PATCH 的 truncateRunes(_, 60) 统一兜底——本体管存储
    # 规则，组件只管引导。
    t = (raw or "").strip()
    wrap = "\"'‘’“”「」『』《》*#`"
    while t and t[0] in wrap:
        t = t[1:]
    while t and t[-1] in wrap:
        t = t[:-1]
    t = " ".join(t.split())  # 折叠换行与多余空白
    return t


def build_user_prompt(old_title, recents):
    """输入极简：当前标题 + 最近几条用户消息（从早到晚、最后一条最新）——
    旧标题本身就是累积状态的载体（核心段「最近主题|最初主题」近期在前、
    冒号后完整描述），每步只看最近的增量；多条一起看，主题延续还是漂移
    判断得更稳。"""
    lines = ["当前标题：" + (old_title or "（无）"),
             "最近 %d 条用户消息（从早到晚，最后一条最新，过长已截断）：" % len(recents)]
    for i, t in enumerate(recents, 1):
        t = " ".join(t.split())
        if len(t) > PER_MSG_CHARS:
            t = t[:PER_MSG_CHARS] + "…"
        lines.append("[%d] %s" % (i, t))
    return "\n".join(lines)


_generated_at = {}  # sessionID -> 上次生成标题时的（非插话）用户消息条数：同 n 不重复生成
_gen_lock = threading.Lock()


def process_session(sid):
    try:
        mr = host_call("GET", "/api/sessions/%s/messages?role=user" % sid)
        if not mr or mr.get("status") != 200:
            return  # 会话不存在/服务忙：静默（下一轮会再试）
        texts = [m.get("text") or "" for m in ((mr.get("body") or {}).get("messages") or [])]
        texts = [t for t in texts if t.strip()]
        n = len(texts)  # 正常轮数（messages 端点已排除插话）
        if n == 0 or n % CADENCE != 1:
            return  # 未到改写轮：首轮（n=1）命名，之后每满 3 轮（n=4,7,…）修订
        with _gen_lock:
            if _generated_at.get(sid) == n:
                return  # 该轮已生成过（期间只有插话进来）：不重复调模型
            _generated_at[sid] = n

        info = host_call("GET", "/api/sessions/" + sid)
        if not info or info.get("status") != 200:
            return
        meta = (info.get("body") or {}).get("meta") or {}
        if meta.get("titlePinned"):
            return  # 用户手动命名钉住：不自动改（主程序侧 409 也会兜底）
        old_title = (meta.get("title") or "").strip()

        ar = host_call("POST", "/api/llm/ask", {
            "session": sid, "system": TITLE_SYSTEM_PROMPT,
            "purpose": "title",
            "user": build_user_prompt(old_title, texts[-RECENT_MSGS:])})
        if not ar or ar.get("status") != 200:
            with _gen_lock:
                _generated_at.pop(sid, None)  # 失败回退：这一轮下次还能重试
            log("生成标题失败: %s" % (ar and ar.get("status")))
            return
        title = clean_title((ar.get("body") or {}).get("text") or "")
        if not title or title == old_title:
            return  # 空/未变：不落库

        pr = host_call("PATCH", "/api/sessions/" + sid,
                       {"title": title, "autoTitle": True})
        if pr and pr.get("status") == 200:
            log("会话 %s 标题 → %s（第 %d 轮）" % (sid, title, n))
        elif pr and pr.get("status") == 409:
            pass  # 钉住拒绝：正常静默
        else:
            log("改标题失败: %s" % (pr and pr.get("status")))
    except Exception as e:  # 任何异常都不能带崩 worker
        log("处理会话 %s 异常: %r" % (sid, e))


def worker_loop():
    while True:
        sid = take_one()
        process_session(sid)


# ---------- 主线程：读 stdin 分发帧 ----------

def main():
    threading.Thread(target=worker_loop, daemon=True).start()
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            f = json.loads(line)
        except Exception:
            continue
        t = f.get("type")
        if t == "event" and f.get("event") == "run.end":
            sid = (f.get("data") or {}).get("sessionID")
            if sid:
                enqueue(sid)  # done/stopped/error 都处理：用户消息已存在即该反映
        elif t == "hostres":
            dispatch_hostres(f)
        elif t == "req":
            # 本 worker 未声明 call 端点；兜底应答防误配
            send_frame({"type": "res", "id": f.get("id"), "body": {"ok": True}})


if __name__ == "__main__":
    main()
