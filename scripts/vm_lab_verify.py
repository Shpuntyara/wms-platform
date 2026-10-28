"""Guest-local, repeatable verification for a dedicated synthetic WMS lab.

Execute as root inside a newly installed laboratory guest. All business fixtures
have fresh UUIDs. HTTP credentials remain in memory or an explicit root-only file.
"""
import argparse
import base64
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import ssl
import stat
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

INVARIANTS = ("operations_once", "one_document", "one_reservation", "balanced_quantity", "events_once", "document_unchanged_stock")


class VerificationFailure(RuntimeError):
    """A failure with a static diagnostic selected by this program."""
    def __init__(self, check):
        self.check = check
        super().__init__(check)


def require(condition, message):
    if not condition:
        raise VerificationFailure(message)


def sql_literal(value):
    if not isinstance(value, str) or "\x00" in value:
        raise ValueError("Invalid SQL text value")
    return "'" + value.replace("'", "''") + "'"


def validate_identity(identity):
    uuid.UUID(identity["instance_id"])
    address = str(ipaddress.IPv4Address(identity["address"]))
    issuer = urllib.parse.urlsplit(identity["issuer"])
    if (issuer.scheme != "https" or issuer.netloc != address or issuer.query or issuer.fragment
            or not issuer.path.startswith("/auth/realms/wms-") or issuer.path.endswith("/")
            or not identity["audience"].startswith("wms-api-")):
        raise ValueError("Identity requires a verified HTTPS origin and an instance-specific realm")
    return "https://" + address


def jwt_parts(token):
    """Decode public metadata only; the API remains responsible for JWT validation."""
    parts = token.split(".")
    if len(parts) != 3 or len(token) > 16384:
        raise ValueError("Malformed token metadata")
    decoded = [json.loads(base64.urlsafe_b64decode(part + "=" * (-len(part) % 4))) for part in parts[:2]]
    if not all(isinstance(part, dict) for part in decoded):
        raise ValueError("Malformed token metadata")
    return decoded


def checked_claims(token, issuer, audience):
    header, claims = jwt_parts(token)
    audiences = claims.get("aud", [])
    if isinstance(audiences, str):
        audiences = [audiences]
    if (header.get("alg") != "RS256" or not header.get("kid") or claims.get("iss") != issuer
            or audience not in audiences or not claims.get("sub") or not isinstance(claims["sub"], str)
            or not isinstance(claims.get("exp"), int) or claims["exp"] < time.time() + 30):
        raise ValueError("Unexpected or expired token metadata")
    return claims


def read_protected(path):
    path = Path(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or (os.name == "posix" and (info.st_uid != 0 or info.st_mode & 0o077)):
        raise ValueError("Secret input must be a regular root-owned file with no group/other access")
    return json.loads(path.read_text())


def write_protected(path, value):
    # O_EXCL also rejects an existing symlink; never replace a previous receipt/bundle.
    descriptor = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as output:
        json.dump(value, output, indent=2)
        output.write("\n")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, new_url):
        raise RuntimeError("Unexpected HTTP redirect")


