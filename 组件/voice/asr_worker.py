# -*- coding: utf-8 -*-
"""asr_worker.py —— 本地语音识别统一 worker（语音模型评估/听写后端）。

协议：每行一个 JSON 请求（stdin），每行一个 JSON 应答（stdout）。
  {"cmd":"load","model":"xasr","device":"cpu"}
      → {"ok":true,"event":"loaded","model":...,"load_ms":...}
  {"cmd":"transcribe","wav_b64":"<16k mono s16le WAV 的 base64>"}
      → {"ok":true,"text":"...","audio_ms":...,"infer_ms":...,"rtf":...}
  流式（一行请求一行应答，Go 侧哑桥转发）：
  {"cmd":"stream_start"} → {"ok":true}
  {"cmd":"stream_chunk","pcm_b64":"<int16 PCM 块>"} 
      → {"ok":true,"event":"partial","text":"灰字实时增量"}
      → 端点触发（用户停顿）时 {"ok":true,"event":"final","text":"离线模型精修后的整句（黑字）"}
  {"cmd":"stream_end"} → {"ok":true,"event":"done"}（尾段若有先回一条 final）
  出错 → {"ok":false,"err":"..."}

stdout 纪律：协议应答走原 fd1 的独立句柄；启动时把 sys.stdout 与 fd1 全部
改指向 stderr（os.dup2），保证 onnxruntime/llama 子进程的 C 层 printf 与
引擎日志全部落在 stderr，不污染 JSON 流。Go 侧（voiceasr.go）把 stderr
当日志收。

落位（组件自包含）：本脚本与模型都在 组件/voice/ 里——
  组件/voice/asr_worker.py   本文件
  组件/voice/models/xasr/    离线模型（草稿与定稿共用；流式模型已删省 163MB）
运行时用共享便携 Python（技能/_runtime/python，随仓库分发；缺失依赖只报错，不自动安装）。
（评估期的 paraformer/sensevoice/qwen 分支已删——结论见 git 历史 054c19e881。）
"""
import base64
import glob
import io
import json
import os
import sys
import time
import wave
from pathlib import Path


# ---------- 识别内核：VAD 分段 + 每段增益 ----------
#
# 从 backup/voice-20261103 分支取回（2026-11-03）。只取这一段识别内核：
# 悬浮窗（overlay.html / port.txt / hotkeys 端点 / client.js 的 overlay 保活）
# 一概不恢复——那次组件化改造引入的问题已由原样回退处理。
#
# 为什么必须切分再识别（原注释保留，均为实测结论）：
#   · X-ASR encoder 约 24s 上限，超出必须切；固定点硬切会切在字中间丢字，
#     所以超长段回看 2.5s 找能量谷（换气处）切。
#   · 切分必须在【原始音频】上跑——整段峰值归一会把底噪同倍放大，破坏 silero
#     的静音判定导致漏字（"所以我们要先切分"曾被切丢前半）；
#     增益只能在切分之后、识别之前，**逐段**做。
#
# 增益目标峰值：按产品结论取 0.7（可用环境变量 VOICE_AGC_TARGET 覆盖）。
# 历史实测记录（原注释原文，留档备查）："引擎舒适区实测在峰值 0.33~0.55，
# 小声（峰值 0.07）会错词、拉到 0.35 恢复；峰值到 0.7 以上过响，反而让边界词
# 劣化（'礼拜二'→'liba'，实测对照）"——若将来发现边界词劣化，把默认值改回 0.35。

_vad = None
_GAIN_TARGET = float(os.environ.get("VOICE_AGC_TARGET", "0.7"))
_MAX_SEG_SEC = 20.0  # X-ASR encoder 实测约 24s 上限，留余量


def _get_vad(models_root):
    """silero VAD（惰性单例）。按停顿切分：0.4s 静音断句、<0.25s 碎语丢弃。"""
    global _vad
    if _vad is None:
        import sherpa_onnx
        cfg = sherpa_onnx.VadModelConfig()
        cfg.silero_vad.model = str(models_root / "silero_vad.onnx")
        cfg.silero_vad.min_silence_duration = 0.4   # 与前端/切段器一致的断句阈值
        cfg.silero_vad.min_speech_duration = 0.25    # "嗯"级碎语不单独成段
        cfg.silero_vad.max_speech_duration = 600     # 超长段由能量谷智能切
        _vad = sherpa_onnx.VoiceActivityDetector(config=cfg, buffer_size_in_seconds=600)
    return _vad


def _normalize_gain(samples, target=None):
    """数字增益：峰值归一化（只放大不衰减）。X-ASR int8 对绝对音量敏感。"""
    import numpy as np
    t = _GAIN_TARGET if target is None else target
    peak = float(np.max(np.abs(samples))) if len(samples) else 0.0
    if 1e-4 < peak < t:
        samples = samples * (t / peak)
    return samples


