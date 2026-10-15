#!/usr/bin/env python3
"""Inert exchange fixtures: no cloud calls or production credential reads."""
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import ssl
import sys
import unittest
from unittest.mock import patch
import urllib.parse

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('wif', ROOT / 'scripts/release-wif-credential.py')
wif = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wif)

# Pick the first role (image-writer) as default for backward-compat tests.
_AUDIENCES = list(wif.ROLES.keys())
_DEFAULT_AUDIENCE = _AUDIENCES[0]
_DEFAULT_IAM = wif.ROLES[_DEFAULT_AUDIENCE]


def config_for_audience(audience):
    """Build a valid ADC config for the given audience."""
    return {
        'type': 'external_account', 'audience': audience,
        'subject_token_type': 'urn:ietf:params:oauth:token-type:jwt',
        'token_url': wif.STS, 'service_account_impersonation_url': wif.ROLES[audience],
        'credential_source': {'file': wif.TOKEN},
    }


CONFIG = config_for_audience(_DEFAULT_AUDIENCE)


class ExchangeTests(unittest.TestCase):
    def exchange(self, config=None, replies=None):
        config = CONFIG if config is None else config
        replies = replies or [{'token_type': 'Bearer', 'access_token': 'inert-federated'},
                              {'accessToken': 'inert-source'}, {'accessToken': 'inert-publisher'}]
        def read(path):
            return json.dumps(config).encode() if path == wif.ADC else b'inert.jwt.signature'
        with patch.dict(os.environ, {'OBERTH_RELEASE_WIF': '1', 'GOOGLE_APPLICATION_CREDENTIALS': wif.ADC}), \
                patch.object(wif, 'read', side_effect=read), patch.object(wif, 'post', side_effect=replies) as post:
            result = wif.credential(wif.REGISTRY)
        return result, post.call_args_list

    def test_exact_exchange_and_impersonation(self):
        result, calls = self.exchange()
        self.assertEqual(result, {'Username': 'oauth2accesstoken', 'Secret': 'inert-publisher'})
        self.assertEqual(len(calls), 3)
        url, body, headers = calls[0].args
        self.assertEqual(url, wif.STS)
        values = urllib.parse.parse_qs(body.decode())
        self.assertEqual(values['audience'], [CONFIG['audience']])
        self.assertEqual(values['subject_token'], ['inert.jwt.signature'])
        url, body, headers = calls[1].args
        self.assertEqual(url, _DEFAULT_IAM)
        self.assertEqual(headers['Authorization'], 'Bearer inert-federated')
        self.assertEqual(json.loads(body)['lifetime'], '600s')
        url, body, headers = calls[2].args
        self.assertEqual(url, 'https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/oberth-release-publisher@skipopsmain.iam.gserviceaccount.com:generateAccessToken')
        self.assertEqual(headers['Authorization'], 'Bearer inert-source')
        self.assertEqual(json.loads(body), {'scope': ['https://www.googleapis.com/auth/cloud-platform'], 'lifetime': '600s'})

    def test_all_three_roles_exchange_to_paired_sa(self):
        """Each valid (audience, SA) pair must succeed and reach the correct IAM endpoint."""
        expected_targets = {
            'oberth-release-images': 'oberth-release-publisher',
            'oberth-release-reader': 'oberth-release-reader',
            'oberth-release-charts': 'oberth-chart-publisher',
        }
        for audience, expected_iam in wif.ROLES.items():
            with self.subTest(audience=audience):
                cfg = config_for_audience(audience)
                result, calls = self.exchange(config=cfg)
                self.assertEqual(result['Username'], 'oauth2accesstoken')
                self.assertEqual(len(calls), 3)
                self.assertEqual(calls[1].args[0], expected_iam)
                pool = audience.split('/workloadIdentityPools/')[1].split('/')[0]
                self.assertEqual(calls[2].args[0], 'https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/' + expected_targets[pool] + '@skipopsmain.iam.gserviceaccount.com:generateAccessToken')
                self.assertEqual(calls[2].args[2]['Authorization'], 'Bearer inert-source')
                self.assertEqual(json.loads(calls[2].args[1])['lifetime'], '600s')
                self.assertEqual(result['Secret'], 'inert-publisher')

    def test_cross_paired_audience_and_sa_rejected(self):
        """Publisher audience with reader SA URL must be refused before network."""
        reader_iam = wif.ROLES[_AUDIENCES[1]]
        cfg = copy.deepcopy(CONFIG)
        cfg['service_account_impersonation_url'] = reader_iam
        with patch.dict(os.environ, {'OBERTH_RELEASE_WIF': '1', 'GOOGLE_APPLICATION_CREDENTIALS': wif.ADC}), \
                patch.object(wif, 'read', return_value=json.dumps(cfg).encode()), \
                patch.object(wif, 'post') as post:
            with self.assertRaises(ValueError):
                wif.credential(wif.REGISTRY)
        post.assert_not_called()

    def test_all_cross_role_pairs_and_second_hop_overrides_refused(self):
        for audience in _AUDIENCES:
            for other in _AUDIENCES:
                if audience == other:
                    continue
                cfg = config_for_audience(audience)
                cfg['service_account_impersonation_url'] = wif.ROLES[other]
                with self.subTest(audience=audience, other=other), patch.dict(os.environ, {
                        'OBERTH_RELEASE_WIF': '1', 'GOOGLE_APPLICATION_CREDENTIALS': wif.ADC}), \
                        patch.object(wif, 'read', return_value=json.dumps(cfg).encode()), \
                        patch.object(wif, 'post') as post:
                    with self.assertRaises(ValueError): wif.credential(wif.REGISTRY)
                    post.assert_not_called()
        for key in ('delegates', 'oberth_impersonation_url'):
            cfg = copy.deepcopy(CONFIG)
            cfg[key] = wif.OBERTH_ROLES[_AUDIENCES[1]]
            with self.subTest(key=key), patch.dict(os.environ, {
                    'OBERTH_RELEASE_WIF': '1', 'GOOGLE_APPLICATION_CREDENTIALS': wif.ADC}), \
                    patch.object(wif, 'read', return_value=json.dumps(cfg).encode()), \
                    patch.object(wif, 'post') as post:
                with self.assertRaises(ValueError): wif.credential(wif.REGISTRY)
                post.assert_not_called()

    def test_second_hop_failure_never_returns_source_token_or_provider_body(self):
        for last in (ValueError('inert-sensitive-provider-body'), {}, {'accessToken': 'bad\nheader'}):
            output, error = io.StringIO(), io.StringIO()
            def read(path):
                return json.dumps(CONFIG).encode() if path == wif.ADC else b'inert.jwt.signature'
            with self.subTest(last=last), patch.dict(os.environ, {
                    'OBERTH_RELEASE_WIF': '1', 'GOOGLE_APPLICATION_CREDENTIALS': wif.ADC}), \
                    patch.object(sys, 'argv', ['helper', 'get']), \
                    patch.object(sys, 'stdin', io.StringIO(wif.REGISTRY + '\n')), \
                    patch.object(sys, 'stdout', output), patch.object(sys, 'stderr', error), \
                    patch.object(wif.os, 'fstat') as fstat, patch.object(wif, 'read', side_effect=read), \
                    patch.object(wif, 'post', side_effect=[
                        {'token_type': 'Bearer', 'access_token': 'inert-federated'},
                        {'accessToken': 'inert-source'}, last]) as post:
                fstat.return_value.st_mode = stat.S_IFIFO | 0o600
                self.assertEqual(wif.main(), 1)
                self.assertEqual(post.call_count, 3)
            self.assertEqual(output.getvalue(), '')
            self.assertEqual(error.getvalue(), 'keyless registry authentication failed\n')

    def test_unknown_pool_rejected(self):
        """A syntactically valid but unknown pool must be refused."""
        cfg = copy.deepcopy(CONFIG)
        cfg['audience'] = '//iam.googleapis.com/projects/138624361369/locations/global/workloadIdentityPools/oberth-release-unknown/providers/tuxbox'
        with patch.dict(os.environ, {'OBERTH_RELEASE_WIF': '1', 'GOOGLE_APPLICATION_CREDENTIALS': wif.ADC}), \
                patch.object(wif, 'read', return_value=json.dumps(cfg).encode()), \
                patch.object(wif, 'post') as post:
            with self.assertRaises(ValueError):
                wif.credential(wif.REGISTRY)
        post.assert_not_called()

    def test_wrong_project_number_rejected(self):
        """A different project number must be refused."""
        cfg = copy.deepcopy(CONFIG)
        cfg['audience'] = CONFIG['audience'].replace('138624361369', '999999999999')
        with patch.dict(os.environ, {'OBERTH_RELEASE_WIF': '1', 'GOOGLE_APPLICATION_CREDENTIALS': wif.ADC}), \
                patch.object(wif, 'read', return_value=json.dumps(cfg).encode()), \
                patch.object(wif, 'post') as post:
            with self.assertRaises(ValueError):
                wif.credential(wif.REGISTRY)
        post.assert_not_called()

    def test_missing_oberth_release_wif_rejected(self):
        """Without OBERTH_RELEASE_WIF=1, the helper must refuse."""
        for env_val in ('', '0', 'true'):
            with self.subTest(val=env_val):
                env = {'GOOGLE_APPLICATION_CREDENTIALS': wif.ADC}
                if env_val:
                    env['OBERTH_RELEASE_WIF'] = env_val
                with patch.dict(os.environ, env, clear=False), \
                        patch.object(wif, 'post') as post:
                    with self.assertRaises(ValueError):
                        wif.credential(wif.REGISTRY)
                post.assert_not_called()

    def test_unapproved_config_denied_before_network(self):
        for key, value in [('audience', '//iam.googleapis.com/other-pool'),
                           ('token_url', 'https://attacker.invalid'),
                           ('service_account_impersonation_url', _DEFAULT_IAM.replace('publisher', 'reader')),
                           ('credential_source', {'file': '/foreign/token'}),
                           ('extra', 'unexpected')]:
            config = copy.deepcopy(CONFIG); config[key] = value
            with self.subTest(key=key), patch.dict(os.environ, {'OBERTH_RELEASE_WIF': '1', 'GOOGLE_APPLICATION_CREDENTIALS': wif.ADC}), \
                    patch.object(wif, 'read', return_value=json.dumps(config).encode()), patch.object(wif, 'post') as post:
                with self.assertRaises(ValueError): wif.credential(wif.REGISTRY)
            post.assert_not_called()

    def test_exchange_errors_and_malformed_tokens_fail_closed(self):
        for replies in [[ValueError('inert-sensitive-diagnostic')],
                        [{'token_type': 'MAC', 'access_token': 'inert'}],
                        [{'token_type': 'Bearer', 'access_token': 'bad\nheader'}],
                        [{'token_type': 'Bearer', 'access_token': 'inert'}, {}]]:
            with self.subTest(replies=replies), self.assertRaises(ValueError):
                self.exchange(replies=replies)

    def test_redirect_duplicate_and_oversized_responses_refused(self):
        with self.assertRaises(ValueError):
            wif.NoRedirect().redirect_request(None, None, 307, '', {}, 'https://attacker.invalid')
        for raw in [b'{"accessToken":"a","accessToken":"b"}', b' ' * (wif.LIMIT + 1), b'[]']:
            with self.assertRaises(ValueError): wif.decode(raw)

    def test_protocol_error_is_sanitized_and_has_no_stdout(self):
        for args in [['get'], ['list'], ['store']]:
            output, error = io.StringIO(), io.StringIO()
            with patch.object(sys, 'argv', ['helper'] + args), patch.object(sys, 'stdin', io.StringIO(wif.REGISTRY + '\n')), \
                    patch.object(sys, 'stdout', output), patch.object(sys, 'stderr', error), \
                    patch.object(wif.os, 'fstat') as fstat, \
                    patch.object(wif, 'credential', side_effect=ValueError('inert-sensitive-diagnostic')):
                fstat.return_value.st_mode = stat.S_IFIFO | 0o600
                self.assertEqual(wif.main(), 1)
            self.assertEqual(output.getvalue(), '')
            self.assertEqual(error.getvalue(), 'keyless registry authentication failed\n')

    def test_protocol_refuses_regular_file_stdout_before_exchange(self):
        output, error = io.StringIO(), io.StringIO()
        with patch.object(sys, 'argv', ['helper', 'get']), patch.object(sys, 'stdout', output), \
                patch.object(sys, 'stderr', error), patch.object(wif.os, 'fstat') as fstat, \
                patch.object(wif, 'credential') as exchange:
            fstat.return_value.st_mode = stat.S_IFREG | 0o600
            self.assertEqual(wif.main(), 1)
            exchange.assert_not_called()
        self.assertEqual(output.getvalue(), '')

    def test_transport_disables_proxies_and_requires_verified_tls13(self):
        with patch.object(wif.urllib.request, 'build_opener') as build:
            response = build.return_value.open.return_value.__enter__.return_value
            response.status = 200
            response.read.return_value = b'{"fixture":true}'
            self.assertEqual(wif.post(wif.STS, b'fixture', {}), {'fixture': True})
            proxy, redirect, https = build.call_args.args
            self.assertEqual(proxy.proxies, {})
            self.assertIsInstance(redirect, wif.NoRedirect)
            self.assertEqual(https._context.minimum_version, ssl.TLSVersion.TLSv1_3)
            self.assertTrue(https._context.check_hostname)
            self.assertEqual(https._context.verify_mode, ssl.CERT_REQUIRED)
            self.assertEqual(build.return_value.open.call_args.kwargs, {'timeout': 20})
            response.read.assert_called_once_with(wif.LIMIT + 1)


