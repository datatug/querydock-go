#!/usr/bin/env python3
"""Own an isolated demo Firebase SDK journey; never reuse another emulator."""
import argparse
from contextlib import ExitStack
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time


def group_exists(pid):
    try:
        os.killpg(pid, 0)
        return True
    except ProcessLookupError:
        return False


def run_owned(command, cwd, env, log, receipt, owner, timeout=240):
    """Own the whole subprocess group through success, timeout and termination."""
    handled = (signal.SIGTERM, signal.SIGINT)
    previous = {sig: signal.getsignal(sig) for sig in handled}
    process = None
    result = 125
    requested = None

    def interrupt(signum, _frame):
        # Record rather than raise: a signal during Popen cannot strand an
        # unassigned process handle. The bounded wait loop enters teardown.
        nonlocal requested
        if requested is None:
            requested = signum

    try:
        for sig in handled:
            signal.signal(sig, interrupt)
        process = subprocess.Popen(command, cwd=cwd, env=env, stdout=log,
                                   stderr=subprocess.STDOUT, start_new_session=True)
        owner["process_group"] = process.pid
        receipt.write_text(json.dumps(owner, indent=2) + "\n")
        deadline = time.monotonic() + timeout
        while True:
            if requested is not None:
                result = 128 + requested
                break
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                result = 124
                break
            try:
                result = process.wait(timeout=min(0.2, remaining))
                break
            except subprocess.TimeoutExpired:
                continue
    finally:
        # A repeated signal must not interrupt teardown midway through.
        for sig in handled:
            signal.signal(sig, signal.SIG_IGN)
        gone = True
        if process is not None:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=10)
            until = time.monotonic() + 2
            while group_exists(process.pid) and time.monotonic() < until:
                time.sleep(0.02)
            if group_exists(process.pid):
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                until = time.monotonic() + 2
                while group_exists(process.pid) and time.monotonic() < until:
                    time.sleep(0.02)
            gone = not group_exists(process.pid)
        if not gone:
            result = 125
        owner["exit_code"] = result
        owner["process_group_released"] = gone
        owner["teardown_complete"] = gone
        receipt.write_text(json.dumps(owner, indent=2) + "\n")
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--firebase-js", type=Path, required=True)
    parser.add_argument("--logs", type=Path, required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    cli = args.firebase_js.resolve()
    args.logs.mkdir(parents=True, exist_ok=True)
    owner = {"owner": "QueryDock Task 2 SDK tests", "owner_pid": os.getpid(),
             "project": "demo-querydock-task2", "teardown": "SIGTERM owned subprocess process group, then wait; SIGKILL only on timeout"}
    receipt = args.logs / "owner.json"
    receipt.write_text(json.dumps(owner, indent=2) + "\n")
    # These sockets are owned by this function and closed by their contexts;
    # the CLI subsequently claims the chosen ports and fails if any was taken.
    ports = []
    with ExitStack() as reservations:
        for _ in range(5):
            reservation = reservations.enter_context(socket.socket())
            reservation.bind(("127.0.0.1", 0))
            ports.append(reservation.getsockname()[1])
    owner["ports"] = ports
    receipt.write_text(json.dumps(owner, indent=2) + "\n")
    with tempfile.TemporaryDirectory(prefix="querydock-sdk-") as scratch:
        scratch = Path(scratch)
        rules = scratch / "firestore.rules"
        rules.write_text("rules_version = '2'; service cloud.firestore { match /databases/{database}/documents { match /{document=**} { allow read, write: if false; } } }\n")
        config = {"firestore": {"rules": str(rules)}, "emulators": {
            "auth": {"host": "127.0.0.1", "port": ports[0]},
            "firestore": {"host": "127.0.0.1", "port": ports[1], "websocketPort": ports[4]},
            "hub": {"host": "127.0.0.1", "port": ports[2]},
            "logging": {"host": "127.0.0.1", "port": ports[3]},
            "ui": {"enabled": False}, "singleProjectMode": True}}
        config_path = scratch / "firebase.json"
        config_path.write_text(json.dumps(config))
        env = os.environ.copy()
        for key in ["GOOGLE_APPLICATION_CREDENTIALS", "FIREBASE_CONFIG", "FIREBASE_AUTH_EMULATOR_HOST", "FIRESTORE_EMULATOR_HOST"]:
            env.pop(key, None)
        env.update({"CI": "1", "GOWORK": "off", "FIREBASE_CLI_DISABLE_UPDATE_CHECK": "true",
                    "QUERYDOCK_TEST_PROJECT": owner["project"], "GCLOUD_PROJECT": owner["project"], "GOOGLE_CLOUD_PROJECT": owner["project"]})
        # Paths are repository-controlled and shell-quoted for emulators:exec.
        import shlex
        tests = "cd " + shlex.quote(str(root)) + " && go test -race -count=1 -run 'Test(Firebase|Firestore)Emulator$' ./internal/identity ./internal/revisions"
        command = ["node", str(cli), "emulators:exec", "--only", "auth,firestore", "--project", owner["project"], "--config", str(config_path), tests]
        with (args.logs / "journey.log").open("w") as log:
            result = run_owned(command, scratch, env, log, receipt, owner)
            for name in ["firebase-debug.log", "firestore-debug.log"]:
                path = scratch / name
                if path.exists():
                    (args.logs / name).write_bytes(path.read_bytes())
            # Confirm that the owned listeners were released, including the
            # Firestore websocket/hub/logging listeners rather than just Go.
            released = True
            for port in ports:
                with socket.socket() as probe:
                    probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                    try:
                        probe.bind(("127.0.0.1", port))
                    except OSError:
                        released = False
            if not released:
                result = 125
            owner["exit_code"] = result
            owner["ports_released"] = released
            owner["teardown_complete"] = released and owner["process_group_released"]
            receipt.write_text(json.dumps(owner, indent=2) + "\n")
            return result


if __name__ == "__main__":
    raise SystemExit(main())
