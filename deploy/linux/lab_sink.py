#!/usr/bin/env python3
"""LAB ONLY HMAC-verified, durable/deduplicated event witness; not an ERP adapter."""
import base64
import hashlib
import hmac
import http.server
import os
import sqlite3
import time
import uuid


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass  # Never log payloads, HMACs or headers.

    def do_POST(self):
        try:
            size = int(self.headers.get('Content-Length', '0'))
            stamp = self.headers.get('X-WMS-Timestamp', '')
            event = str(uuid.UUID(self.headers.get('X-WMS-Event-ID', '')))
            if self.path != '/lab/events' or not 0 < size <= 1 << 20 or abs(time.time() - int(stamp)) > 300:
                raise ValueError('invalid request')
            body = self.rfile.read(size)
            expected = hmac.new(KEY, (stamp + '\n' + event + '\n').encode() + body, hashlib.sha256).hexdigest()
            if len(body) != size or not hmac.compare_digest(expected, self.headers.get('X-WMS-Signature', '')):
                raise ValueError('invalid signature')
            digest = hashlib.sha256(body).hexdigest()
            with sqlite3.connect(DATABASE, timeout=10) as connection:
                connection.execute('PRAGMA synchronous=FULL')
                connection.execute('CREATE TABLE IF NOT EXISTS events (id TEXT PRIMARY KEY, payload_sha256 TEXT NOT NULL, received_at INTEGER NOT NULL)')
                existing = connection.execute('SELECT payload_sha256 FROM events WHERE id=?', (event,)).fetchone()
                if existing and existing[0] != digest:
                    self.send_error(409)
                    return
                connection.execute('INSERT OR IGNORE INTO events VALUES (?,?,?)', (event, digest, int(time.time())))
            self.send_response(204)
            self.end_headers()
        except (ValueError, sqlite3.Error):
            self.send_error(403)


if __name__ == '__main__':
    KEY = base64.b64decode(os.environ['WMS_OUTBOX_HMAC_KEY'], validate=True)
    if len(KEY) < 32:
        raise ValueError('invalid signing key')
    DATABASE = '/var/lib/wms-lab-sink/events.sqlite3'
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 8091), Handler)
    server.timeout = 10
    server.serve_forever()
