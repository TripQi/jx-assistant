# -*- coding: utf-8 -*-
"""VoiceType asr_server.py —— 小米 X-ASR 本地识别服务（OpenAI 兼容接口，端口 8320）。

与 SayIt/local_asr_server.py 同一套已验证方案：
  · X-ASR (Zipformer2 transducer int8) + sherpa-onnx
  · Silero VAD 按停顿分段（0.4s 断句）+ 超长段能量谷智能切分（单段≤20s）
  · POST /v1/audio/transcriptions (multipart WAV) → {"text": ...}
新增：GPU/CPU 自动探测（dml → cuda → cpu，探测结果只打日志，失败自动回落）。
"""
import io
import json
import math
import os
import multiprocessing
import select
import socket
import struct
import sys
import threading
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_BASE = os.path.dirname(os.path.abspath(__file__))
# ---------- 模型目录：与主程序「语音组件」共用同一份 ----------
# 同一套 X-ASR 模型（enc/dec/joiner/tokens）+ silero VAD 在主程序组件里已经有一份，
# 输入法不再重复囤 168MB。解析顺序：
#   1) VT_MODEL_DIR 环境变量（主程序拉起时注入＝组件 models 目录，最可靠）
#   2) exe 同目录 models（独立安装/自包含形态）
#   3) 仓库里的兄弟目录（开发形态：..\坚祥数字助理GO\组件\voice\models）
#   4) 上层目录里的组件（安装形态：<安装目录>\组件\voice\models）
def _resolve_model_dir():
    cands = []
    if os.environ.get("VT_MODEL_DIR"):
        cands.append(os.environ["VT_MODEL_DIR"])
    cands += [
        os.path.join(_BASE, "models"),
        # 输入法就住在组件里时（<组件>/voice/voicetype/），模型在上一级 models
        os.path.join(os.path.dirname(_BASE), "models"),
        os.path.join(os.path.dirname(_BASE), "坚祥数字助理GO", "组件", "voice", "models"),
        os.path.join(_BASE, "组件", "voice", "models"),
        os.path.join(os.path.dirname(_BASE), "组件", "voice", "models"),
    ]
    for d in cands:
        if os.path.isdir(os.path.join(d, "xasr")) and os.path.isfile(os.path.join(d, "silero_vad.onnx")):
            return d, cands
    return "", cands


_MODELS_ROOT, _MODEL_CANDS = _resolve_model_dir()
if not _MODELS_ROOT:
    raise RuntimeError("找不到模型目录（尝试过：%s）——可用环境变量 VT_MODEL_DIR 指定" % _MODEL_CANDS)
MODEL_DIR = os.path.join(_MODELS_ROOT, "xasr")
VAD_MODEL = os.path.join(_MODELS_ROOT, "silero_vad.onnx")
print(f"[vt-asr] 模型目录: {_MODELS_ROOT}", file=sys.stderr)
HOST, PORT = "127.0.0.1", 8320
MAX_SEG_SEC = 20.0

_recognizer = None
_vad = None
_lock = threading.Lock()


def detect_provider():
    """GPU/CPU 自动检测：看 onnxruntime 可用后端。
    sherpa-onnx 1.13.x 只认 'cuda'/'cpu' 字符串（'dml' 会静默回落），
    故仅当 CUDA 真正可用时才下传 provider；DirectML 待 sherpa-onnx 支持后接入。
    """
    import sherpa_onnx

    def one(pattern):
        import glob
        hits = sorted(glob.glob(str(MODEL_DIR + "\\**\\" + pattern), recursive=True))
        return hits[0] if hits else None

    files = dict(enc=one("encoder*.int8.onnx"), dec=one("decoder*.onnx"),
                 joi=one("joiner*.int8.onnx"), tok=one("tokens.txt"))
    if not all(files.values()):
        raise RuntimeError(f"xasr 模型文件不完整: {MODEL_DIR}")

    provider = "cpu"
    kwargs = {}
    try:
        import onnxruntime as ort
        avail = ort.get_available_providers()
        if "CUDAExecutionProvider" in avail:
            provider, kwargs = "cuda", {"provider": "cuda"}
    except Exception as e:  # noqa: BLE001
        print(f"[vt-asr] onnxruntime 探测失败，用 CPU: {e}", file=sys.stderr)

    rec = sherpa_onnx.OfflineRecognizer.from_transducer(
        encoder=files["enc"], decoder=files["dec"], joiner=files["joi"],
        tokens=files["tok"], num_threads=4, **kwargs)
    print(f"[vt-asr] 推理后端: {provider.upper()}", file=sys.stderr)
    return rec, provider


