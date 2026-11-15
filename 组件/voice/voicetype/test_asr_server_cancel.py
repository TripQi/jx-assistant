"""Isolated fake-inference regression; no models, real service or fixed port.

Run with the bundled Python; --server accepts the preserved pre-fix source for RED.
All generated files are under .jxai/tmp; owned workers are closed in finally.
"""
import argparse
import http.client
import importlib
import io
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
import threading
import time
import wave
from http.server import ThreadingHTTPServer

# Importing the chosen server (including in Windows spawn) leaves no source-tree
# __pycache__ artifacts. All other generated files use the task temp directory.
sys.dont_write_bytecode = True
os.environ["PYTHONDONTWRITEBYTECODE"] = "1"


def fake_warmup():
    pass


def fake_transcribe(wav):
    root = Path(os.environ["VT_CANCEL_TEST_DIR"])
    pid = os.getpid()
    if b"SLOW" not in wav:
        return f"ok:{pid}"
    deadline = time.monotonic() + 6
    while time.monotonic() < deadline:
        if (root / "stop").exists():
            return "stopped"
        (root / "heartbeat").write_text(f"{pid}:{time.monotonic_ns()}", encoding="ascii")
        time.sleep(0.02)
    (root / "completed").write_text(str(pid), encoding="ascii")
    return "late result"


def wait_for(predicate, timeout=10):
    until = time.monotonic() + timeout
    while time.monotonic() < until:
        if predicate():
            return
        time.sleep(0.02)
    raise AssertionError("timed out waiting for fake inference")


def wav_bytes(slow=False):
    out = io.BytesIO()
    with wave.open(out, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(b"SLOW" * 16 if slow else b"\0\0" * 32)
    return out.getvalue()


def request(conn, slow=False):
    wav = wav_bytes(slow)
    conn.request("POST", "/v1/audio/transcriptions", wav,
                 {"Content-Type": "audio/wav"})


def successful_request(port):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=15)
    try:
        request(conn)
        response = conn.getresponse()
        body = json.loads(response.read())
        assert response.status == 200, body
        assert body["text"].startswith("ok:"), body
        return body["text"]
    finally:
        conn.close()


def run(server_path):
    repo = Path(__file__).resolve().parents[3]
    temp_root = repo / ".jxai" / "tmp"
    temp_root.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="asr-cancel-regression-", dir=temp_root) as tmp:
        root = Path(tmp)
        # Model discovery only sees placeholders, and fake workers never load them.
        (root / "xasr").mkdir()
        (root / "silero_vad.onnx").touch()
        os.environ["VT_MODEL_DIR"] = tmp
        os.environ["VT_CANCEL_TEST_DIR"] = tmp
        sys.path.insert(0, str(server_path.parent))
        module = importlib.import_module(server_path.stem)
        module.transcribe_wav = fake_transcribe  # pre-fix direct-call path
        isolated = hasattr(module, "InferenceWorker")
        if isolated:
            module._worker = module.InferenceWorker(fake_transcribe, fake_warmup)
        cls = getattr(module, "ASRServer", ThreadingHTTPServer)
        server = cls(("127.0.0.1", 0), module.Handler)
        server.daemon_threads = True
        server.block_on_close = False
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        port = server.server_address[1]
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=15)
        try:
            request(conn, slow=True)
            heartbeat = root / "heartbeat"
            wait_for(heartbeat.exists)
            # Wait for inference to actually start, not merely for TCP acceptance.
            time.sleep(0.1)
            conn.sock.settimeout(0.15)
            try:
                conn.getresponse()
                raise AssertionError("slow request unexpectedly completed")
            except socket.timeout:
                print("client timed out; closing its connection", flush=True)
            finally:
                conn.close()
            # Native work must stop promptly, not just return late and drop output.
            time.sleep(0.4)
            first = heartbeat.read_text(encoding="ascii")
            time.sleep(0.25)
            second = heartbeat.read_text(encoding="ascii")
            assert first == second, "RED: server inference still running after client timeout/disconnect"
            assert not (root / "completed").exists(), "cancelled inference completed"
            cancelled_pid = first.split(":")[0]
            if isolated:
                assert cancelled_pid != str(os.getpid()), "fake inference was not isolated"
            next_result = successful_request(port)
            assert next_result != f"ok:{cancelled_pid}", "next request reused cancelled worker"
            assert successful_request(port) == next_result, "normal requests reloaded the resident worker"
            print("GREEN: server work stopped; next request succeeds; healthy worker reused", flush=True)
        finally:
            conn.close()
            (root / "stop").touch()
            server.shutdown()
            server.server_close()
            thread.join(timeout=3)
            if isolated:
                assert module._worker._process is None, "owned worker was not reaped"
            # The pre-fix fake runs in a handler thread; stop sentinel bounds cleanup.
            time.sleep(0.1)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=Path(__file__).with_name("asr_server.py"))
    args = parser.parse_args()
    run(args.server.resolve())
