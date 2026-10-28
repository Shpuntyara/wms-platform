"""Temporary loopback Viewer proxy for real lab screenshots; validates guest TLS.

Does not change Windows/browser trust stores. Stop the process after recording.
Upstream Viewer credentials stay in memory and are never sent to the browser.
"""
import argparse
import base64
import http.server
import ipaddress
import json
from pathlib import Path
import re
import ssl
import subprocess
import urllib.error
import urllib.parse
import urllib.request


def allowed(method, path):
    decoded = path
    for _ in range(3):
        decoded = urllib.parse.unquote(decoded)
    parsed = urllib.parse.urlsplit(decoded)
    if (parsed.scheme or parsed.netloc or not parsed.path.startswith('/monitor/')
            or '\\' in decoded or '..' in parsed.path.split('/') or '\x00' in decoded):
        return False
    if method == 'GET':
        return True
    if method != 'POST' or parsed.path != '/monitor/api/ds/query':
        return False
    query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True, max_num_fields=4)
    return all(key in ('ds_type', 'requestId') and len(values) == 1
               and re.fullmatch('[A-Za-z0-9_-]{1,80}', values[0])
               for key, values in query.items())


def upstream_headers(client, credential, address):
    headers = {'Authorization': 'Basic ' + credential, 'Host': address, 'Accept-Encoding': 'identity'}
    if client.get('Content-Type'):
        headers['Content-Type'] = client['Content-Type']
    return headers


def rewrite(body, address, port):
    return body.replace(('https://' + address).encode(), ('http://127.0.0.1:' + str(port)).encode())


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, new_url):
        return None


def local_location(location, address, port):
    parsed = urllib.parse.urlsplit(location)
    if parsed.scheme or parsed.netloc:
        if parsed.scheme != 'https' or parsed.netloc != address:
            raise ValueError('Unexpected monitoring redirect origin')
    if not allowed('GET', parsed.path):
        raise ValueError('Unexpected monitoring redirect path')
    return rewrite(location.encode(), address, port).decode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--plan', type=Path, required=True)
    parser.add_argument('--vm', choices=['WMS-Lab-A', 'WMS-Lab-B'], required=True)
    parser.add_argument('--port', type=int, choices=[18081, 18082], required=True)
    args = parser.parse_args()
    plan = json.loads(args.plan.read_text(encoding='utf-8-sig'))
    vm = next(node for node in plan['vms'] if node['name'] == args.vm)
    receipt = json.loads(Path(vm['receipt_path']).read_text(encoding='utf-8-sig'))
    address = str(ipaddress.IPv4Address(receipt['ip_address']))
    command = ['ssh', '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=yes',
               '-o', 'UserKnownHostsFile=' + vm['known_hosts_path'], '-o', 'GlobalKnownHostsFile=NUL',
               '-o', 'HostKeyAlias=' + vm['instance_id'], '-i', vm['private_key_path'], 'wmsadmin@' + address]
    ca = subprocess.check_output(command + ['sudo cat /etc/wms/tls/ca.crt'], stderr=subprocess.PIPE).decode()
    password = subprocess.check_output(command + [
        "sudo python3 -c \"import json;print(json.load(open('/etc/wms/secrets.json'))['monitor_password'])\""],
        stderr=subprocess.PIPE).decode().strip()
    credential = base64.b64encode(('wms-viewer:' + password).encode()).decode()
    context = ssl.create_default_context(cadata=ca)
    opener = urllib.request.build_opener(urllib.request.HTTPSHandler(context=context), urllib.request.ProxyHandler({}), NoRedirect())

    class Viewer(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def forward(self):
            if not allowed(self.command, self.path):
                self.send_error(403)
                return
            length = int(self.headers.get('Content-Length', '0'))
            if not 0 <= length <= 1024 * 1024:
                self.send_error(413)
                return
            data = self.rfile.read(length) if self.command == 'POST' else None
            request = urllib.request.Request('https://' + address + self.path, data=data,
                headers=upstream_headers(self.headers, credential, address), method=self.command)
            try:
                try:
                    response = opener.open(request, timeout=30)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    body = response.read(20 * 1024 * 1024 + 1)
                    if len(body) > 20 * 1024 * 1024:
                        raise ValueError('Response too large')
                    content_type = response.headers.get('Content-Type', 'application/octet-stream')
                    if any(kind in content_type for kind in ('text', 'json', 'javascript')):
                        body = rewrite(body, address, args.port)
                    location = response.headers.get('Location')
                    if location:
                        location = local_location(location, address, args.port)
                    self.send_response(response.status)
                    self.send_header('Content-Type', content_type)
                    if location:
                        self.send_header('Location', location)
                    self.send_header('Content-Length', str(len(body)))
                    self.send_header('Cache-Control', 'no-store')
                    self.send_header('X-Content-Type-Options', 'nosniff')
                    self.end_headers()
                    self.wfile.write(body)
            except (OSError, ValueError, urllib.error.URLError):
                self.send_error(502, 'Verified guest monitoring unavailable')

        do_GET = forward
        do_POST = forward

    server = http.server.ThreadingHTTPServer(('127.0.0.1', args.port), Viewer)
    print(json.dumps({'vm': args.vm, 'url': 'http://127.0.0.1:' + str(args.port) + '/monitor/d/wms-operations',
                      'guest_tls_verified': True, 'scope': 'temporary local Viewer only'}), flush=True)
    server.serve_forever()


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        raise SystemExit('Monitoring proxy unavailable: ' + type(error).__name__) from None
