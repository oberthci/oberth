"""Loopback TLS/API fixtures; never contact a cluster or public release service."""
import contextlib
import http.server
import json
import ssl
import threading


@contextlib.contextmanager
def tls_server(probe, name, respond, client_ca=None):
    cert, key = probe.work / (name + '.crt'), probe.work / (name + '.key')
    probe.run(name + '-key', ['openssl', 'req', '-x509', '-newkey', 'ec',
              '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes', '-days', '1',
              '-subj', '/CN=localhost', '-addext', 'subjectAltName=DNS:localhost',
              '-addext', 'basicConstraints=critical,CA:TRUE',
              '-keyout', str(key), '-out', str(cert)])

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def dispatch(self):
            body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
            status, value = respond(self.command, self.path, self.headers, body)
            raw = json.dumps(value).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        do_GET = dispatch
        do_POST = dispatch

    server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(cert, key)
    if client_ca:
        tls.load_verify_locations(cafile=client_ca)
        tls.verify_mode = ssl.CERT_REQUIRED
    server.socket = tls.wrap_socket(server.socket, server_side=True)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield 'https://localhost:' + str(server.server_port), cert, key
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def private_values(work):
    values = []
    for path in work.rglob('*.key'):
        raw = path.read_bytes()
        values.append(raw)
        values.extend(line for line in raw.splitlines() if line and not line.startswith(b'-----'))
    return values
