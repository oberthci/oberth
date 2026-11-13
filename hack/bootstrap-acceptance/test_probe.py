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

    def test_large_inventory_uses_literal_bytes_across_capture_boundaries(self):
        values = [b'fixture-credential-%08d' % i for i in range(6000)]
        value = b'private.[key]+$'
        values.append(value)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'capture.stderr'
            path.write_bytes(b'privateXkkkk')
            probe.scan_logs([path], values)
            path.write_bytes(b'x' * (65536 - 4) + value)
            with self.assertRaises(probe.Refused) as caught:
                probe.scan_logs([path], values)
            self.assertEqual(caught.exception.channel, 'stderr')
            path.write_text('\n'.join(json.dumps(dict(
                Package='github.com/oberthci/oberth/internal/installer', Test='TestFixture',
                Output=part)) for part in ('private.[', 'key]+$')))
            with self.assertRaises(probe.Refused) as caught:
                probe.scan_go_events(path, values)
            self.assertEqual(caught.exception.channel, 'go-output')
            self.assertEqual(caught.exception.test, 'TestFixture')
            self.assertNotIn(value.decode(), str(caught.exception))
            for inventory in ([], [b'']):
                with self.subTest(inventory=inventory), self.assertRaises(probe.Refused):
                    probe.scan_go_events(path, inventory)

    def test_decoded_scan_keeps_completing_event_identity_across_blocks(self):
        value = b'fixture-private-credential'
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events'
            events = [
                dict(Package='github.com/oberthci/oberth/internal/installer',
                     Test='TestPrefix', Output='x' * (65536 - 8) + value[:8].decode()),
                dict(Package='github.com/oberthci/oberth/internal/releaseverify',
                     Test='TestCompletingEvent', Output=value[8:].decode()),
                dict(Package='github.com/oberthci/oberth/cmd/oberth',
                     Test='TestLaterEvent', Output='public trailing output'),
            ]
            path.write_text('\n'.join(map(json.dumps, events)))
            with self.assertRaises(probe.Refused) as caught:
                probe.scan_go_events(path, [value, value + b'-longer'])
            self.assertEqual(caught.exception.package, events[1]['Package'])
            self.assertEqual(caught.exception.test, 'TestCompletingEvent')
            self.assertEqual(caught.exception.channel, 'go-output')

    def test_literal_scan_follows_overlapping_prefixes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'capture'
            path.write_bytes(b'abcdefghiJ')
            with self.assertRaises(probe.Refused):
                probe.scan_logs([path], [b'abcdexyz', b'bcdefghiJ'])

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

class FixtureTransportBoundary(unittest.TestCase):
    def test_every_mutating_verb_is_recorded_and_rejected(self):
        import sys
        import ssl
        import urllib.request
        import urllib.error
        sys.path.insert(0, str(Path(__file__).resolve().parent))
        from fixtures import tls_server
        from ci import assert_no_mutations, Refused as FixtureRefused
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runner = probe.Probe(root, root, 'selected', root / 'unused')
            seen = []
            def respond(method, path, headers, body):
                return (200 if method == 'GET' else 403), {}
            with tls_server(runner, 'methods', respond, record=lambda method, path: seen.append((method, path)), allowed_methods={'GET'}) as (url, cert, _):
                context = ssl.create_default_context(cafile=str(cert))
                for method in ('POST', 'PUT', 'PATCH', 'DELETE'):
                    with self.subTest(method=method):
                        seen.clear()
                        request = urllib.request.Request(url, data=b'{}', method=method)
                        with self.assertRaises(urllib.error.HTTPError) as error:
                            urllib.request.urlopen(request, context=context, timeout=3)
                        self.assertEqual(error.exception.code, 403)
                        error.exception.close()
                        self.assertEqual(seen, [(method, '/')])
                        with self.assertRaises(FixtureRefused):
                            assert_no_mutations(seen)

    def test_stalled_tls_peer_does_not_hang_shutdown(self):
        import socket
        import time
        import sys
        sys.path.insert(0, str(Path(__file__).resolve().parent))
        from fixtures import tls_server
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runner = probe.Probe(root, root, 'selected', root / 'unused')
            started = time.monotonic()
            with tls_server(runner, 'stalled', lambda *args: (200, {})) as (url, _, _):
                port = int(url.rsplit(':', 1)[1])
                peer = socket.create_connection(('127.0.0.1', port), timeout=2)
                time.sleep(0.05)
            peer.close()
            self.assertLess(time.monotonic() - started, 4)


if __name__ == '__main__':
    unittest.main()
