"""Isolated dependency checks; never import actual ASR packages or invoke pip."""
import ast
import builtins
from pathlib import Path
import sys
import types
import unittest
from unittest.mock import patch

SOURCE = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).with_name("asr_worker.py")
sys.argv[1:] = []


def dependency_check():
    tree = ast.parse(SOURCE.read_text(encoding="utf-8"))
    main = next(n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == "main")
    fn = next(n for n in main.body if isinstance(n, ast.FunctionDef) and n.name == "ensure_deps")
    scope = {"sys": sys}
    exec(compile(ast.Module(body=[fn], type_ignores=[]), str(SOURCE), "exec"), scope)
    return scope["ensure_deps"]


class Dependencies(unittest.TestCase):
    def check(self, absent=(), broken=False):
        installed = []
        original = builtins.__import__
        def fake_import(name, *args, **kwargs):
            if name in absent:
                raise ImportError("isolated missing " + name)
            if name == "sherpa_onnx":
                return types.SimpleNamespace(**({} if broken else {"OfflineRecognizer": object()}))
            if name == "numpy":
                return types.SimpleNamespace()
            if name == "subprocess":
                def forbidden(*args, **kwargs):
                    installed.append(args)
                    raise AssertionError("MUST NOT install into shared runtime")
                return types.SimpleNamespace(run=forbidden)
            return original(name, *args, **kwargs)
        with patch("builtins.__import__", fake_import):
            if absent or broken:
                with self.assertRaises(RuntimeError) as ctx:
                    dependency_check()()
                message = str(ctx.exception)
                self.assertIn("pip install", message)
                self.assertIn("共享", message)
                self.assertIn("sherpa-onnx" if broken or "sherpa_onnx" in absent else "numpy", message)
            else:
                dependency_check()()
        self.assertEqual(installed, [])

    def test_missing_both_no_install(self):
        self.check(("sherpa_onnx", "numpy"))

    def test_missing_numpy_no_install(self):
        self.check(("numpy",))

    def test_broken_sherpa_no_install(self):
        self.check(broken=True)

    def test_installed_unchanged(self):
        self.check()


if __name__ == "__main__":
    unittest.main(verbosity=2)