def _smart_hard_split(samples, sr, limit):
    """超长连续语音（一口气说不停无停顿）的智能切分：到 limit 回看 2.5s
    找能量谷（换气处）切，避免固定点硬切切在字中间导致丢字。"""
    import numpy as np
    if len(samples) <= limit:
        return [samples]
    frame = int(sr * 0.02)
    n = len(samples) // frame
    if n == 0:
        return [samples]
    rms = np.sqrt(np.mean(samples[:n * frame].reshape(n, frame) ** 2, axis=1))
    lookback = int(sr * 2.5)
    valley_frames = max(1, int(sr * 0.16) // frame)
    pieces, start = [], 0
    while len(samples) - start > limit:
        f_hi = (start + limit) // frame
        f_lo = max((start + limit - lookback) // frame, start // frame + 1)
        if f_hi - f_lo >= valley_frames:
            win = np.convolve(rms[f_lo:f_hi], np.ones(valley_frames), "valid")
            best_f = f_lo + int(np.argmin(win)) + valley_frames // 2
            cut = best_f * frame
        else:
            cut = start + limit
        pieces.append(samples[start:cut])
        start = cut
    pieces.append(samples[start:])
    return pieces


def _split_segments(samples, sr, models_root):
    """VAD 按停顿切段（在原始音频上跑）。每段增益由调用方在识别前做。"""
    import numpy as np
    try:
        vad = _get_vad(models_root)
    except Exception as e:  # noqa: BLE001 —— VAD 文件缺失/加载失败：整段直送保底
        print(f"[voice] VAD 不可用，退化为整段识别: {e}", file=sys.stderr)
        limit = int(_MAX_SEG_SEC * sr)
        return [(0, samples[i:i + limit]) for i in range(0, len(samples), limit)] if len(samples) else []
    vad.reset()
    for i in range(0, len(samples), 512):  # silero 必须按窗口分块流式喂
        vad.accept_waveform(samples[i:i + 512])
    vad.flush()
    segs = []
    while not vad.empty():
        seg = vad.front
        s = np.array(seg.samples, copy=True)
        start = int(getattr(seg, "start", 0))
        vad.pop()
        limit = int(_MAX_SEG_SEC * sr)
        if len(s) <= limit:
            segs.append((start, s))
            continue
        for piece in _smart_hard_split(s, sr, limit):
            segs.append((start, piece))
            start += len(piece)
    if not segs:  # 全静音兜底
        limit = int(_MAX_SEG_SEC * sr)
        return [(i, samples[i:i + limit]) for i in range(0, len(samples), limit)] if len(samples) else []
    return segs


def _join_texts(texts):
    """跨段拼接：英文/数字开头补空格，中文直连。"""
    out = ""
    for t in texts:
        if out and t and t[0].isascii() and t[0].isalnum():
            out += " " + t
        else:
            out += t
    return out


def main():
    root = Path(__file__).resolve().parent  # 组件/voice：worker 与 models/ 同级（自包含）
    models_root = root / "models"

    # ---- stdout 纪律：fd1 留给协议，其余全部转 stderr ----
    proto = os.fdopen(os.dup(1), "w", encoding="utf-8")
    os.dup2(2, 1)
    sys.stdout = sys.stderr

    def reply(obj):
        proto.write(json.dumps(obj, ensure_ascii=False) + "\n")
        proto.flush()

    state = {"model": None, "engine": None}

    def close_engine():
        eng = state.get("engine")
        if eng is None:
            return
        try:
            if hasattr(eng, "shutdown"):
                eng.shutdown()
        except Exception:
            pass
        state["engine"] = None
        state["model"] = None

    # ---------- 运行时依赖检测（只在装载引擎时检查） ----------
    def ensure_deps():
        """共享运行时也服务 Office/PDF/OCR，不能在识别时擅自安装或修复包。"""
        missing = []
        try:
            import sherpa_onnx
            if not hasattr(sherpa_onnx, "OfflineRecognizer"):
                raise ImportError("缺 OfflineRecognizer，包可能损坏或版本不兼容")
        except Exception as e:
            missing.append(("sherpa-onnx", str(e)))
        try:
            import numpy  # noqa: F401
        except Exception as e:
            missing.append(("numpy", str(e)))
        if missing:
            packages = " ".join(name for name, _ in missing)
            detail = "；".join(f"{name}: {reason}" for name, reason in missing)
            raise RuntimeError(
                f"语音依赖缺失或不可用：{detail}。未自动修改共享运行时。"
                "请通过技能管理器修复语音依赖，或由维护者确认共享依赖兼容性后手动执行："
                f'"{sys.executable}" -m pip install --upgrade {packages}'
                "（当前要求 sherpa-onnx 提供 OfflineRecognizer，numpy 可正常导入）")

    # ---------- 分片模型拼回 ----------
    def ensure_parts(xasr_dir):
        """GitHub 单文件 100MB 限制：X-ASR 的 encoder（153MB）以 *.onnx.partN 分片入库，
        首次加载时拼回整体文件；整体文件已存在（安装包分发 / 软著版仓库）就直接用。
        拼回走临时文件 + os.replace，中断不会留下半截模型。"""
        if not xasr_dir.is_dir():
            return
        stems = sorted({p.name.rsplit(".part", 1)[0] for p in xasr_dir.glob("*.onnx.part*")})
        for stem in stems:
            target = xasr_dir / stem
            if target.exists():
                continue
            parts = sorted(xasr_dir.glob(stem + ".part*"),
                           key=lambda q: int(q.name.rsplit(".part", 1)[1]))
            tmp = target.with_name(target.name + ".tmp")
            with open(tmp, "wb") as out:
                for q in parts:
                    with open(q, "rb") as f:
                        for chunk in iter(lambda: f.read(1 << 20), b""):
                            out.write(chunk)
            os.replace(tmp, target)
            print(f"[voice] 分片模型已拼回：{target.name}"
                  f"（{target.stat().st_size // (1 << 20)}MB）", file=sys.stderr)

    # ---------- 加载 ----------
    def load_engine(model, device):
        """装好并返回离线引擎（不回话、不动 state——load 与流式精修共用）。"""
        ensure_deps()
        if model == "xasr":
            # 小米 X-ASR：Zipformer2 transducer，中英双语、自带标点与大小写，
            # 离线/流式一体（这里用离线档），int8 约 170MB
            import sherpa_onnx
            d = models_root / "xasr"
            ensure_parts(d)

            def one(pattern):
                hits = sorted(glob.glob(str(d / "**" / pattern), recursive=True))
                return hits[0] if hits else None

            enc, dec, joi, tok = (one("encoder*.int8.onnx"), one("decoder*.onnx"),
                                  one("joiner*.int8.onnx"), one("tokens.txt"))
            if not (enc and dec and joi and tok):
                raise RuntimeError(f"xasr 模型文件不完整：{d}")
            return sherpa_onnx.OfflineRecognizer.from_transducer(
                encoder=enc, decoder=dec, joiner=joi, tokens=tok, num_threads=4)
        raise RuntimeError(f"未知模型: {model}")

    def load(model, device):
        t0 = time.time()
        close_engine()
        state["engine"] = load_engine(model, device)
        state["model"] = model
        reply({"ok": True, "event": "loaded", "model": model, "load_ms": int((time.time() - t0) * 1000)})

    # ---------- 转录 ----------
    def transcribe(wav_b64):
        # 哑管桥（componentbridge.go）只透传一行一答：引擎装载由 worker 自理，
        # 静默装载（load() 会多发一条 loaded 应答，打乱一行一答）
        if state.get("engine") is None:
            state["engine"] = load_engine("xasr", "cpu")
            state["model"] = "xasr"
        engine = state["engine"]
        raw = base64.b64decode(wav_b64)
        with wave.open(io.BytesIO(raw), "rb") as w:
            sr = w.getframerate()
            n = w.getnframes()
            ch = w.getnchannels()
            sw = w.getsampwidth()
            pcm = w.readframes(n)
        audio_ms = int(n / sr * 1000)
        import numpy as np
        samples = np.frombuffer(pcm, dtype=np.int16)
        if ch > 1:
            samples = samples.reshape(-1, ch).mean(axis=1).astype(np.int16)
        t0 = time.time()
        f32 = samples.astype(np.float32) / 32768.0

        # 极小声（峰值 < 0.02 ≈ -34dBFS）先整体提一级再做 VAD：
        # 这种录音的底噪也极低，整体提级不会污染静音判定；不提的话 silero 会把
        # 段首当静音切掉——实测 1.5%/0.8% 音量时"这"字会丢（备份版原样即如此）。
        # 正常音量**不走**这一步，严格保留"原始音频切分"的实测纪律。
        import numpy as _np
        _peak = float(_np.max(_np.abs(f32))) if f32.size else 0.0
        if 1e-4 < _peak < 0.02:
            before = _peak
            f32 = _normalize_gain(f32)
            print(f"[voice] 极小声(峰值 {before:.4f})：整体提级后再切分", file=sys.stderr)

        # 识别内核：先 VAD 切分（原始音频）→ 每段各自增益 → 逐段识别 → 拼接
        texts = []
        for _start, seg in _split_segments(f32, sr, models_root):
            seg = _normalize_gain(seg)
            stream = engine.create_stream()
            stream.accept_waveform(sr, seg)
            engine.decode_stream(stream)
            t = (stream.result.text or "").strip()
            if t:
                texts.append(t)
        text = _join_texts(texts)
        infer_ms = int((time.time() - t0) * 1000)
        reply({"ok": True, "text": text, "audio_ms": audio_ms,
               "infer_ms": infer_ms, "rtf": round(infer_ms / max(audio_ms, 1), 3),
               "model": state["model"], "sampwidth": sw})

    # ---------- 主循环 ----------
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
            if req.get("cmd") == "load":
                load(req.get("model", ""), req.get("device", "cpu"))
            elif req.get("cmd") == "transcribe":
                transcribe(req.get("wav_b64") or req.get("wavB64") or "")
            elif req.get("cmd") == "shutdown":
                close_engine()
                reply({"ok": True})
                return
            else:
                reply({"ok": False, "err": f"未知命令: {req.get('cmd')}"})
        except Exception as e:  # noqa: BLE001 —— worker 必须活着回错误，不能崩
            import traceback
            traceback.print_exc()
            reply({"ok": False, "err": f"{type(e).__name__}: {e}"})
        proto.flush()


if __name__ == "__main__":
    main()
