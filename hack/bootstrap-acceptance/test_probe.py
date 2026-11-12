import base64
import importlib.util
import http.server
import json
import shutil
import ssl
import subprocess
import threading
from pathlib import Path
import secrets
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('probe', Path(__file__).with_name('probe.py'))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class AcceptanceAssertions(unittest.TestCase):
    def test_verify_rejects_partial_empty_wrong_context_and_duplicate(self):
        good = dict(version=1, ok=True, context='disposable', checks=[dict(code='tls', ok=True)])
        probe.verify_result(good, 'disposable')
        for changed in [dict(ok=False), dict(context='other'), dict(checks=[]),
                        dict(checks=[dict(code='tls', ok=False)]),
                        dict(checks=[dict(code='tls', ok=True)] * 2)]:
            with self.assertRaises(probe.Refused):
                probe.verify_result(good | changed, 'disposable')

    def test_scan_detects_boundary_and_decoded_private_material_without_disclosure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = secrets.token_hex(32).encode()
            ceremony = root / 'ceremony'
            ceremony.write_bytes(b'OBERTH_UPSTREAM_PRIVATE_KEY_BASE64=' + base64.b64encode(value) + b'\n')
            values = probe.secret_values(ceremony)
            log = root / 'output'
            log.write_bytes(b'x' * (65536 - 7) + value + b'\n')
            try:
                probe.scan_logs([log], values)
            except probe.Refused as error:
                self.assertNotIn(value.decode(), str(error))
            else:
                self.fail('boundary exposure was accepted')
            log.write_bytes(b'public status only\n')
            probe.scan_logs([log], values)
            with self.assertRaises(probe.Refused):
                probe.scan_logs([log], [])

    def test_capture_never_returns_failed_command_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runner = probe.Probe(root, root, 'disposable', root / 'config')
            with self.assertRaises(probe.Refused) as caught:
                runner.run('failure', ['/bin/sh', '-c', 'echo private-diagnostic >&2; exit 1'])
            self.assertNotIn('private-diagnostic', str(caught.exception))
            self.assertEqual(len(runner.logs), 2)
            self.assertNotIn('OBERTH_TOKEN', runner.env)

    @unittest.skipUnless(shutil.which('openssl'), 'OpenSSL prerequisite is unavailable')
    def test_real_https_requires_selected_ca_and_keeps_credentials_private(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ('server', 'other'):
                subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048',
                                '-nodes', '-days', '1', '-subj', '/CN=localhost',
                                '-addext', 'subjectAltName=DNS:localhost',
                                '-keyout', str(root / (name + '.key')),
                                '-out', str(root / (name + '.crt'))],
                               check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            credential = secrets.token_hex(32)
            received = []

            class Handler(http.server.BaseHTTPRequestHandler):
                def log_message(self, *args):
                    pass

                def do_POST(self):
                    received.append(self.headers.get('Authorization') == 'Bearer ' + credential)
                    request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                    body = json.dumps({'jsonrpc': '2.0', 'id': request['id'],
                                       'result': {'tools': [{'name': 'system_status'}]}}).encode()
                    self.send_response(200)
                    self.send_header('Content-Length', str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)

            server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
            tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            tls.load_cert_chain(root / 'server.crt', root / 'server.key')
            server.socket = tls.wrap_socket(server.socket, server_side=True)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                runner = probe.Probe(root, root, 'disposable', root / 'config')
                url = 'https://localhost:' + str(server.server_port) + '/mcp'
                result = runner.mcp(url, root / 'server.crt', credential, 'tools/list', {}, 1)
                self.assertEqual(result['tools'][0]['name'], 'system_status')
                self.assertEqual(received, [True])
                probe.scan_logs(runner.logs, [credential.encode()])
                with self.assertRaises(probe.Refused):
                    runner.mcp(url, root / 'other.crt', credential, 'tools/list', {}, 2)
                self.assertEqual(received, [True])
            finally:
                server.shutdown()
                server.server_close()
                thread.join()

    def test_redirect_is_refused(self):
        with self.assertRaises(probe.Refused):
            probe.NoRedirect().redirect_request(None, None, 302, '', {}, 'https://elsewhere.invalid')


if __name__ == '__main__':
    unittest.main()
