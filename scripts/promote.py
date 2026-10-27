#!/usr/bin/env python3
"""Promote a verified canonical-main CI binary; never rebuild source or create services."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile

REPOSITORY = "datatug/querydock-go"
REPOSITORY_ID = 1434555650
OWNER_ID = 61551712
ARTIFACT = "querydock-go-linux-amd64"
BASE = "gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab"
IMAGE = "europe-west1-docker.pkg.dev/sneat-eur3-1/querydock/querydock-go"
SERVICE = "projects/sneat-eur3-1/locations/europe-west1/services/querydock-go"
RUNTIME = "querydock-runtime@sneat-eur3-1.iam.gserviceaccount.com"
SHA = re.compile(r"[a-f0-9]{40}\Z")
DIGEST = re.compile(r"sha256:[a-f0-9]{64}\Z")


def require(ok, message):
    if not ok:
        raise ValueError(message)


def request(url, token=None, data=None, method=None):
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    if data is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(data).encode()
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=60) as response:
        body = response.read(150_000_001)
        require(len(body) <= 150_000_000, "response size limit")
        return body


def github(path):
    return json.loads(request("https://api.github.com/repos/" + REPOSITORY + path,
                              os.environ["GH_TOKEN"]))


def provenance(repo, run, head, artifacts, run_id):
    require(repo.get("id") == REPOSITORY_ID and repo.get("owner", {}).get("id") == OWNER_ID
            and repo.get("full_name") == REPOSITORY, "repository identity mismatch")
    require(run.get("id") == run_id and run.get("repository", {}).get("id") == REPOSITORY_ID
            and run.get("head_repository", {}).get("id") == REPOSITORY_ID, "run origin mismatch")
    require(run.get("name") == "Go CI" and run.get("path") == ".github/workflows/ci.yml"
            and run.get("head_branch") == "main" and run.get("event") in ("push", "workflow_dispatch")
            and run.get("status") == "completed" and run.get("conclusion") == "success",
            "whole canonical Go CI run must succeed")
    sha = run.get("head_sha", "")
    require(SHA.fullmatch(sha) and sha == head.get("object", {}).get("sha"), "stale or invalid main SHA")
    matches = [a for a in artifacts if a.get("name") == ARTIFACT]
    require(len(matches) == 1, "expected one immutable binary artifact")
    artifact = matches[0]
    origin = artifact.get("workflow_run", {})
    require(type(artifact.get("id")) is int and artifact["id"] > 0 and artifact.get("expired") is False
            and 0 < artifact.get("size_in_bytes", 0) < 150_000_000
            and DIGEST.fullmatch(artifact.get("digest", "")), "invalid artifact metadata")
    require(origin.get("id") == run_id and origin.get("repository_id") == REPOSITORY_ID
            and origin.get("head_repository_id") == REPOSITORY_ID and origin.get("head_branch") == "main"
            and origin.get("head_sha") == sha, "artifact provenance mismatch")
    return sha, artifact


def verify_archive(raw, artifact, sha, target):
    require(len(raw) == artifact["size_in_bytes"], "archive size mismatch")
    require("sha256:" + hashlib.sha256(raw).hexdigest() == artifact["digest"], "archive digest mismatch")
    expected = {"querydock-go", "querydock-go.sha256", "querydock-go.source-sha"}
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        entries = archive.infolist()
        require(len(entries) == 3 and {e.filename for e in entries} == expected, "unexpected archive paths")
        for entry in entries:
            mode = entry.external_attr >> 16
            require(not entry.is_dir() and (stat.S_IFMT(mode) in (0, stat.S_IFREG))
                    and not entry.flag_bits & 1 and 0 < entry.file_size < (150_000_000 if entry.filename == "querydock-go" else 200),
                    "unsafe archive member")
        contents = {e.filename: archive.read(e) for e in entries}
    require(contents["querydock-go.source-sha"] == (sha + "\n").encode(), "source SHA mismatch")
    binary = contents["querydock-go"]
    checksum = hashlib.sha256(binary).hexdigest()
    require(contents["querydock-go.sha256"] == (checksum + "  bin/querydock-go\n").encode(), "binary checksum mismatch")
    require(binary[:6] == b"\x7fELF\x02\x01" and len(binary) >= 20 and binary[18:20] == b"\x3e\x00", "expected static Linux amd64 binary")
    target.mkdir(parents=True, exist_ok=False)
    (target / "querydock-go").write_bytes(binary)
    (target / "querydock-go").chmod(0o555)
    (target / "Dockerfile").write_text(f"FROM {BASE}\nWORKDIR /app\nCOPY --chown=65532:65532 querydock-go /app/querydock-go\nUSER 65532:65532\nENV PORT=8080\nEXPOSE 8080\nENTRYPOINT [\"/app/querydock-go\"]\n")
    return checksum


def artifact_list(run_id):
    result = []
    for page in range(1, 101):
        data = github(f"/actions/runs/{run_id}/artifacts?per_page=100&page={page}")
        result.extend(data["artifacts"])
        if len(result) >= data["total_count"]:
            return result
    raise ValueError("artifact pagination limit")


def validate(run_id):
    return provenance(github(""), github(f"/actions/runs/{run_id}"),
                      github("/git/ref/heads/main"), artifact_list(run_id), run_id)


def prepare(run_id, work):
    sha, artifact = validate(run_id)
    # urllib follows GitHub's signed archive redirect without forwarding a token:
    # obtain only the Location with a nonredirecting opener first.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *args):
            return None
    req = urllib.request.Request(f"https://api.github.com/repos/{REPOSITORY}/actions/artifacts/{artifact['id']}/zip",
                                 headers={"Authorization": "Bearer " + os.environ["GH_TOKEN"]})
    try:
        urllib.request.build_opener(NoRedirect).open(req, timeout=60)
        raise ValueError("expected signed archive redirect")
    except urllib.error.HTTPError as response:
        require(response.code == 302, "artifact download rejected")
        location = response.headers["Location"]
    require(urllib.parse.urlsplit(location).scheme == "https", "insecure archive redirect")
    raw = request(location)
    checksum = verify_archive(raw, artifact, sha, work / "image")
    manifest = {"source_sha": sha, "run_id": run_id, "artifact_id": artifact["id"],
                "artifact_digest": artifact["digest"], "binary_sha256": checksum}
    (work / "verified.json").write_text(json.dumps(manifest, indent=2) + "\n")


def cloud(path, token, data=None, method=None):
    return json.loads(request("https://run.googleapis.com/v2/" + path, token, data, method))


def revision_id(name):
    prefix = SERVICE + "/revisions/"
    require(isinstance(name, str) and name.startswith(prefix), "foreign revision resource")
    short = name[len(prefix):]
    require(len(short) <= 63 and re.fullmatch(r"querydock-go-[a-z0-9]+(?:-[a-z0-9]+)*", short),
            "invalid revision identifier")
    return short


def deployment_body(before, digest, source, run_id):
    require(DIGEST.fullmatch(digest) and SHA.fullmatch(source) and type(run_id) is int and run_id > 0, "invalid deployment provenance")
    require(before.get("name") == SERVICE and before.get("etag"), "existing service required")
    require(not before.get("reconciling") and before.get("latestReadyRevision")
            and before.get("latestReadyRevision") == before.get("latestCreatedRevision")
            and before.get("terminalCondition", {}).get("state") == "CONDITION_SUCCEEDED", "existing service must be stable")
    ready_id = revision_id(before["latestReadyRevision"])
    traffic = before.get("trafficStatuses", [])
    require(len(traffic) == 1 and traffic[0].get("percent") == 100
            and traffic[0].get("revision") == ready_id,
            "existing traffic must fully target the ready revision")
    template = json.loads(json.dumps(before["template"]))
    require(len(template.get("containers", [])) == 1, "expected one runtime container")
    template["revision"] = f"querydock-go-{source[:12]}-{run_id}"
    template["serviceAccount"] = RUNTIME
    template.setdefault("labels", {})["querydock-source"] = source
    container = template["containers"][0]
    container["image"] = IMAGE + "@" + digest
    env = container.get("env", [])
    require(not any(e["name"] in ("FIRESTORE_EMULATOR_HOST", "FIREBASE_AUTH_EMULATOR_HOST", "GOOGLE_APPLICATION_CREDENTIALS") for e in env), "unsafe existing runtime environment")
    container["env"] = [e for e in env if e["name"] not in ("QUERYDOCK_PROJECT_ID", "QUERYDOCK_DATABASE_ID")] + [
        {"name": "QUERYDOCK_PROJECT_ID", "value": "sneat-eur3-1"},
        {"name": "QUERYDOCK_DATABASE_ID", "value": "(default)"}]
    return {"name": SERVICE, "etag": before["etag"], "template": template,
            "traffic": [{"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST", "percent": 100}]}


def verify_service(service, body):
    expected = SERVICE + "/revisions/" + body["template"]["revision"]
    expected_id = revision_id(expected)
    require(service.get("latestReadyRevision") == expected and service.get("latestCreatedRevision") == expected
            and str(service.get("observedGeneration")) == str(service.get("generation"))
            and service.get("terminalCondition", {}).get("state") == "CONDITION_SUCCEEDED",
            "service revision not ready")
    template = service["template"]
    require(template["containers"][0]["image"] == body["template"]["containers"][0]["image"]
            and template.get("labels", {}).get("querydock-source") == body["template"]["labels"]["querydock-source"]
            and template.get("serviceAccount") == RUNTIME, "deployed identity/image/source mismatch")
    traffic = service.get("trafficStatuses", [])
    require(len(traffic) == 1 and traffic[0].get("revision") == expected_id and traffic[0].get("percent") == 100,
            "traffic not fully promoted")
    uri = urllib.parse.urlsplit(service.get("uri", ""))
    require(uri.scheme == "https" and uri.hostname and uri.hostname.endswith(".run.app")
            and not uri.username and not uri.query and not uri.fragment, "invalid service URI")
    return expected, service["uri"]


def deploy(work):
    manifest = json.loads((work / "verified.json").read_text())
    sha, artifact = validate(manifest["run_id"])
    require(sha == manifest["source_sha"] and artifact["id"] == manifest["artifact_id"]
            and artifact["digest"] == manifest["artifact_digest"], "provenance changed")
    require(hashlib.sha256((work / "image/querydock-go").read_bytes()).hexdigest() == manifest["binary_sha256"], "verified binary changed")
    token = os.environ["GOOGLE_ACCESS_TOKEN"]
    subprocess.run(["docker", "login", "europe-west1-docker.pkg.dev", "-u", "oauth2accesstoken", "--password-stdin"], input=token.encode(), check=True, stdout=subprocess.DEVNULL)
    tag = IMAGE + ":" + sha + "-" + str(manifest["run_id"])
    subprocess.run(["docker", "build", "--platform=linux/amd64", "--label", "org.opencontainers.image.revision=" + sha, "-t", tag, str(work / "image")], check=True)
    pushed = subprocess.run(["docker", "push", tag], check=True, capture_output=True, text=True)
    matches = re.findall(r"digest: (sha256:[a-f0-9]{64})", pushed.stdout + pushed.stderr)
    require(len(set(matches)) == 1, "missing pushed immutable digest")
    digest = matches[0]
    # Recheck canonical main after image publication, immediately before mutation.
    latest, _ = validate(manifest["run_id"])
    require(latest == sha, "main changed before deployment")
    before = cloud(SERVICE, token)
    body = deployment_body(before, digest, sha, manifest["run_id"])
    rollback = {"revision": before.get("latestReadyRevision"), "image": before["template"]["containers"][0]["image"]}
    attempt = dict(manifest, image=IMAGE + "@" + digest,
                   revision=SERVICE + "/revisions/" + body["template"]["revision"], rollback_predecessor=rollback)
    (work / "attempt.json").write_text(json.dumps(attempt, indent=2) + "\n")
    operation = cloud(SERVICE + "?updateMask=template,traffic", token, body, "PATCH")
    deadline = time.monotonic() + 600
    while not operation.get("done"):
        require(time.monotonic() < deadline, "deployment operation timeout")
        time.sleep(3)
        require(operation.get("name", "").startswith("projects/sneat-eur3-1/locations/europe-west1/operations/"), "unexpected operation")
        operation = cloud(operation["name"], token)
    require(not operation.get("error"), "deployment operation failed")
    revision, uri = verify_service(cloud(SERVICE, token), body)
    with urllib.request.urlopen(uri.rstrip("/") + "/readyz", timeout=30) as response:
        require(response.status == 200 and response.read(1) == b"", "unexpected readiness response")
    manifest.update({"image": IMAGE + "@" + digest, "revision": revision, "uri": uri, "rollback_predecessor": rollback})
    (work / "receipt.json").write_text(json.dumps(manifest, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("prepare", "deploy"))
    parser.add_argument("--run-id", type=int)
    parser.add_argument("--work", type=Path, required=True)
    args = parser.parse_args()
    if args.mode == "prepare":
        require(args.run_id is not None and args.run_id > 0, "positive run id required")
        args.work.mkdir(parents=True, exist_ok=False)
        prepare(args.run_id, args.work)
    else:
        deploy(args.work)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        # Provider response bodies, credential paths and signed URLs are private.
        raise SystemExit("QueryDock promotion failed; no credential or provider details logged") from None