class Runtime:
    def __init__(self, config=Path("/etc/wms")):
        if os.name != "posix" or os.geteuid() != 0:
            raise RuntimeError("Run inside the laboratory guest as root")
        self.identity = json.loads((config / "identity.json").read_text())
        self.origin = validate_identity(self.identity)
        self.credentials = read_protected(config / "secrets.json")
        if self.credentials["instance_id"] != self.identity["instance_id"]:
            raise ValueError("Instance identity and secret scope differ")
        self.ca_path = config / "tls/ca.crt"
        context = ssl.create_default_context(cafile=str(self.ca_path))
        self.opener = urllib.request.build_opener(urllib.request.HTTPSHandler(context=context), NoRedirect())

    def request(self, method, path, *, data=None, headers=None):
        url = path if path.startswith("https://") else self.origin + path
        if urllib.parse.urlsplit(url).netloc != urllib.parse.urlsplit(self.origin).netloc:
            raise ValueError("HTTP request origin differs from this installation")
        request = urllib.request.Request(url, data=data, headers=headers or {}, method=method)
        try:
            response = self.opener.open(request, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            payload = response.read(1024 * 1024 + 1)
            if len(payload) > 1024 * 1024:
                raise RuntimeError("HTTP verification response too large")
            try:
                body = json.loads(payload) if payload else {}
            except (UnicodeDecodeError, json.JSONDecodeError):
                body = {}
            return response.code, body

    def token(self):
        values = dict(grant_type="client_credentials", client_id=self.identity["audience"], client_secret=self.credentials["oidc_client_secret"])
        status, body = self.request("POST", self.identity["issuer"] + "/protocol/openid-connect/token",
            data=urllib.parse.urlencode(values).encode(), headers={"Content-Type": "application/x-www-form-urlencoded"})
        require(status == 200 and isinstance(body.get("access_token"), str), "Local OIDC client authentication failed")
        token = body["access_token"]
        return token, checked_claims(token, self.identity["issuer"], self.identity["audience"])

    def api(self, method, path, token, body=None, command=None):
        headers = {"Authorization": "Bearer " + token}
        if body is not None:
            headers["Content-Type"] = "application/json"
        if command:
            headers["Idempotency-Key"] = command
        status, response = self.request(method, path, data=None if body is None else json.dumps(body).encode(), headers=headers)
        require(status == (200 if method == "GET" else 201), "Business API returned an unexpected status")
        require(isinstance(response.get("data"), dict), "Business API returned an unexpected envelope")
        return response["data"]

    def sql(self, text):
        command = ["docker", "exec", "-i", "wms-postgres", "psql", "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1", "-U", "wms_admin", "-d", "wms"]
        result = subprocess.run(command, input=text, text=True, encoding="utf-8", capture_output=True, timeout=45)
        require(result.returncode == 0, "Guest database verification failed")
        return result.stdout.strip()

    def fingerprints(self, token):
        issuer = self.identity["issuer"]
        status, discovery = self.request("GET", issuer + "/.well-known/openid-configuration")
        require(status == 200 and discovery.get("issuer") == issuer, "OIDC discovery identity mismatch")
        jwks_uri = discovery.get("jwks_uri", "")
        require(jwks_uri == issuer + "/protocol/openid-connect/certs", "Unexpected OIDC key endpoint")
        status, keys = self.request("GET", jwks_uri)
        kid = jwt_parts(token)[0]["kid"]
        selected = [key for key in keys.get("keys", []) if key.get("kid") == kid and key.get("kty") == "RSA"]
        require(status == 200 and len(selected) == 1, "OIDC signing public key missing or ambiguous")
        public = {name: selected[0][name] for name in ("e", "kty", "n")}
        thumbprint = hashlib.sha256(json.dumps(public, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        ca = ssl.PEM_cert_to_DER_cert(self.ca_path.read_text())
        return dict(tls_ca_sha256=hashlib.sha256(ca).hexdigest(), oidc_jwk_sha256=thumbprint)


def fixture_sql(ids, claims):
    q = {name: sql_literal(value) for name, value in ids.items()}
    issuer, subject = sql_literal(claims["iss"]), sql_literal(claims["sub"])
    return f"""BEGIN;
SET LOCAL standard_conforming_strings=on;
INSERT INTO platform.organizations(id,code,name) VALUES({q['organization_id']},{q['run_id']},'Hyper-V synthetic verification');
INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES({q['warehouse_id']},{q['organization_id']},'lab');
INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES({q['owner_id']},{q['organization_id']},'lab');
INSERT INTO catalog.units(code,dimension) VALUES({q['run_id']},'count');
INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking)
 VALUES({q['item_id']},{q['organization_id']},'lab',{q['run_id']},0,'none');
INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind)
 VALUES({q['location_id']},{q['organization_id']},{q['warehouse_id']},'lab','storage');
INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id)
 VALUES({q['position_id']},{q['organization_id']},{q['warehouse_id']},{q['location_id']});
INSERT INTO inventory.stock_balances(id,organization_id,warehouse_id,owner_id,item_id,position_id,stock_state,on_hand)
 VALUES({q['balance_id']},{q['organization_id']},{q['warehouse_id']},{q['owner_id']},{q['item_id']},{q['position_id']},'available',10);
INSERT INTO platform.actors(id,issuer,subject) VALUES({q['actor_id']},{issuer},{subject}) ON CONFLICT(issuer,subject) DO NOTHING;
INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission)
 SELECT a.id,{q['organization_id']},{q['warehouse_id']},{q['owner_id']},permission
 FROM platform.actors a CROSS JOIN unnest(ARRAY['document.create','document.transition','document.read','reservation.create','reservation.release','reservation.read','stock.read','operation.read']) permission
 WHERE a.issuer={issuer} AND a.subject={subject};
COMMIT;
"""


def reconcile_sql(ids, commands, document_id, reservation_id):
    org, balance = sql_literal(ids["organization_id"]), sql_literal(ids["balance_id"])
    document, reservation = sql_literal(document_id), sql_literal(reservation_id)
    keys = ",".join(sql_literal(command) + "::uuid" for command in commands)
    return f"""-- reconcile lab fixture
SELECT jsonb_build_object(
'operations_once',(SELECT count(*)=4 AND bool_and(id=ANY(ARRAY[{keys}]) AND completed_at IS NOT NULL AND result IS NOT NULL) FROM platform.operations WHERE organization_id={org}),
'one_document',(SELECT count(*)=1 FROM warehouse.documents WHERE organization_id={org}),
'one_reservation',(SELECT count(*)=1 AND bool_and(id={reservation} AND quantity=3 AND active_qty=0 AND released_qty=3 AND consumed_qty=0) FROM inventory.reservations WHERE organization_id={org}),
'balanced_quantity',(SELECT on_hand=10 AND reserved_qty=0 AND reserved_qty=(SELECT coalesce(sum(active_qty),0) FROM inventory.reservations WHERE balance_id={balance}) FROM inventory.stock_balances WHERE id={balance}),
'events_once',(SELECT count(*)=2 AND count(*) FILTER(WHERE kind='reserved' AND quantity=3)=1 AND count(*) FILTER(WHERE kind='released' AND quantity=3)=1 FROM inventory.reservation_events WHERE reservation_id={reservation}),
'document_unchanged_stock',(SELECT d.state='active' AND d.version=4 AND count(l.id)=1 AND bool_and(l.requested_qty=10 AND l.reserved_qty=0 AND l.fulfilled_qty=0) FROM warehouse.documents d JOIN warehouse.document_lines l ON l.document_id=d.id WHERE d.id={document} GROUP BY d.id)
);
"""


def run_smoke(runtime):
    require(runtime.request("GET", "/readyz") == (200, {"status": "ready"}), "Public TLS readiness failed")
    require(runtime.request("GET", "/monitor/")[0] == 401, "Monitoring must require authentication")
    actual = runtime.sql("SELECT id::text FROM hardened_private.installation WHERE singleton;")
    require(actual == runtime.identity["instance_id"], "Database installation UUID mismatch")
    token, claims = runtime.token()
    fingerprints = runtime.fingerprints(token)
    ids = {name: str(uuid.uuid4()) for name in ("run_id", "organization_id", "warehouse_id", "owner_id", "item_id", "location_id", "position_id", "balance_id", "actor_id")}
    commands = [str(uuid.uuid4()) for _ in range(4)]
    runtime.sql(fixture_sql(ids, claims))
    scope = {name: ids[name] for name in ("organization_id", "warehouse_id")}
    query = "?" + urllib.parse.urlencode(scope)
    create = dict(scope, owner_id=ids["owner_id"], kind="shipment", number="lab-" + ids["run_id"], lines=[dict(item_id=ids["item_id"], quantity="10")])

    def command(path, body, key):
        result = runtime.api("POST", path, token, body, key)
        replay = runtime.api("POST", path, token, body, key)
        require(result == replay and result.get("operation_id") == key, "Command replay changed the committed result")
        return result

    created = command("/api/v1/documents", create, commands[0])
    document_id = str(uuid.UUID(created["document_id"]))
    line_id = str(uuid.UUID(created["lines"][0]["id"]))
    activated = command(f"/api/v1/documents/{document_id}/transitions", dict(scope, expected_document_version=1, state="active"), commands[1])
    require(activated["document_version"] == 2, "Document activation version mismatch")
    reserved = command("/api/v1/reservations", dict(scope, expected_document_version=2, quantity="3", document_line_id=line_id, balance_id=ids["balance_id"]), commands[2])
    reservation_id = str(uuid.UUID(reserved["reservation_id"]))
    require(reserved["document_version"] == 3, "Reservation version mismatch")
    released = command(f"/api/v1/reservations/{reservation_id}/releases", dict(scope, expected_document_version=3, quantity="3", reason="Hyper-V lab verification"), commands[3])
    require(released["document_version"] == 4, "Release version mismatch")
    document = runtime.api("GET", f"/api/v1/documents/{document_id}" + query, token)
    balance = runtime.api("GET", f"/api/v1/stock-balances/{ids['balance_id']}" + query, token)
    reservation = runtime.api("GET", f"/api/v1/reservations/{reservation_id}" + query, token)
    require(document["id"] == document_id and document["version"] == 4 and document["state"] == "active", "Document read did not reflect committed commands")
    require(balance["on_hand"] == "10.000000" and balance["reserved_qty"] == "0.000000", "Stock read did not reconcile")
    require(reservation["active_qty"] == "0.000000" and reservation["released_qty"] == "3.000000", "Reservation read did not reconcile")
    invariants = json.loads(runtime.sql(reconcile_sql(ids, commands, document_id, reservation_id)))
    require(set(invariants) == set(INVARIANTS) and all(value is True for value in invariants.values()), "Database reconciliation failed")
    return dict(status="verified", run_id=ids["run_id"], instance_id=actual, issuer=runtime.identity["issuer"], audience=runtime.identity["audience"],
                fingerprints=fingerprints, fixture=ids, command_ids=commands, document_id=document_id, reservation_id=reservation_id,
                invariants=invariants, tls_readiness=True, monitoring_requires_auth=True, completed_at=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))


def run_probe(runtime, bundle):
    source_id = str(uuid.UUID(bundle["source_instance_id"]))
    if source_id == runtime.identity["instance_id"]:
        raise ValueError("Peer probe must come from a different installation")
    checked_claims(bundle["access_token"], bundle["source_issuer"], bundle["client_id"])
    form = urllib.parse.urlencode(dict(grant_type="client_credentials", client_id=bundle["client_id"], client_secret=bundle["client_secret"])).encode()
    status, body = runtime.request("POST", runtime.identity["issuer"] + "/protocol/openid-connect/token", data=form,
                                   headers={"Content-Type": "application/x-www-form-urlencoded"})
    require(status in (400, 401) and body.get("error") == "invalid_client", "Foreign client credentials were not explicitly rejected")
    query = urllib.parse.urlencode(dict(organization_id=str(uuid.uuid4()), warehouse_id=str(uuid.uuid4())))
    status, _ = runtime.request("GET", "/api/v1/documents?" + query, headers={"Authorization": "Bearer " + bundle["access_token"]})
    require(status == 401, "Foreign OIDC token was not rejected as unauthenticated")
    return dict(status="verified", source_instance_id=source_id, target_instance_id=runtime.identity["instance_id"], foreign_client_rejected=True, foreign_token_rejected=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("smoke", "export-probe", "probe"))
    parser.add_argument("--config", type=Path, default=Path("/etc/wms"))
    parser.add_argument("--output", type=Path)
    parser.add_argument("--input", type=Path)
    args = parser.parse_args()
    try:
        runtime = Runtime(args.config)
        if args.action == "smoke":
            receipt = run_smoke(runtime)
        elif args.action == "export-probe":
            if not args.output:
                raise ValueError("export-probe requires an explicit protected output file")
            token, _ = runtime.token()
            bundle = dict(source_instance_id=runtime.identity["instance_id"], source_issuer=runtime.identity["issuer"], client_id=runtime.identity["audience"],
                          client_secret=runtime.credentials["oidc_client_secret"], access_token=token)
            write_protected(args.output, bundle)
            receipt = dict(status="exported", source_instance_id=runtime.identity["instance_id"], protected_file=str(args.output))
        else:
            if not args.input:
                raise ValueError("probe requires a root-only peer bundle file")
            receipt = run_probe(runtime, read_protected(args.input))
        if args.output and args.action != "export-probe":
            write_protected(args.output, receipt)
        print(json.dumps(receipt, sort_keys=True))
    except Exception as error:
        # Never echo exception text: HTTP/SQL libraries may embed sensitive request material.
        receipt = dict(status="failed", action=args.action, error_class=type(error).__name__)
        if isinstance(error, VerificationFailure):
            receipt["check"] = error.check
        print(json.dumps(receipt))
        raise SystemExit(1)


if __name__ == "__main__":
    main()
