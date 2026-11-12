import base64
import importlib.util
import json
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

    def test_real_mtls_requires_selected_ca_and_keeps_private_key_out_of_logs(self):
        import sys
        sys.path.insert(0, str(Path(__file__).resolve().parent))
        from fixtures import private_values, tls_server
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runner = probe.Probe(root, root, 'disposable', root / 'config')
            for name in ('client', 'other'):
                runner.run(name + '-key', ['openssl', 'req', '-x509', '-newkey', 'ec',
                           '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes', '-days', '1',
                           '-subj', '/CN=fixture-client', '-keyout', str(root / (name + '.key')),
                           '-out', str(root / (name + '.crt'))])
            received = []

            def respond(method, path, headers, raw):
                received.append(headers.get('Authorization'))
                request = json.loads(raw)
                return 200, {'jsonrpc': '2.0', 'id': request['id'],
                             'result': {'tools': [{'name': 'system_status'}]}}

            with tls_server(runner, 'server', respond, root / 'client.crt') as (url, ca, _):
                result = runner.mcp(url + '/mcp', ca, root / 'client.crt', root / 'client.key',
                                    'tools/list', {}, 1)
                self.assertEqual(result['tools'][0]['name'], 'system_status')
                self.assertEqual(received, [None])
                with self.assertRaises(probe.Refused):
                    runner.mcp(url + '/mcp', root / 'other.crt', root / 'client.crt',
                               root / 'client.key', 'tools/list', {}, 2)
                with self.assertRaises(probe.Refused):
                    runner.mcp(url + '/mcp', ca, root / 'other.crt', root / 'other.key',
                               'tools/list', {}, 3)
                self.assertEqual(received, [None])
            probe.scan_logs(runner.logs, private_values(root))

    def test_redirect_is_refused(self):
        with self.assertRaises(probe.Refused):
            probe.NoRedirect().redirect_request(None, None, 302, '', {}, 'https://elsewhere.invalid')


if __name__ == '__main__':
    unittest.main()