def get_recognizer():
    global _recognizer
    if _recognizer is not None:
        return _recognizer
    _recognizer, _provider = detect_provider()
    return _recognizer


def get_vad():
    global _vad
    if _vad is not None:
        return _vad
    import sherpa_onnx

    cfg = sherpa_onnx.VadModelConfig()
    cfg.silero_vad.model = VAD_MODEL
    cfg.silero_vad.min_silence_duration = 0.4
    cfg.silero_vad.min_speech_duration = 0.25
    cfg.silero_vad.max_speech_duration = 600  # 超长段由能量谷智能切，不劳 VAD 固定点硬切
    _vad = sherpa_onnx.VoiceActivityDetector(config=cfg, buffer_size_in_seconds=600)
    print("[vt-asr] silero VAD 已加载", file=sys.stderr)
    return _vad


def smart_hard_split(samples, sr, limit):
    """超长连续语音（无停顿）的智能切分：到 limit 回看 2.5s 找能量谷切。"""
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


def normalize_peak(x, target=None, max_gain_db=60.0, floor=1e-4):
    """峰值归一（只提升、不衰减），返回 (新信号, 实际增益 dB)。

    **target=0.35 与顺序纪律都来自产品组件的实测校准**（原话在
    组件/voice/asr_worker.py 的备份版里）：

      · X-ASR int8 对绝对音量敏感，引擎舒适区在峰值 0.33~0.55；
        小声（峰值 0.07）会错词，拉到 0.35 恢复；
      · 0.7 是引擎的舒适峰值，**不能**按 -3dBFS（约 0.7）以外的更响目标归一；
      峰值超过 0.7 会更响，反而让边界词劣化（"礼拜二"→"liba"）；
      · **切分必须在原始音频上跑**：整段归一会把底噪同倍放大、破坏 silero 的静音判定，
        导致漏字（"所以我们要先切分"曾被切丢前半）；
        增益只能在切分之后、识别之前，**逐段**做。

    所以本函数的调用点是"逐段"，不是"整段"。
    """
    import numpy as np

    if target is None:
        target = float(os.environ.get("VT_AGC_TARGET", "0.7"))
    if x.size == 0:
        return x, 0.0
    peak = float(np.max(np.abs(x)))
    if peak <= floor or peak >= target:
        return x, 0.0
    gain = min(target / peak, 10 ** (max_gain_db / 20.0))
    return x * gain, 20.0 * math.log10(gain)


def split_segments(samples, sr):
    import numpy as np

    vad = get_vad()
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
        limit = int(MAX_SEG_SEC * sr)
        if len(s) <= limit:
            segs.append((start, s))
            continue
        for piece in smart_hard_split(s, sr, limit):
            segs.append((start, piece))
            start += len(piece)
    if not segs:  # 全静音兜底
        limit = int(MAX_SEG_SEC * sr)
        for i in range(0, len(samples), limit):
            segs.append((i, samples[i:i + limit]))
    return segs


