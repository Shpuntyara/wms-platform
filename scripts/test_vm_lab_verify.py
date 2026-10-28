"""Offline behavior tests for the guest-side verifier. No live service is implied."""
import base64
import hashlib
import io
import json
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest import mock
import uuid

sys.path.insert(0, str(Path(__file__).parent))
import vm_lab_verify as verify


class FakeRuntime:
    def __init__(self, *, replay_changed=False, invariant=False, foreign_client=401, foreign_token=401):
        self.identity = dict(instance_id=str(uuid.uuid4()), issuer="https://192.0.2.1/auth/realms/wms-test", audience="wms-api-test")
        self.credentials = {name: "SECRET-" + name for name in ("oidc_client_secret", "api_password", "worker_password", "request_key", "outbox_key", "db_admin_password", "keycloak_password", "grafana_password", "monitor_password")}
        self.replay_changed, self.invariant = replay_changed, invariant
        self.foreign_client, self.foreign_token = foreign_client, foreign_token
        self.commands, self.queries = {}, []
        self.api_calls = []
        self.document, self.line, self.reservation = [str(uuid.uuid4()) for _ in range(3)]

    def token(self):
        return "SECRET-TOKEN", dict(iss=self.identity["issuer"], sub="service'account", aud=self.identity["audience"], exp=int(time.time()) + 600)

    def fingerprints(self, token):
        return dict(tls_ca_sha256="a" * 64, oidc_jwk_sha256="b" * 64)

    def request(self, method, path, *, data=None, headers=None):
        if path == "/readyz":
            return 200, {"status": "ready"}
        if path == "/monitor/":
            return 401, {}
        if path.endswith("/protocol/openid-connect/token"):
            return self.foreign_client, {"error": "invalid_client"}
        return self.foreign_token, {}

    def sql(self, text):
        self.queries.append(text)
        if "SELECT id::text FROM hardened_private.installation" in text:
            return self.identity["instance_id"]
        if "-- reconcile lab fixture" in text:
            return json.dumps({name: not self.invariant for name in verify.INVARIANTS})
        return ""

    def api(self, method, path, token, body=None, command=None):
        self.api_calls.append((method, path, body, command))
        if method == "GET":
            if "/operations/" in path:
                key = path.split("/operations/", 1)[1].split("?", 1)[0]
                return {"id": key, "status": "completed", "result": self.commands[key]}
            if "/stock-balances/" in path:
                return {"on_hand": "10.000000", "reserved_qty": "0.000000"}
            if "/reservations/" in path:
                return {"id": self.reservation, "quantity": "3.000000", "active_qty": "0.000000", "released_qty": "3.000000"}
            return {"id": self.document, "state": "active", "version": 4, "lines": [{"id": self.line, "reserved_qty": "0.000000", "fulfilled_qty": "0.000000"}]}
        if command in self.commands:
            result = dict(self.commands[command])
            if self.replay_changed:
                result["document_version"] += 1
            return result
        version = 1 if path == "/api/v1/documents" else (2 if path.endswith("/transitions") else (4 if path.endswith("/releases") else 3))
        result = dict(operation_id=command, document_id=self.document, document_version=version, content_revision=1,
                      lines=[{"id": self.line}], reservation_id=self.reservation)
        self.commands[command] = result
        return result

    def probe_database_password(self, role, password):
        return True


def peer_bundle():
    header = base64.urlsafe_b64encode(json.dumps(dict(alg="RS256", kid="public-key")).encode()).decode().rstrip("=")
    payload = base64.urlsafe_b64encode(json.dumps(dict(iss="https://192.0.2.2/auth/realms/wms-other", aud="wms-api-other", sub="peer", exp=int(time.time()) + 600)).encode()).decode().rstrip("=")
    return dict(source_instance_id=str(uuid.uuid4()), source_issuer="https://192.0.2.2/auth/realms/wms-other",
                client_id="wms-api-other", client_secret="PEER-SECRET", api_password="PEER-API-SECRET", worker_password="PEER-WORKER-SECRET", access_token=header + "." + payload + ".opaque")


