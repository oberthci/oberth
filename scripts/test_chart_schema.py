#!/usr/bin/env python3
"""Exercise the active Argo values through Helm's actual schema validator."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
MAX_NAME = '.'.join(['a' * 63] * 3 + ['a' * 61])
SAMPLER = {'enabled': True, 'interval': '15s',
           'nodes': {'node-a': 'https://node-a.example.com:10250'},
           'caCert': 'public-ca-fixture'}


class ArgoSchemaTests(unittest.TestCase):
    def test_pod_policy_switch_is_boolean(self):
        for enabled in (True, False):
            result = self.render({'podPolicy': {'enabled': enabled}})
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual('kind: ValidatingAdmissionPolicy\n' in result.stdout, enabled)
        for enabled in ('false', 0, None):
            self.assert_schema_refuses({'podPolicy': {'enabled': enabled}})

    def test_pinned_source_policy_is_explicit_and_typed(self):
        baseline = self.render({})
        self.assertEqual(baseline.returncode, 0, baseline.stderr)
        self.assertIn('--source-allowlist={}', baseline.stdout)
        with tempfile.TemporaryDirectory() as directory:
            values = Path(directory) / 'sources.json'
            policy = {'forge/acme/consumer': ['forge/acme/producer']}
            values.write_text(json.dumps({'sourceAccess': policy}))
            result = self.render({}, '-f', str(values))
            self.assertEqual(result.returncode, 0, result.stderr)
            # Helm quotes the complete JSON argument, with no implicit pairs.
            self.assertIn(json.dumps('--source-allowlist=' + json.dumps(policy, separators=(',', ':'))), result.stdout)
            for invalid in ([], {'consumer': ['forge/acme/producer']},
                            {'forge/acme/consumer': 'forge/acme/producer'},
                            {'forge/acme/consumer': ['https://example.com/repo']},
                            {'forge/acme/consumer': ['forge/acme/producer'] * 65}):
                with self.subTest(invalid=invalid):
                    values.write_text(json.dumps({'sourceAccess': invalid}))
                    self.assert_schema_refuses({}, '-f', str(values))

    def test_retired_secret_settings_are_optional_and_inert(self):
        baseline = self.render({})
        self.assertEqual(baseline.returncode, 0, baseline.stderr)
        for settings in (
            ['--set', 'secretCreatePolicy=null'],
            ['--set', 'upstream.createSecrets=null', '--set', 'upstream.keySecret=null',
             '--set', 'upstream.knownHostsSecret=null', '--set', 'auditAnchor.rootsKey=null'],
            ['--set', 'auditAnchor.rootsKey=old-roots.pem'],
            ['--set', 'secretCreatePolicy.enabled=true'],
            ['--set', 'upstream.keySecret=legacy', '--set', 'upstream.knownHostsSecret=legacy'],
        ):
            with self.subTest(settings=settings):
                result = self.render({}, *settings)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, baseline.stdout)

    def test_secret_creation_stays_disabled(self):
        self.assert_schema_refuses({}, '--set', 'upstream.createSecrets=true')

    def render(self, argo, *overrides):
        with tempfile.TemporaryDirectory() as directory:
            values = Path(directory) / 'values.json'
            values.write_text(json.dumps({'argo': argo}))
            return subprocess.run([
                'helm', 'template', 'oberth', str(ROOT / 'charts/oberth'),
                '--set', 'image.ref=example.invalid/oberth@sha256:' + '1' * 64,
                '-f', str(values), *overrides,
            ], capture_output=True, text=True)

    def assert_schema_refuses(self, argo, *overrides):
        result = self.render(argo, *overrides)
        self.assertNotEqual(result.returncode, 0, f'Helm accepted invalid values: {argo}')
        self.assertIn('schema', result.stderr.lower(), result.stderr)

    def test_string_false_cannot_enable_sampler(self):
        self.assert_schema_refuses({'resourceSampler': SAMPLER},
                                   '--set-string', 'argo.resourceSampler.enabled=false')
        result = self.render({'resourceSampler': dict(SAMPLER, enabled=False)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('name: oberth-resource-sampler', result.stdout)

    def test_active_value_types_and_unknown_keys(self):
        cases = [
            {'enabled': 'false'},
            {'vault': {'adress': 'https://bao.example.com'}},
            {'goProxy': {'enabeld': True}},
            {'ciSecrets': 17},
            {'ciSecrets': {'serviceAccount': 17}},
            {'ciSecrets': {'vaultRole': []}},
            {'ciSecrets': {'servceAccount': 'typo'}},
            {'controllerProfile': False},
            {'controllerProfile': 'unknown'},
            {'perRepoIdentities': 'repo'},
            {'perRepoCIIdentities': [7]},
            {'perStepIdentities': [{}]},
            {'resourceSampler': {'enabled': 'false'}},
            {'resourceSampler': {'interval': 15}},
            {'resourceSampler': {'interval': 'not-a-duration'}},
            {'resourceSampler': {'nodes': []}},
            {'resourceSampler': {'nodes': {'node-a': 10250}}},
            {'resourceSampler': {'nodes': {'node-a': 'http://node-a:10250'}}},
            {'resourceSampler': {'caCert': []}},
            {'resourceSampler': {'enabeld': True}},
            {'piplineServiceAccount': 'typo'},
        ]
        for argo in cases:
            with self.subTest(argo=argo):
                self.assert_schema_refuses(argo)

    def test_identity_and_sampler_bounds(self):
        cases = [
            {'ciSecrets': {'serviceAccount': 'bad/name'}},
            {'ciSecrets': {'vaultRole': 'bad/name'}},
            {'resourceSampler': {'nodes': {'node-' + str(n): 'https://node.example.com:10250'
                                           for n in range(33)}}},
            {'resourceSampler': {'nodes': {MAX_NAME + 'a': 'https://node.example.com:10250'}}},
            {'resourceSampler': {'enabled': True, 'nodes': {}, 'caCert': 'public-ca-fixture'}},
            {'resourceSampler': dict(SAMPLER, caCert='')},
        ]
        for key in ('perRepoIdentities', 'perRepoCIIdentities', 'perStepIdentities'):
            cases.extend([{key: ['bad/name']}, {key: [MAX_NAME + 'a']},
                          {key: ['duplicate', 'duplicate']}])
        for argo in cases:
            with self.subTest(argo=argo):
                self.assert_schema_refuses(argo)

    def test_valid_boundaries_and_legacy_values_render(self):
        cases = [{}, {'enabled': True}, {'enabled': False}, {'ciSecrets': None}, {'ciServiceAccount': 'legacy-pipeline'},
                 {'ciSecretsServiceAccount': ''}, {'vault': {'ciSecretsRole': ''}},
                 {'ciSecrets': {'serviceAccount': 'ci-secrets', 'vaultRole': 'ci-role'},
                  'controllerProfile': 'nonroot-static-v1', 'resourceSampler': SAMPLER},
                 {'resourceSampler': dict(SAMPLER, nodes={
                     'node-' + str(n): 'https://node.example.com:10250' for n in range(32)})},
                 {'resourceSampler': dict(SAMPLER, interval='')},
                 {'resourceSampler': dict(SAMPLER, interval='1s')},
                 {'resourceSampler': dict(SAMPLER, interval='30s')},
                 {'resourceSampler': dict(SAMPLER, interval='1.5s')},
                 {'resourceSampler': dict(SAMPLER, interval='1s500ms')}]
        for key in ('perRepoIdentities', 'perRepoCIIdentities', 'perStepIdentities'):
            cases.append({key: [MAX_NAME]})
        for argo in cases:
            with self.subTest(argo=argo):
                result = self.render(argo)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_empty_ci_secret_names_preserve_defaults_for_one_release(self):
        default = self.render({})
        self.assertEqual(default.returncode, 0, default.stderr)
        for ci_secrets in ({'serviceAccount': ''}, {'vaultRole': ''},
                           {'serviceAccount': '', 'vaultRole': ''}):
            with self.subTest(ciSecrets=ci_secrets):
                result = self.render({'ciSecrets': ci_secrets})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, default.stdout)

    def test_retired_vm_keys_accept_and_ignore_saved_values(self):
        schema = json.loads((ROOT / 'charts/oberth/values.schema.json').read_text())
        argo = schema['properties']['argo']
        self.assertIs(argo['additionalProperties'], False)
        saved = {'kubevirtCeiling': {'maxVMs': 2, 'maxCores': 6, 'maxMemory': '12Gi'},
                 'vmLauncherServiceAccount': 'legacy-vm-launcher',
                 'containerDiskPrefixes': ['registry.example.com/disks/']}
        result = self.render(saved)
        self.assertEqual(result.returncode, 0, result.stderr)
        baseline = self.render({})
        self.assertEqual(baseline.returncode, 0, baseline.stderr)
        self.assertEqual(result.stdout, baseline.stdout, 'retired values must not change resources')
        for key in saved:
            with self.subTest(key=key):
                description = argo['properties'][key]['description'].lower()
                self.assertIn('removed', description)
                self.assertIn('accept and ignore historical values', description)

    def test_retired_aliases_still_report_migration(self):
        for argo in ({'ciSecretsServiceAccount': 'retired-account'},
                     {'vault': {'ciSecretsRole': 'retired-role'}}):
            with self.subTest(argo=argo):
                result = self.render(argo)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('retired', result.stderr.lower(), result.stderr)


if __name__ == '__main__':
    unittest.main()