class ReleaseYAMLRenderTests(unittest.TestCase):
    """Verify the release.yaml structure matches the WIF contract."""

    @classmethod
    def setUpClass(cls):
        try:
            import yaml as _yaml
            cls.yaml = _yaml
        except ImportError:
            cls.yaml = None

    def _load(self):
        if self.yaml is None:
            self.skipTest('PyYAML required for render checks')
        path = ROOT / '.oberth' / 'release.yaml'
        with open(path) as f:
            return self.yaml.safe_load(f)

    def _find_template(self, wf, name):
        for t in wf['spec']['templates']:
            if t['name'] == name:
                return t
        self.fail(f'template {name!r} not found')

    def test_release_verify_has_image_reader_wif_role(self):
        wf = self._load()
        t = self._find_template(wf, 'release-verify')
        self.assertEqual(t['metadata']['annotations']['oberth.ci/release-wif-role'], 'image-reader')

    def test_release_verify_secretstore_has_no_gar_reader_key(self):
        wf = self._load()
        t = self._find_template(wf, 'release-verify')
        args = t['container']['args']
        for arg in args:
            self.assertNotIn('gar-reader-key', str(arg))

    def test_workflow_secret_paths_has_no_gar_reader_key(self):
        wf = self._load()
        paths = wf['metadata']['annotations']['oberth.ci/secret-paths']
        self.assertNotIn('gar-reader-key', paths)

    def test_release_publish_images_retains_image_writer_role(self):
        wf = self._load()
        t = self._find_template(wf, 'release-publish-images')
        self.assertEqual(t['metadata']['annotations']['oberth.ci/release-wif-role'], 'image-writer')


if __name__ == '__main__': unittest.main()