def transcribe_wav(wav_bytes):
    import numpy as np

    with wave.open(io.BytesIO(wav_bytes), "rb") as w:
        sr, n, ch = w.getframerate(), w.getnframes(), w.getnchannels()
        pcm = w.readframes(n)
    samples = np.frombuffer(pcm, dtype=np.int16)
    if ch > 1:
        samples = samples.reshape(-1, ch).mean(axis=1).astype(np.int16)
    f32 = samples.astype(np.float32) / 32768.0
    # 注意顺序（产品组件里实测踩过的坑）：
    #   ✗ 整段先归一 → 底噪同倍放大 → silero 静音判定被破坏 → 漏字
    #   ✓ 先在最原始的音频上 VAD 切段，再**逐段**做峰值归一，然后识别
    # 我第一版就是按 ✗ 写的，看到组件备份里的实测记录后改成 ✓。
    agc = not os.environ.get("VT_NO_AGC")
    # 极小声（峰值 < 0.02 ≈ -34dBFS）先整体提一级再切分：这种录音的底噪也极低，
    # 整体提一级不会污染 VAD，反而能避免 silero 把段首当静音丢掉（实测：1.5% 音量
    # 时"这"字会丢）。正常音量的录音**不走**这一步，严格按组件那边的纪律：
    # 原始音频切分 → 逐段增益（整段归一在正常场景会把底噪同倍放大，破坏 VAD）。
    if agc and f32.size:
        import numpy as _np
        peak = float(_np.max(_np.abs(f32)))
        if 1e-4 < peak < 0.02:
            f32, g0 = normalize_peak(f32)
            if g0 > 0.5:
                print(f"[vt-asr] 极小声：整体 +{g0:.1f}dB 后再切分", file=sys.stderr)
    # 整段推理必须串行：silero VAD 与识别引擎都是**共享的非线程安全实例**，
    # 而"中途段识别"和"松手后的最终全量识别"会并发打进来（ThreadingHTTPServer）。
    # 以前只锁了识别引擎、没锁 VAD，于是 VAD 被两个线程同时 accept_waveform 就炸：
    #   RuntimeError: NULL input supplied for input h
    #   ValueError: vector too long
    # 用户看到的就是"分段句子出来了，最终结果却报识别失败"（2026-11-03 实测）。
    with _lock:
        segs = split_segments(f32, sr)
        texts = []
        engine = get_recognizer()
        gains = []
        for _start, seg in segs:
            if agc:
                seg, g = normalize_peak(seg)
                if g > 0.5:
                    gains.append(g)
            stream = engine.create_stream()
            stream.accept_waveform(sr, seg)
            engine.decode_stream(stream)
            t = (stream.result.text or "").strip()
            if t:
                texts.append(t)
        if gains:
            print(f"[vt-asr] 段级增益: " + ", ".join(f"+{g:.1f}dB" for g in gains), file=sys.stderr)
    out = ""
    for t in texts:  # 英文/数字开头补空格，中文直连
        if out and t and t[0].isascii() and t[0].isalnum():
            out += " " + t
        else:
            out += t
    return out


def extract_wav_from_multipart(body: bytes) -> bytes:
    pos = body.find(b"RIFF")
    if pos < 0 or body[pos + 8:pos + 12] != b"WAVE":
        raise ValueError("multipart 里没找到 WAV 数据")
    (riff_size,) = struct.unpack_from("<I", body, pos + 4)
    return body[pos:pos + 8 + riff_size]


class ClientDisconnected(Exception):
    """The caller no longer owns an inference request."""


def _inference_worker(conn, transcribe=None, warmup=None):
    """Spawn-safe entry point; native models live only in this process."""
    try:
        if warmup is None:
            get_recognizer()
            get_vad()
        else:
            warmup()
        conn.send(("ready", None))
        while True:
            wav = conn.recv()
            try:
                text = (transcribe or transcribe_wav)(wav)
                conn.send(("ok", text))
            except Exception as exc:
                conn.send(("error", f"{type(exc).__name__}: {exc}"))
    except (EOFError, BrokenPipeError):
        pass
    finally:
        conn.close()