class VerifyTests(unittest.TestCase):
    def test_real_business_workflow_contract_and_public_receipt(self):
        runtime = FakeRuntime()
        receipt = verify.run_smoke(runtime)
        self.assertEqual(receipt["status"], "verified")
        self.assertEqual(len(runtime.commands), 4)
        self.assertEqual(receipt["instance_id"], runtime.identity["instance_id"])
        self.assertTrue(all(receipt["invariants"].values()))
        self.assertIn("service''account", "\n".join(runtime.queries))
        self.assertNotIn("SECRET", json.dumps(receipt))
        uuid.UUID(receipt["run_id"])

    def test_changed_replay_result_fails(self):
        with self.assertRaisesRegex(RuntimeError, "replay"):
            verify.run_smoke(FakeRuntime(replay_changed=True))

    def test_bad_reconciliation_fails(self):
        with self.assertRaisesRegex(RuntimeError, "reconciliation"):
            verify.run_smoke(FakeRuntime(invariant=True))

    def test_monitor_auth_and_installation_uuid_fail_before_fixture_write(self):
        runtime = FakeRuntime()
        original = runtime.request
        runtime.request = lambda method, path, **kwargs: (200, {}) if path == "/monitor/" else original(method, path, **kwargs)
        with self.assertRaisesRegex(RuntimeError, "Monitoring"):
            verify.run_smoke(runtime)
        self.assertEqual(runtime.queries, [])
        runtime = FakeRuntime()
        runtime.sql = lambda text: str(uuid.uuid4())
        with self.assertRaisesRegex(RuntimeError, "UUID"):
            verify.run_smoke(runtime)

    def test_foreign_credentials_and_foreign_token_must_both_be_denied(self):
        result = verify.run_probe(FakeRuntime(), peer_bundle())
        self.assertTrue(result["foreign_client_rejected"])
        self.assertTrue(result["foreign_token_rejected"])
        self.assertNotIn("SECRET", json.dumps(result))
        for values in [dict(foreign_client=200), dict(foreign_token=200), dict(foreign_client=503)]:
            with self.subTest(values=values), self.assertRaises(RuntimeError):
                verify.run_probe(FakeRuntime(**values), peer_bundle())

    def test_same_instance_probe_and_expired_bundle_fail(self):
        runtime, bundle = FakeRuntime(), peer_bundle()
        bundle["source_instance_id"] = runtime.identity["instance_id"]
        with self.assertRaises(ValueError):
            verify.run_probe(runtime, bundle)
        bundle = peer_bundle()
        claims = verify.jwt_parts(bundle["access_token"])[1]
        claims["exp"] = int(time.time()) - 1
        payload = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
        parts = bundle["access_token"].split(".")
        bundle["access_token"] = parts[0] + "." + payload + "." + parts[2]
        with self.assertRaises(ValueError):
            verify.run_probe(runtime, bundle)

    def test_exclusive_secret_bundle_write(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "bundle.json"
            verify.write_protected(target, {"secret": "do-not-print"})
            with self.assertRaises(FileExistsError):
                verify.write_protected(target, {"secret": "replacement"})
            self.assertEqual(json.loads(target.read_text()), {"secret": "do-not-print"})

    def test_identity_rejects_tls_or_origin_drift(self):
        for issuer in ["http://192.0.2.1/auth/realms/wms-a", "https://other.example/auth/realms/wms-a"]:
            with self.subTest(issuer=issuer), self.assertRaises(ValueError):
                verify.validate_identity(dict(instance_id=str(uuid.uuid4()), address="192.0.2.1", issuer=issuer, audience="wms-api-a"))

    def test_diagnostic_failure_is_a_static_public_check(self):
        with self.assertRaises(verify.VerificationFailure) as failure:
            verify.require(False, "public readiness failed")
        self.assertEqual(failure.exception.check, "public readiness failed")

    def test_replay_reuses_exact_original_ids_bodies_and_never_seeds(self):
        runtime = FakeRuntime()
        first = verify.run_smoke(runtime)
        original = {key: (path, body) for method, path, body, key in runtime.api_calls if method == "POST"}
        runtime.api_calls.clear()
        runtime.queries.clear()
        result = verify.run_replay(runtime, first)
        self.assertEqual(result["status"], "verified")
        self.assertEqual(result["run_id"], first["run_id"])
        replayed = [(key, path, body) for method, path, body, key in runtime.api_calls if method == "POST"]
        self.assertEqual(len(replayed), 4)
        self.assertEqual({key: (path, body) for key, path, body in replayed}, original)
        self.assertFalse(any("INSERT" in query for query in runtime.queries))
        self.assertEqual(result["secret_sha256"]["api_password"], hashlib.sha256(runtime.credentials["api_password"].encode()).hexdigest())
        self.assertNotIn("SECRET", json.dumps(result))
        runtime.replay_changed = True
        with self.assertRaisesRegex(RuntimeError, "replay"):
            verify.run_replay(runtime, first)

    def test_replay_requires_same_instance_keys_and_secret_fingerprints(self):
        for changed in ("instance", "tls", "secret", "invariant"):
            runtime = FakeRuntime()
            first = verify.run_smoke(runtime)
            if changed == "instance":
                runtime.identity["instance_id"] = str(uuid.uuid4())
            elif changed == "tls":
                runtime.fingerprints = lambda token: dict(tls_ca_sha256="c" * 64, oidc_jwk_sha256="b" * 64)
            elif changed == "secret":
                runtime.credentials["api_password"] = "ROTATED-SECRET"
            else:
                runtime.invariant = True
            with self.subTest(changed=changed), self.assertRaises(RuntimeError):
                verify.run_replay(runtime, first)

    def test_watch_reads_authenticated_existing_document_and_stock_with_zero_errors(self):
        runtime = FakeRuntime()
        first = verify.run_smoke(runtime)
        runtime.api_calls.clear()
        now = [0.0]
        def sleep(value):
            now[0] += value
        result = verify.run_watch(runtime, first, seconds=3, interval=1, clock=lambda: now[0], sleeper=sleep)
        self.assertEqual(result["request_count"], 6)
        self.assertEqual(result["error_count"], 0)
        self.assertEqual(result["document_reads"], 3)
        self.assertEqual(result["stock_reads"], 3)
        self.assertEqual(result["status"], "verified")
        self.assertTrue(all(call[0] == "GET" for call in runtime.api_calls))
        self.assertNotIn("SECRET", json.dumps(result))

    def test_watch_fails_with_public_summary_when_a_read_fails(self):
        runtime = FakeRuntime()
        first = verify.run_smoke(runtime)
        original = runtime.api
        runtime.api = lambda *args, **kwargs: (_ for _ in ()).throw(RuntimeError("SECRET must never be echoed"))
        now = [0.0]
        with self.assertRaises(verify.WatchFailure) as caught:
            verify.run_watch(runtime, first, seconds=1, interval=1, clock=lambda: now[0], sleeper=lambda value: now.__setitem__(0, now[0] + value))
        self.assertEqual(caught.exception.receipt["error_count"], 2)
        self.assertEqual(caught.exception.receipt["status"], "failed")
        self.assertNotIn("SECRET", json.dumps(caught.exception.receipt))

    def test_ten_minute_watch_refreshes_five_minute_tokens_before_expiry(self):
        runtime = FakeRuntime()
        first = verify.run_smoke(runtime)
        now, tokens = [0.0], []
        def token():
            tokens.append(now[0])
            return "SECRET-TOKEN", dict(exp=1000 + now[0] + 300)
        runtime.token = token
        with mock.patch.object(verify.time, "time", side_effect=lambda: 1000 + now[0]):
            result = verify.run_watch(runtime, first, seconds=600, interval=10, clock=lambda: now[0], sleeper=lambda value: now.__setitem__(0, now[0] + value))
        self.assertEqual(tokens, [0, 270, 540])
        self.assertEqual(result["token_refreshes"], 2)
        self.assertEqual(result["request_count"], 120)
        self.assertEqual(result["error_count"], 0)

    def test_failed_watch_cli_writes_summary_and_exits_nonzero_without_error_text(self):
        runtime = FakeRuntime()
        first = verify.run_smoke(runtime)
        summary = dict(status="failed", request_count=4, error_count=1, error_classes={"RuntimeError": 1}, latency_ms={"maximum": 3.5})
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "watch.json"
            stdout = io.StringIO()
            with mock.patch.object(verify, "Runtime", return_value=runtime), mock.patch.object(verify, "read_input", return_value=first), \
                    mock.patch.object(verify, "run_watch", side_effect=verify.WatchFailure(summary)), \
                    mock.patch.object(sys, "argv", ["vm_lab_verify.py", "watch", "--input", "/root/baseline.json", "--seconds", "1", "--output", str(output)]), \
                    mock.patch.object(sys, "stdout", stdout):
                with self.assertRaises(SystemExit) as caught:
                    verify.main()
            self.assertEqual(caught.exception.code, 1)
            self.assertEqual(json.loads(output.read_text())["request_count"], 4)
            self.assertEqual(json.loads(output.read_text())["error_count"], 1)
            self.assertNotIn("SECRET", stdout.getvalue())

    def test_probe_stdin_is_bounded_and_database_negative_needs_password_error(self):
        bundle = peer_bundle()
        self.assertEqual(verify.read_input("-", io.BytesIO(json.dumps(bundle).encode())), bundle)
        with self.assertRaises(ValueError):
            verify.read_input("-", io.BytesIO(b"x" * (verify.MAX_INPUT_BYTES + 1)))
        runtime = FakeRuntime()
        runtime.probe_database_password = lambda role, password: False
        with self.assertRaisesRegex(RuntimeError, "database"):
            verify.run_probe(runtime, bundle)

    def test_database_probe_passwords_are_env_only_and_connection_errors_do_not_pass(self):
        runtime = object.__new__(verify.Runtime)
        runtime.credentials = dict(api_password="LOCAL-SECRET")
        captured = []
        def run(command, **kwargs):
            captured.append((command, kwargs))
            if len(captured) == 1:
                return mock.Mock(returncode=0, stdout="1\n", stderr="")
            return mock.Mock(returncode=2, stdout="", stderr='FATAL:  password authentication failed for user "wms_api"')
        with mock.patch.object(verify.subprocess, "run", side_effect=run):
            self.assertTrue(runtime.probe_database_password("wms_api", "FOREIGN-SECRET"))
        self.assertNotIn("SECRET", " ".join(captured[1][0]))
        self.assertEqual(captured[1][1]["env"]["PGPASSWORD"], "FOREIGN-SECRET")
        self.assertIn("--env", captured[1][0])
        self.assertIn("PGPASSWORD", captured[1][0])
        with mock.patch.object(verify.subprocess, "run", side_effect=[mock.Mock(returncode=0, stdout="1\n", stderr=""), mock.Mock(returncode=2, stdout="", stderr="connection refused")]):
            self.assertFalse(runtime.probe_database_password("wms_api", "FOREIGN-SECRET"))


if __name__ == "__main__":
    unittest.main()
