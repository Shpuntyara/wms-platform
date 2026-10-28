"""Offline behavior tests for the guest-side verifier. No live service is implied."""
import base64
import json
from pathlib import Path
import sys
import tempfile
import time
import unittest
import uuid

sys.path.insert(0, str(Path(__file__).parent))
import vm_lab_verify as verify


class FakeRuntime:
    def __init__(self, *, replay_changed=False, invariant=False, foreign_client=401, foreign_token=401):
        self.identity = dict(instance_id=str(uuid.uuid4()), issuer="https://192.0.2.1/auth/realms/wms-test", audience="wms-api-test")
        self.credentials = {"oidc_client_secret": "SECRET-CLIENT"}
        self.replay_changed, self.invariant = replay_changed, invariant
        self.foreign_client, self.foreign_token = foreign_client, foreign_token
        self.commands, self.queries = {}, []
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
        if method == "GET":
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


def peer_bundle():
    header = base64.urlsafe_b64encode(json.dumps(dict(alg="RS256", kid="public-key")).encode()).decode().rstrip("=")
    payload = base64.urlsafe_b64encode(json.dumps(dict(iss="https://192.0.2.2/auth/realms/wms-other", aud="wms-api-other", sub="peer", exp=int(time.time()) + 600)).encode()).decode().rstrip("=")
    return dict(source_instance_id=str(uuid.uuid4()), source_issuer="https://192.0.2.2/auth/realms/wms-other",
                client_id="wms-api-other", client_secret="PEER-SECRET", access_token=header + "." + payload + ".opaque")


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


if __name__ == "__main__":
    unittest.main()
