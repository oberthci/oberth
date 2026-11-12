"""Loopback TLS/API fixtures; never contact a cluster or public release service."""
import contextlib
import http.server
import json
import ssl
import socket
import threading


@contextlib.contextmanager
def tls_server(probe, name, respond, client_ca=None, record=None, allowed_methods=None):
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
            if record:
                record(self.command, self.path)
            if allowed_methods is not None and self.command not in allowed_methods:
                self.send_error(403)
                return
            length = int(self.headers.get('Content-Length', '0'))
            if length < 0 or length > 1024 * 1024:
                self.send_error(413)
                return
            body = self.rfile.read(length)
            status, value = respond(self.command, self.path, self.headers, body)
            raw = json.dumps(value).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def handle(self):
            def expire():
                try:
                    self.connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            deadline = threading.Timer(2, expire)
            deadline.daemon = True
            deadline.start()
            try:
                super().handle()
            finally:
                deadline.cancel()

        def __getattr__(self, name):
            if name.startswith('do_'):
                return self.dispatch
            raise AttributeError(name)

    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(cert, key)
    if client_ca:
        tls.load_verify_locations(cafile=client_ca)
        tls.verify_mode = ssl.CERT_REQUIRED

    class Server(http.server.ThreadingHTTPServer):
        daemon_threads = True
        block_on_close = False

        def get_request(self):
            connection, address = self.socket.accept()
            connection.settimeout(1)
            try:
                secured = tls.wrap_socket(connection, server_side=True)
            except (OSError, ValueError):
                connection.close()
                raise
            return secured, address

        def handle_error(self, request, client_address):
            # Fixtures never expose request bodies or handshake diagnostics.
            pass

    server = Server(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': 0.05}, daemon=True)
    thread.start()
    try:
        yield 'https://localhost:' + str(server.server_port), cert, key
    finally:
        shutdown = threading.Thread(target=server.shutdown, daemon=True)
        shutdown.start()
        shutdown.join(2)
        server.server_close()
        thread.join(2)
        if shutdown.is_alive() or thread.is_alive():
            from probe import Refused
            raise Refused('fixture HTTP server teardown exceeded its bound', stage='fixture-http', check='http-teardown')


def private_values(work):
    values = []
    for path in work.rglob('*.key'):
        raw = path.read_bytes()
        values.append(raw)
        values.extend(line for line in raw.splitlines() if line and not line.startswith(b'-----'))
    return values