class InferenceWorker:
    """One reusable model process; cancellation discards its entire native state.

    Threads cannot interrupt sherpa/ONNX C++ calls. Only the process handle we
    created is terminated, never a PID lookup or another desktop/service process.
    The lock covers warmup, inference and reap, so queued requests cannot kill a
    different request's worker. Normal requests keep both models resident.
    """
    def __init__(self, transcribe=None, warmup=None):
        self._ctx = multiprocessing.get_context("spawn")
        self._gate = threading.Lock()
        self._closing = threading.Event()
        self._process = None
        self._conn = None
        self._ready = False
        self._transcribe = transcribe
        self._warmup = warmup

    def _discard(self):
        process, conn = self._process, self._conn
        self._process = self._conn = None
        self._ready = False
        if conn is not None:
            conn.close()
        if process is not None:
            if process.is_alive():
                process.terminate()
            process.join(timeout=2)
            if process.is_alive():
                process.kill()
                process.join(timeout=2)
            if process.is_alive():
                # Do not allow a second model process if OS termination failed.
                self._process = process
                raise RuntimeError("推理子进程无法终止")
            process.close()

    def _start(self):
        if self._process is not None and self._process.is_alive():
            if self._conn is None:
                raise RuntimeError("上轮推理子进程尚未终止")
            return
        self._discard()
        parent, child = self._ctx.Pipe()
        process = self._ctx.Process(
            target=_inference_worker,
            args=(child, self._transcribe, self._warmup), daemon=True)
        try:
            process.start()
        except BaseException:
            parent.close()
            child.close()
            raise
        child.close()
        self._conn, self._process = parent, process

    def _receive(self, disconnected):
        while True:
            if self._closing.is_set():
                raise RuntimeError("识别服务正在关闭")
            if disconnected():
                raise ClientDisconnected()
            if self._conn.poll(0.05):
                return self._conn.recv()
            if not self._process.is_alive():
                raise RuntimeError("推理子进程意外退出")

    def transcribe(self, wav, disconnected):
        # A disconnected queued request must not interrupt the current owner.
        while not self._gate.acquire(timeout=0.05):
            if disconnected():
                raise ClientDisconnected()
        try:
            if self._closing.is_set():
                raise RuntimeError("识别服务正在关闭")
            if disconnected():
                raise ClientDisconnected()
            try:
                self._start()
                if not self._ready:
                    kind, _ = self._receive(disconnected)
                    if kind != "ready":
                        raise RuntimeError("推理子进程未就绪")
                    self._ready = True
                if disconnected():
                    raise ClientDisconnected()
                self._conn.send(wav)
                kind, value = self._receive(disconnected)
                if kind != "ok":
                    raise RuntimeError(value)
                return value
            except BaseException:
                self._discard()
                raise
        finally:
            self._gate.release()

    def close(self):
        self._closing.set()
        with self._gate:
            self._discard()


_worker = InferenceWorker()


class ASRServer(ThreadingHTTPServer):
    daemon_threads = True
    block_on_close = False

    def server_close(self):
        super().server_close()
        _worker.close()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass  # 静默访问日志，只留业务日志

    def _json(self, code, obj):
        data = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path.rstrip("/") in ("/", "/health"):
            self._json(200, {"ok": True, "model": "xasr"})
        else:
            self._json(404, {"error": {"message": "not found"}})

    def _disconnected(self):
        # Peek only: consuming bytes would corrupt a pipelined/keep-alive request.
        # HTTP clients must close the socket on cancellation (Go context does).
        try:
            readable, _, _ = select.select([self.connection], [], [], 0)
            return bool(readable) and self.connection.recv(1, socket.MSG_PEEK) == b""
        except (OSError, ValueError):
            return True

    def do_POST(self):
        if not self.path.rstrip("/").endswith("/audio/transcriptions"):
            self._json(404, {"error": {"message": f"unknown route: {self.path}"}})
            return
        try:
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length)
            wav = extract_wav_from_multipart(body)
            import time
            t0 = time.time()
            text = _worker.transcribe(wav, self._disconnected)
            ms = int((time.time() - t0) * 1000)
            print(f"[vt-asr] {len(wav)//1024}KB → {len(text)}字 / {ms}ms", file=sys.stderr)
            self._json(200, {"text": text})
        except (ClientDisconnected, BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
            self.close_connection = True
            print("[vt-asr] 客户端已断开，本轮推理已停止", file=sys.stderr)
        except Exception as e:  # noqa: BLE001
            import traceback
            traceback.print_exc()
            try:
                self._json(500, {"error": {"message": f"{type(e).__name__}: {e}", "type": "server_error"}})
            except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
                self.close_connection = True


def main():
    # Binding first avoids loading another model copy when the port is occupied.
    srv = ASRServer((HOST, PORT), Handler)
    try:
        print(f"[vt-asr] 就绪 http://{HOST}:{PORT}/v1/audio/transcriptions", file=sys.stderr)
        # Models warm up on the first live request in a cancellable spawn worker.
        srv.serve_forever()
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
