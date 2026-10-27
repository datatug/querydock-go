import copy
import hashlib
import io
import json
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import patch
import zipfile
import promote as p


class PromotionTests(unittest.TestCase):
    def setUp(self):
        self.sha = "a" * 40
        self.repo = {"id": p.REPOSITORY_ID, "owner": {"id": p.OWNER_ID}, "full_name": p.REPOSITORY}
        self.run = {"id": 42, "repository": {"id": p.REPOSITORY_ID}, "head_repository": {"id": p.REPOSITORY_ID},
                    "name": "Go CI", "path": ".github/workflows/ci.yml", "head_branch": "main", "event": "push",
                    "status": "completed", "conclusion": "success", "head_sha": self.sha}
        self.head = {"object": {"sha": self.sha}}
        self.artifact = {"id": 77, "name": p.ARTIFACT, "expired": False, "size_in_bytes": 1000,
                         "digest": "sha256:" + "b" * 64,
                         "workflow_run": {"id": 42, "repository_id": p.REPOSITORY_ID,
                                          "head_repository_id": p.REPOSITORY_ID, "head_branch": "main", "head_sha": self.sha}}
        binary = b"\x7fELF\x02\x01" + b"\0" * 12 + b"\x3e\x00" + b"verified binary bytes"
        self.files = {"querydock-go": binary, "querydock-go.source-sha": (self.sha + "\n").encode(),
                      "querydock-go.sha256": (hashlib.sha256(binary).hexdigest() + "  bin/querydock-go\n").encode()}

    def provenance(self):
        return p.provenance(self.repo, self.run, self.head, [self.artifact], 42)

    def archive(self, files=None, mode=None):
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w") as archive:
            for name, data in (files or self.files).items():
                info = zipfile.ZipInfo(name)
                if mode:
                    info.external_attr = mode << 16
                archive.writestr(info, data)
        raw = output.getvalue()
        self.artifact["size_in_bytes"] = len(raw)
        self.artifact["digest"] = "sha256:" + hashlib.sha256(raw).hexdigest()
        return raw

    def test_canonical_success(self):
        sha, artifact = self.provenance()
        self.assertEqual((sha, artifact["id"]), (self.sha, 77))
        with tempfile.TemporaryDirectory() as temp:
            target = Path(temp) / "image"
            checksum = p.verify_archive(self.archive(), artifact, sha, target)
            self.assertEqual(checksum, hashlib.sha256(self.files["querydock-go"]).hexdigest())
            self.assertEqual((target / "querydock-go").read_bytes(), self.files["querydock-go"])
            self.assertIn(p.BASE, (target / "Dockerfile").read_text())
            self.assertNotIn("go build", (target / "Dockerfile").read_text())

    def test_provenance_rejections(self):
        changes = [("repo", "id", 1), ("repo", "owner", {"id": 1}), ("repo", "full_name", "fork/querydock-go"),
                   ("run", "conclusion", "failure"), ("run", "conclusion", "cancelled"),
                   ("run", "conclusion", None), ("run", "status", "in_progress"),
                   ("run", "event", "pull_request"), ("run", "head_branch", "integration/test"),
                   ("run", "head_repository", {"id": 1}), ("run", "repository", {"id": 1}),
                   ("run", "path", ".github/workflows/other.yml"), ("run", "name", "Other CI"),
                   ("run", "id", 43), ("run", "head_sha", "bad"),
                   ("head", "object", {"sha": "b" * 40}), ("artifact", "expired", True),
                   ("artifact", "id", "77"), ("artifact", "size_in_bytes", 0),
                   ("artifact", "digest", "sha256:bad"), ("artifact", "workflow_run", {"id": 1})]
        for obj, key, value in changes:
            with self.subTest(obj=obj, key=key, value=value):
                original = copy.deepcopy(getattr(self, obj))
                getattr(self, obj)[key] = value
                with self.assertRaises(ValueError):
                    self.provenance()
                setattr(self, obj, original)
        for artifacts in ([], [self.artifact, self.artifact]):
            with self.assertRaises(ValueError):
                p.provenance(self.repo, self.run, self.head, artifacts, 42)

    def test_malformed_artifacts_leave_no_output(self):
        variants = []
        for name in ("../querydock-go", "/querydock-go", "bin/querydock-go", "querydock-go/", "unexpected"):
            files = dict(self.files)
            files[name] = files.pop("querydock-go")
            variants.append((files, None))
        variants.append((self.files, stat.S_IFLNK | 0o777))
        for name in self.files:
            files = dict(self.files)
            files[name] += b"tampered"
            variants.append((files, None))
        files = dict(self.files)
        files["querydock-go"] = b"not ELF"
        files["querydock-go.sha256"] = (hashlib.sha256(files["querydock-go"]).hexdigest() + "  bin/querydock-go\n").encode()
        variants.append((files, None))
        with tempfile.TemporaryDirectory() as temp:
            for files, mode in variants:
                with self.subTest(files=list(files), mode=mode):
                    target = Path(temp) / "image"
                    with self.assertRaises(ValueError):
                        p.verify_archive(self.archive(files, mode), self.artifact, self.sha, target)
                    self.assertFalse(target.exists())
            raw = self.archive()
            with self.assertRaises(ValueError):
                p.verify_archive(raw + b"tampered", self.artifact, self.sha, Path(temp) / "image")

    def test_duplicate_members_rejected(self):
        raw = self.archive()
        output = io.BytesIO(raw)
        with zipfile.ZipFile(output, "a") as archive:
            archive.writestr("querydock-go", b"duplicate")
        raw = output.getvalue()
        self.artifact["size_in_bytes"] = len(raw)
        self.artifact["digest"] = "sha256:" + hashlib.sha256(raw).hexdigest()
        with tempfile.TemporaryDirectory() as temp, self.assertRaises(ValueError):
            p.verify_archive(raw, self.artifact, self.sha, Path(temp) / "image")

    def service(self):
        return {"name": p.SERVICE, "etag": "etag", "latestReadyRevision": "previous", "latestCreatedRevision": "previous",
                "terminalCondition": {"state": "CONDITION_SUCCEEDED"},
                "trafficStatuses": [{"revision": "previous", "percent": 100}], "template": {
            "revision": "previous", "serviceAccount": p.RUNTIME,
            "containers": [{"image": "previous@sha256:" + "a" * 64, "env": [{"name": "EXISTING", "value": "keep"}]}]}}

    def test_existing_service_update_only_and_receipt_checks(self):
        before = self.service()
        body = p.deployment_body(before, "sha256:" + "c" * 64, self.sha, 42)
        self.assertEqual(before, self.service())
        self.assertEqual(body["etag"], "etag")
        self.assertEqual(body["template"]["serviceAccount"], p.RUNTIME)
        env = body["template"]["containers"][0]["env"]
        self.assertIn({"name": "QUERYDOCK_DATABASE_ID", "value": "(default)"}, env)
        self.assertIn({"name": "QUERYDOCK_PROJECT_ID", "value": "sneat-eur3-1"}, env)
        self.assertIn({"name": "EXISTING", "value": "keep"}, env)
        revision = p.SERVICE + "/revisions/" + body["template"]["revision"]
        ready = {"template": body["template"], "latestReadyRevision": revision, "latestCreatedRevision": revision,
                 "observedGeneration": "2", "generation": "2", "terminalCondition": {"state": "CONDITION_SUCCEEDED"},
                 "trafficStatuses": [{"revision": revision, "percent": 100}], "uri": "https://querydock-go-example.run.app"}
        self.assertEqual(p.verify_service(ready, body), (revision, ready["uri"]))
        for key, value in (("latestReadyRevision", "old"), ("latestCreatedRevision", "old"),
                           ("observedGeneration", "1"), ("terminalCondition", {"state": "CONDITION_FAILED"}),
                           ("trafficStatuses", [{"revision": revision, "percent": 50}]), ("uri", "https://evil.example")):
            invalid = copy.deepcopy(ready)
            invalid[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                p.verify_service(invalid, body)
        invalid = copy.deepcopy(ready)
        invalid["template"]["containers"][0]["image"] = "tag:mutable"
        with self.assertRaises(ValueError):
            p.verify_service(invalid, body)
        for traffic in ([], [{"revision": "previous", "percent": 50}, {"revision": "older", "percent": 50}],
                        [{"revision": "older", "percent": 100}]):
            invalid = self.service()
            invalid["trafficStatuses"] = traffic
            with self.subTest(previous_traffic=traffic), self.assertRaises(ValueError):
                p.deployment_body(invalid, "sha256:" + "c" * 64, self.sha, 42)
        for key in ("name", "etag"):
            invalid = self.service()
            invalid.pop(key)
            with self.assertRaises(ValueError):
                p.deployment_body(invalid, "sha256:" + "c" * 64, self.sha, 42)
        for name in ("FIRESTORE_EMULATOR_HOST", "FIREBASE_AUTH_EMULATOR_HOST", "GOOGLE_APPLICATION_CREDENTIALS"):
            invalid = self.service()
            invalid["template"]["containers"][0]["env"].append({"name": name, "value": "unsafe"})
            with self.assertRaises(ValueError):
                p.deployment_body(invalid, "sha256:" + "c" * 64, self.sha, 42)

    def test_deploy_records_exact_receipt_without_rebuild(self):
        digest = "sha256:" + "c" * 64
        with tempfile.TemporaryDirectory() as temp:
            work = Path(temp)
            checksum = p.verify_archive(self.archive(), self.artifact, self.sha, work / "image")
            manifest = {"run_id": 42, "artifact_id": 77, "source_sha": self.sha,
                        "artifact_digest": self.artifact["digest"], "binary_sha256": checksum}
            (work / "verified.json").write_text(json.dumps(manifest))
            before = self.service()
            body = p.deployment_body(before, digest, self.sha, 42)
            revision = p.SERVICE + "/revisions/" + body["template"]["revision"]
            ready = {"template": body["template"], "latestReadyRevision": revision, "latestCreatedRevision": revision,
                     "observedGeneration": "2", "generation": "2", "terminalCondition": {"state": "CONDITION_SUCCEEDED"},
                     "trafficStatuses": [{"revision": revision, "percent": 100}], "uri": "https://querydock-go-example.run.app"}
            calls = []
            def process(args, **kwargs):
                calls.append((args, kwargs))
                return p.subprocess.CompletedProcess(args, 0, stdout="digest: " + digest, stderr="")
            from unittest.mock import MagicMock
            response = MagicMock()
            response.__enter__.return_value.status = 200
            response.__enter__.return_value.read.return_value = b""
            with patch.object(p, "validate", return_value=(self.sha, self.artifact)), patch.dict(p.os.environ, {"GOOGLE_ACCESS_TOKEN": "test-token"}), \
                    patch.object(p.subprocess, "run", side_effect=process), patch.object(p.urllib.request, "urlopen", return_value=response), \
                    patch.object(p, "cloud", side_effect=[before, {"name": "operation", "done": True}, ready]) as cloud:
                p.deploy(work)
            receipt = json.loads((work / "receipt.json").read_text())
            self.assertEqual(receipt["image"], p.IMAGE + "@" + digest)
            self.assertEqual(receipt["revision"], revision)
            self.assertEqual(receipt["rollback_predecessor"], {"revision": "previous", "image": before["template"]["containers"][0]["image"]})
            self.assertEqual(cloud.call_args_list[1].args[0], p.SERVICE + "?updateMask=template,traffic")
            self.assertEqual(cloud.call_args_list[1].args[3], "PATCH")
            self.assertEqual(calls[0][1]["input"], b"test-token")
            self.assertFalse(any("test-token" in argument for args, _ in calls for argument in args))
            self.assertEqual([args[1] for args, _ in calls], ["login", "build", "push"])

    def test_changed_binary_cannot_be_pushed(self):
        with tempfile.TemporaryDirectory() as temp:
            work = Path(temp)
            checksum = p.verify_archive(self.archive(), self.artifact, self.sha, work / "image")
            (work / "verified.json").write_text(json.dumps({"run_id": 42, "artifact_id": 77, "source_sha": self.sha,
                "artifact_digest": self.artifact["digest"], "binary_sha256": checksum}))
            (work / "image/querydock-go").chmod(0o755)
            (work / "image/querydock-go").write_bytes(b"tampered")
            with patch.object(p, "validate", return_value=(self.sha, self.artifact)), patch.object(p.subprocess, "run") as process, patch.object(p, "cloud") as cloud:
                with self.assertRaises(ValueError):
                    p.deploy(work)
                process.assert_not_called()
                cloud.assert_not_called()

    def test_stale_deployment_cannot_reach_cloud(self):
        with tempfile.TemporaryDirectory() as temp:
            work = Path(temp)
            (work / "verified.json").write_text(json.dumps({"run_id": 42, "artifact_id": 77, "source_sha": self.sha,
                                                          "artifact_digest": self.artifact["digest"]}))
            with patch.object(p, "validate", return_value=("b" * 40, self.artifact)), patch.object(p, "cloud") as cloud:
                with self.assertRaises(ValueError):
                    p.deploy(work)
                cloud.assert_not_called()


if __name__ == "__main__":
    unittest.main()
