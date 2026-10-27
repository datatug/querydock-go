"""Disposable process-group proofs for the emulator owner's teardown boundary."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest


class OwnedRunnerTests(unittest.TestCase):
    def exercise(self, mode):
        with tempfile.TemporaryDirectory(prefix="querydock-owner-") as directory:
            scratch = Path(directory)
            child = scratch / "child.py"
            child.write_text("""import json, os, signal, subprocess, sys, time
from pathlib import Path
grandchild = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(120)'])
def stopped(signum, frame):
    grandchild.wait(timeout=5)
    raise SystemExit(128 + signum)
signal.signal(signal.SIGTERM, stopped)
signal.signal(signal.SIGINT, stopped)
Path('ready.json').write_text(json.dumps([os.getpid(), grandchild.pid]))
while True: time.sleep(1)
""")
            wrapper = scratch / "owner.py"
            module = Path(__file__).with_name("sdk-emulators.py").resolve()
            wrapper.write_text("""import importlib.util, json, os, sys
from pathlib import Path
spec=importlib.util.spec_from_file_location('sdk_owner', sys.argv[1])
sdk=importlib.util.module_from_spec(spec)
spec.loader.exec_module(sdk)
receipt=Path('owner.json')
owner={'owner':'disposable process-group test','owner_pid':os.getpid(),'teardown':'owned SIGTERM through run_owned'}
receipt.write_text(json.dumps(owner))
command=[sys.executable,'child.py']
if sys.argv[2]=='normal':command=[sys.executable,'-c','raise SystemExit(0)']
with Path('child.log').open('w') as log:
    result=sdk.run_owned(command,Path.cwd(),os.environ.copy(),log,receipt,owner,timeout=0.5 if sys.argv[2]=='timeout' else 30)
raise SystemExit(result)
""")
            # Owner: this test; the wrapper owns its disposable child group.
            # Teardown is installed before observing readiness or sending signals.
            with (scratch / "wrapper.log").open("w") as log:
                process = subprocess.Popen([sys.executable, str(wrapper), str(module), mode], cwd=scratch,
                                           stdout=log, stderr=subprocess.STDOUT)
                try:
                    pids = []
                    if mode != "normal":
                        deadline = time.monotonic() + 10
                        while not (scratch / "ready.json").exists() and time.monotonic() < deadline:
                            if process.poll() is not None:
                                self.fail("owner exited before child group became ready")
                            time.sleep(0.02)
                        self.assertTrue((scratch / "ready.json").exists())
                        pids = json.loads((scratch / "ready.json").read_text())
                    if mode == "sigterm":
                        process.send_signal(signal.SIGTERM)
                    result = process.wait(timeout=15)
                    self.assertEqual(result, {"normal": 0, "timeout": 124, "sigterm": 143}[mode])
                    receipt = json.loads((scratch / "owner.json").read_text())
                    self.assertEqual(receipt["exit_code"], result)
                    self.assertTrue(receipt["process_group_released"])
                    self.assertTrue(receipt["teardown_complete"])
                    for pid in pids:
                        with self.assertRaises(ProcessLookupError):
                            os.kill(pid, 0)
                finally:
                    if process.poll() is None:
                        process.send_signal(signal.SIGTERM)
                        process.wait(timeout=15)
                    receipt_file = scratch / "owner.json"
                    if receipt_file.exists():
                        group = json.loads(receipt_file.read_text()).get("process_group")
                        if group:
                            try:
                                os.killpg(group, signal.SIGKILL)
                            except ProcessLookupError:
                                pass

    def test_normal_completion(self):
        self.exercise("normal")

    def test_timeout_cleans_child_and_grandchild(self):
        self.exercise("timeout")

    def test_sigterm_cleans_child_and_grandchild(self):
        self.exercise("sigterm")


if __name__ == "__main__":
    unittest.main()
