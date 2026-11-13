"""Negative fixtures prove each contract lint rejects new violations."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import subprocess

SPEC = importlib.util.spec_from_file_location("contract_check", Path(__file__).with_name("check.py"))
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class ContractGuards(unittest.TestCase):
    def scan(self, name, source, module=None):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            if module:
                (root / "go.mod").write_text("module " + module + "\n")
            p = root / name
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(source)
            return CHECK.scan(root, [name], {"entries": {}}, [])

    def test_secretmem_owns_integer_erasure(self):
        source = 'package secretmem\nfunc wipe() { n.SetInt64(0) }\n'
        for name in ('secretmem/bigint.go', 'internal/secretmem/bigint.go',
                     'secret/bigint.go', 'hygiene/bigint.go'):
            with self.subTest(name=name):
                self.assertNotIn('rule7-hygiene', [r for r, _, _ in self.scan(name, source)])
        for name in ('other/bigint.go', 'secretmem_copy/bigint.go', 'secretmemory/bigint.go'):
            with self.subTest(name=name):
                self.assertIn('rule7-hygiene', [r for r, _, _ in self.scan(name, source)])

    def test_laws_and_structural_rules(self):
        cases = [
            ("a.go", "// MUST match another value", "law1-confession"),
            ("a.md", "| option | DEFAULT_MODE |", "law2-handtyped-table"),
            ("a.go", 'os.Getenv("NEW_MODE")', "law3-surface"),
            ("a.go", "// temporary workaround", "law4-unregistered"),
            ("a.md", "/ho" + "me/example", "law5-residue"),
            ("handoffs/note.md", "note", "law5-path"),
            ("a_test.go", 'import "go/parser"', "rule6-test-parser"),
            ("a.go", 'import "unsafe"', "rule7-hygiene"),
            ("a.go", 'os.Getenv("APP_TEST_BYPASS")', "rule8-env-seam"),
            ("a_test.go", 'os.Getenv("RUN_LIVE")', "rule10-gate-setter"),
            ("a.go", "//nolint:gosec", "rule11-suppression"),
            (".golangci.yml", "  - G402", "rule11-security-exclusion"),
            ("a.go", "func huge() {\n" + "// x\n" * 201 + "}\n", "rule6-function-length"),
        ]
        for name, source, rule in cases:
            with self.subTest(rule=rule):
                self.assertIn(rule, [r for r, _, _ in self.scan(name, source)])

    def test_markdown_default_tables_follow_headers(self):
        for source in (
                '| Law or rule | Guard |\n| --- | --- |\n| 9. Lifecycle on the default profile | review |',
                '| Scenario | Action |\n| --- | --- |\n| bootstrap | Install default profile |',
                '| Namespace | Binding |\n| --- | --- |\n| default | reader |',
                '| Tool | Purpose |\n| --- | --- |\n| `issue_get` | Full record by default |'):
            with self.subTest(source=source):
                self.assertNotIn('law2-handtyped-table', [r for r, _, _ in self.scan('a.md', source)])
        for source in (
                '| Flag | Default | Meaning |\n| --- | --- | --- |\n| `--limit` | 256 MiB | per run |',
                '| Setting | **Default** |\n| --- | --- |\n| `retention.interval` | `10m` |',
                '| Option | Value |\n| --- | --- |\n| mode | APP_MODE |'):
            with self.subTest(source=source):
                found = [line for r, _, line in self.scan('a.md', source) if r == 'law2-handtyped-table']
                self.assertEqual(found, [source.splitlines()[-1]])

    def test_runtime_temporary_witnesses_do_not_declare_sunsets(self):
        # Six unchanged source witnesses from the consumer qualification stop.
        witnesses = (
            ('consumer-a', '.oberth/release_oci.py', 'temporary = None'),
            ('consumer-b', '.oberth/release_oci.py', 'temporary = None'),
            ('consumer-c', '.oberth/release_oci.py', 'temporary = None'),
            ('consumer-d', '.oberth/test_release_alias.py',
             'with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temporary:'),
            ('consumer-e', 'cmd/source_install_docker_test.go',
             '// temporarily swapping to bytes.Buffer because printDockerDryRun writes'),
            ('consumer-f', '.oberth/release_oci.py', 'temporary = None'),
        )
        for consumer, name, source in witnesses:
            with self.subTest(consumer=consumer):
                self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan(name, source)])
        for source in ('temporary := newBuffer()', 'temporary.Close()',
                       'with tempfile.NamedTemporaryFile() as temporary:',
                       '# Create a temporary file for atomic replacement.',
                       '// Temporarily redirect stdout for this test.',
                       '// API is temporarily unavailable during restart.',
                       '// Temporarily disable the API during this request.',
                       '// Store the response in a temporary buffer.',
                       'func write(temporary API) {}', 'temporary API',
                       '// A temporary path for atomic image replacement.',
                       'Temporary files do not require a sunset record.'):
            with self.subTest(source=source):
                self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])

    def test_real_api_lifetime_statements_still_require_sunsets(self):
        for source in ('// temporary workaround', '// temporary measure',
                       '// The temporary v2-only compatibility path has no authenticated registration.',
                       '// The temporary v2.1-only compatibility path remains available.',
                       '`--controller-introduction-transition=true` is an explicit temporary companion to',
                       '// This is a temporary API.', '// The API is temporary.',
                       '// The v1 endpoint is temporarily supported.',
                       '// Temporarily retain the old API.',
                       '// We will deprecate this endpoint.',
                       '// Sunset the v1 endpoint.', '// remove-by v2.0.0',
                       '// legacy API until v2.0.0',
                       '// temporary = None; deprecated API',
                       '// tempfile.TemporaryDirectory(); sunset the v1 API',
                       '// temporary workaround for this test; temporary API',
                       '// Use a temporary API for this test; deprecated endpoint',
                       'help := \"temporary API\"'):
            with self.subTest(source=source):
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])

    def test_resource_modifiers_and_identifier_conditions_are_not_lifetimes(self):
        for source in ('if temporary is not None:', 'if temporary {',
                       'env = {"HOME": temporary, "PATH": path}',
                       '// temporary TLS listener', '// temporary SA token file',
                       '// temporary banner buffer', '// temporary R2 credentials',
                       '// temporary cosign binary', '// temporary activation file',
                       '// temporary report artifact',
                       'ok "Downloaded and verified temporary cosign v${COSIGN_VERSION}"'):
            with self.subTest(source=source):
                self.assertFalse(CHECK.temporary_lifetime(source, source.startswith('//') or source == 'if temporary {'))
        for source in ('// temporary copy of auth/tokenreview',
                       '// temporary flag file', '// temporary feature buffer',
                       '// temporary override for the cache',
                       '// temporary API with an outage diagnostic'):
            with self.subTest(source=source):
                self.assertTrue(CHECK.temporary_lifetime(source, True))

    def test_temporary_prose_punctuation_is_a_lifetime(self):
        claims = (
            '// TEMPORARY: remove once the gateway ships auth',
            '# Temporary: remove once the gateway ships auth',
            'text = "temporary(remove after migration)"',
            'text = "temporary: remove after migration"',
            '// Temporary: drop after the v2 migration',
            '// temporary (remove after migration)',
            '// temporary: keep accepting unauthenticated registrations',
            '// This is (temporary) for old clients',
            "printf 'Or keep plain HTTP temporarily:\\n'",
            '// Temporary. Remove after migration.',
            '// temporary = remove after migration',
        )
        for name in ('a.go', 'a.py', 'a.md', 'a.yaml', 'a.txt'):
            for claim in claims:
                with self.subTest(name=name, claim=claim):
                    self.assertIn('law4-unregistered', [r for r, _, _ in self.scan(name, claim)])
        for claim in ('temporary := newBuffer()', 'temporary.Close()',
                      'Temporary()', 'temporary: Path = None'):
            with self.subTest(identifier=claim):
                self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan('a.py', claim)])
        for name in ('a.md', 'a.yaml', 'a.txt'):
            self.assertIn('law4-unregistered', [r for r, _, _ in self.scan(name, 'temporary: Remove when auth ships')])
        for name in ('a.sh', 'a.bats'):
            self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan(name, 'temporary=$(mktemp)')])

    def test_temporary_identifiers_are_not_lifetime_claims(self):
        cases = (
            ('a.go', 'temporary, err := os.CreateTemp(dir, "stage-*")'),
            ('a.go', 'if !temporary {'),
            ('a.go', 'env := "TMPDIR="+temporary'),
            ('a.py', 'return temporary'),
            ('a.py', 'return temporary  # staging path'),
            ('a.py', 'result = cleanup(before_remove=f"rm \'{temporary}\'; printf customer > \'{temporary}\'")'),
            ('a.go', 'target := "temporary-target-install-token"'),
            ('a.py', 'target = "temporary-target-install-token"'),
        )
        for name, source in cases:
            with self.subTest(name=name, source=source):
                self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan(name, source)])
        for name, source in (
                ('a.go', 'temporary, err := call() // temporary: remove once auth ships'),
                ('a.py', 'return temporary  # temporary: remove once auth ships'),
                ('a.py', 'message = f"{temporary}: temporary API"'),
                ('a.py', 'message = "temporary-target support for old clients"'),
                ('a.md', 'return temporary'),
                ('a.go', '// return temporary'),
                ('a.go', '// temporary-target support'),
                ('a.go', 'message := "temporary-target: remove once auth ships"')):
            with self.subTest(positive=source):
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan(name, source)])

    def test_temporary_trigger_fails_closed(self):
        for source in (
                '// This temporary compatibility is intended only for a rolling upgrade',
                '// Temporary train-356 controller copy.',
                '// TODO: temporary, remove after migration',
                '// This flag is temporary and will be removed.',
                '// temporary flag for the v1 migration',
                '// The --foo option is temporary.', '// This env var is temporary.',
                '// temporary compatibility setting', '// temporary hack',
                '// temporary fix; remove once the gateway ships auth',
                '// temporary override', '// Temporarily support the old flag.',
                '// Temporarily accept unauthenticated registrations.',
                '// temporary field kept for old clients',
                '// The old field is temporarily retained.',
                '// temporary copy of auth/tokenreview',
                '// temporary bridge for v2 clients', '// temporary support for v2 agents',
                '// temporary object API', '// temporary credential override',
                '// temporary file; temporary flag', '// temporary file. Temporary hack',
                '// temporary file, temporary feature', '// temporary mysterious mechanism'):
            with self.subTest(source=source):
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])
        for source in ('temporary = None', 'temporary.Close()', 'call(temporary)',
                       'call(first, temporary, last)', 'Temporary()',
                       'obj.Temporary()', 'with resource() as temporary:',
                       '// Create temporary objects.', '// Use a temporary directory.',
                       '// Store temporary credentials.', '// temporary output',
                       '// temporary key', '// temporary token', '// temporary listener',
                       '// temporary storage', '// temporary snapshot', '// temporary root',
                       '// temporary Pods', '// temporary unit', '// temporary binary',
                       '// temporary download', '// temporary cache', '// temporary database'):
            with self.subTest(source=source):
                self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])

    def test_operation_context_does_not_waive_lifetime_claims(self):
        for source in ('// temporary compatibility mode for each request from v2 agents',
                       '// The temporary API is kept for the request path from v1 clients.',
                       '// Use a temporary API for this test.',
                       '// temporary flag for the test',
                       '// temporary override during an operation'):
            with self.subTest(source=source):
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])
        for source in ('// Temporarily redirect stdout for this test.',
                       '// Temporarily disable the API during this request.',
                       '// Use a temporary buffer for each request.'):
            with self.subTest(source=source):
                self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])

    def test_temporary_runtime_failures_are_not_sunsets(self):
        for source in ('// temporary delete outage', '// temporary DNS lookup error',
                       '// temporary API outage', 'err := errors.New("temporary API outage")',
                       '// temporary network failure', '// temporary authentication error',
                       '// temporary outage', '// temporary error',
                       '// API temporarily unavailable'):
            with self.subTest(source=source):
                self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])
        for source in ('// temporary API outage; temporary flag',
                       '// temporary error, temporary hack',
                       '// temporary API outage. Temporary API',
                       '// temporary flag reports an error',
                       '// temporary API outage; deprecated endpoint'):
            with self.subTest(source=source):
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])

    def test_until_retained_copy_policy_and_qualification_lifetimes(self):
        claims = (
            ('a.md', '`v0.13.x`. Until the minimum supported controller is >= v0.14.0, the agent keeps'),
            ('a.yaml', '# pattern locally until a fragment release provides this tool.'),
            ('a.tpl', 'existing installs continue working until the helm-update pipeline populates'),
            ('a.go', '// proxyReadersSupported stays false until gateway qualification lifts the gate in'),
            ('a.go', '// upgrade keeps its old, broad policy in the store until its controller re-inits'),
        )
        for name, source in claims:
            with self.subTest(source=source):
                self.assertTrue(CHECK.UNTIL.search(source))
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan(name, source)])
        for source in (
                '// buffer locally until the write completes',
                '// existing connections continue working until the request ends',
                '// keeps its old buffer until the copy completes',
                '// readiness stays false until the probe succeeds',
                '// wait until the minimum number of workers is ready',
                'if until is not None:', 'until := deadline'):
            with self.subTest(runtime=source):
                self.assertFalse(CHECK.sunset_statement(source, True))

    def test_until_requires_a_feature_lifetime(self):
        for source in ('// Serve accepts connections until ctx is cancelled',
                       '// retries requests until it is revoked',
                       '// Accept probe connections until test cleanup',
                       'if until is not None:', 'if now != until {',
                       '// Read until EOF.', '// copy until the stream closes',
                       '// Keep the connection until the request finishes',
                       '// Supported requests run until cancellation'):
            with self.subTest(source=source):
                self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])
        for source in ('// supported until the replacement ships',
                       '// removed until v2.0.0', '// legacy until 2027-01-01',
                       '// API stays local until wire ships the replacement',
                       '// The alias is retained until clients migrate.',
                       '// The feature remains available until upstream lands.',
                       '// The API is supported until December.',
                       '// accepted until the 1.1 chart',
                       '// honoured until the 1.1 chart'):
            with self.subTest(source=source):
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])

    def test_only_wire_owned_generated_outputs_are_excluded(self):
        go_header = '// Generated from wire registry/surfaces.json; edit the registry and rerun scripts/surface_registry.py.\n'
        py_header = '#!/usr/bin/env python3\n# Generated from wire registry; regenerate with scripts/surface_registry.py.\n'
        debt = '// temporary workaround MUST match another value\n'
        for name, header in (('internal/webhook/surface_registry_generated.go', go_header),
                             ('internal/annotations/surface_constants_generated.go', go_header),
                             ('.oberth/check-surface-registry.py', py_header),
                             ('scripts/check-surface-registry.py', py_header)):
            with self.subTest(name=name):
                self.assertEqual(self.scan(name, header + debt), [])
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan(name, debt)])
        self.assertIn('law4-unregistered', [r for r, _, _ in self.scan('other_generated.go', go_header + debt)])
        self.assertIn('law4-unregistered', [r for r, _, _ in self.scan('internal/webhook/handwritten.go', debt)])

    def test_wire_generated_inventory_artifacts_are_excluded(self):
        for name, header in (
                ('tools/surface_registry_generated.py', '# Generated from wire registry/surfaces.json.\n'),
                ('docs/reference/surface-registry.md', '# Surface registry inventory\n\nGenerated from wire `registry/surfaces.json`.')):
            with self.subTest(name=name):
                self.assertEqual(self.scan(name, header + '\n// temporary workaround'), [])
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan(name, '// temporary workaround')])

    def test_generator_and_specific_explanation(self):
        self.assertEqual(self.scan("a.go", "// MUST match generator tools/generate.go output\n//nolint:gosec // bounded public input"), [])

    def test_generator_exception_requires_a_named_generator(self):
        self.assertIn('law1-confession', [rule for rule, _, _ in self.scan('a.go', '// MUST match generator output')])

    def test_baseline_does_not_allow_different_violation(self):
        old = CHECK.fingerprint([("law1", "a.go", "MUST match A")])
        new = CHECK.fingerprint([("law1", "a.go", "MUST match B")])
        self.assertTrue(new - old)

    def test_source_archive_without_git_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'a.go').write_text('package main')
            self.assertEqual(CHECK.files(root), ['a.go'])

    def test_git_ownership_is_scoped_to_source(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            (root / '.git').write_text('gitdir: unused')
            with patch.object(CHECK.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'a.go\0')) as run:
                self.assertEqual(CHECK.files(root), ['a.go'])
            command = run.call_args.args[0]
            self.assertEqual(command[:3], ['git', '-c', 'safe.directory=' + str(root)])

    def test_pane_residue_does_not_match_modulo(self):
        for source in ('n%10 == 0', 'n %10 == 0', 'n % 10 == 0', 'fmt.Printf("%10d", n)', 'width: 50%20'):
            with self.subTest(source=source):
                self.assertEqual(self.scan('a.go', source), [])
        for source in ('pa' + 'ne ' + '%' + '12', 'target=' + '%' + '12', '"' + '%' + '12' + '"', '%' + '12'):
            with self.subTest(source=source):
                self.assertIn('law5-residue', [r for r, _, _ in self.scan('a.md', source)])

    def test_test_annotation_literals_are_not_product_surfaces(self):
        source = 'const archivedAnnotation = "' + 'oberth.ci/' + 'retired-fixture"'
        self.assertNotIn('law3-surface', [r for r, _, _ in self.scan('a_test.go', source)])
        self.assertIn('law3-surface', [r for r, _, _ in self.scan('a.go', source)])

    def test_build_annotation_requires_registration(self):
        self.assertIn('law3-surface', [r for r, _, _ in self.scan('a.go', '"oberth.ci/new-mode"')])

    def test_registered_label_has_the_same_surface_guard(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "deprecations.yaml").write_text('{"deprecations":[]}')
            source = root / "a.go"
            source.write_text('package a\nvar labels = map[string]string{"oberth.ci/probe": "owned"}\n')
            registry = {"entries": {"label:oberth.ci/probe": {"doc_anchor": "probe.md", "test_ref": "probe test"}}}
            (root / "surface-registry.json").write_text(json.dumps(registry))
            CHECK.validate_registries(root)
            self.assertNotIn("law3-surface", [r for r, _, _ in CHECK.scan(root, ["a.go"], registry, [])])
            self.assertIn("law3-surface", [r for r, _, _ in CHECK.scan(root, ["a.go"], {"entries": {}}, [])])
            del registry["entries"]["label:oberth.ci/probe"]["test_ref"]
            (root / "surface-registry.json").write_text(json.dumps(registry))
            with self.assertRaisesRegex(ValueError, "test_ref"):
                CHECK.validate_registries(root)

    def test_generic_registry_test_reference(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "deprecations.yaml").write_text('{"deprecations":[]}')
            (root / "surface-registry.json").write_text('{"entries":{"annotation:oberth.ci/build-mode":{"doc_anchor":"build.md#mode","test_ref":"build admission"}}}')
            CHECK.validate_registries(root)
            (root / "surface-registry.json").write_text('{"entries":{"annotation:oberth.ci/build-mode":{"doc_anchor":"build.md#mode"}}}')
            with self.assertRaisesRegex(ValueError, "test_ref"):
                CHECK.validate_registries(root)

    def test_dated_and_undated_compatibility_claims(self):
        for source in ('compat window kept until 2026-12-01',
                       'until 2026-12-01T12:00:00Z',
                       'temporary until upstream ships the fix',
                       'honoured until the 1.1 chart',
                       'compatibility shim', 'shim remains',
                       'backward compatibility remains'):
            with self.subTest(source=source):
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan('a.md', source)])

    def test_only_canonical_suppression_syntax(self):
        for source in ('//nolint:gosec -- reason', '//nolint:all // reason',
                       '//nolint:gosec, // reason', '//nolint:gosec //'):
            with self.subTest(source=source):
                self.assertIn('rule11-suppression', [r for r, _, _ in self.scan('a.go', source)])
        self.assertNotIn('rule11-suppression', [r for r, _, _ in self.scan('a.go', '//nolint:gosec,errcheck // reason')])

    def test_timestamp_sunsets_and_removed_records(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "surface-registry.json").write_text('{"entries":{}}')
            record = {"id":"old", "since":"2020-01-01", "until":"2099-01-02T00:00:00Z", "owner":"maintainer", "replacement":"remove"}
            def write():
                (root / "deprecations.yaml").write_text(json.dumps({"deprecations":[record]}))
            write()
            CHECK.validate_registries(root)
            record["until"] = "2020-01-02T00:00:00Z"
            write()
            with self.assertRaisesRegex(ValueError, "expired"):
                CHECK.validate_registries(root)
            record.update(status="removed", removed="2020-01-01", paths=[{"path":"a.go", "contains":"retired()"}])
            write()
            CHECK.validate_registries(root)
            (root / "a.go").write_text("retired()")
            with self.assertRaisesRegex(ValueError, "removed deprecation remains"):
                CHECK.validate_registries(root)
            (root / "a.go").write_text("current()")
            record["removal_guard"] = {"path":"a.go", "contains":"refuse retired"}
            (root / "a.go").write_text("refuse retired")
            write()
            CHECK.validate_registries(root)
            (root / "a.go").write_text("accept retired")
            with self.assertRaisesRegex(ValueError, "removal guard missing"):
                CHECK.validate_registries(root)
            record["until"] = "invalid"
            write()
            with self.assertRaises(ValueError):
                CHECK.validate_registries(root)

    def test_expired_sunset_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "surface-registry.json").write_text('{"entries":{}}')
            (root / "deprecations.yaml").write_text('{"deprecations":[{"id":"old","since":"2020-01-01","until":"2020-01-02","owner":"maintainer","replacement":"remove"}]}')
            with self.assertRaisesRegex(ValueError, "expired"):
                CHECK.validate_registries(root)

    def test_review_compatibility_and_removal_claims(self):
        for source in ('kept for compatibility with older CLIs',
                       'For compatibility with releases that do not inherit the operator store',
                       'Retained for compatibility with 0.18 callers',
                       'Temporarily accept the bare HMAC form',
                       'for back-compat', 'back-compatible with pre-v0.1.64 operators',
                       'backcompat shim', 'compatibility alias for --store-addr',
                       'retained for backward API compatibility',
                       'remove after 2026-12-01', 'TODO: remove in v0.20',
                       'remove by 2027-01-31', 'removed at v2.0',
                       'legacyCaps remains enabled through 2026-11-30',
                       'accepted at the floor until the next floor raise'):
            with self.subTest(source=source):
                self.assertIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])
        for source in ('service temporarily unavailable', 'temporarily unable to connect',
                       '// Keep the floor until its latest timestamp expires.'):
            self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])

    def test_absent_alias_and_timestamp_floors_are_not_sunsets(self):
        for source in ('There is no compatibility alias: absence is enforced',
                       'no compatibility alias. This deliberate break is required',
                       'tests prove the unsafe compatibility alias remains absent',
                       'RequestID: "backcompat-k8s-1",',
                       'v2 is used for compatibility checks',
                       'nil AAD chunks for compatibility tests',
                       'grants fail closed for compatibility; protection stays enforced',
                       'Keep each floor until even its latest fenced timestamp is outside the window',
                       'retain the older floor until its maximum timestamp expires'):
            with self.subTest(source=source):
                self.assertNotIn('law4-unregistered', [r for r, _, _ in self.scan('a.go', source)])

    def test_staticcheck_ignore_is_specific_explained_and_line_scoped(self):
        for source in ('//lint:ignore SA1019 required API probe', '//lint:ignore SA4006 explicit test control'):
            with self.subTest(source=source):
                self.assertNotIn('rule11-suppression', [r for r, _, _ in self.scan('a.go', source)])
        for source in ('//lint:ignore', '//lint:ignore SA1019', '//lint:ignore SA1019   ',
                       '//lint:ignore all reason', '//lint:ignore S1000 reason',
                       '//lint:ignore SA1019 SA4006 reason', '//lint:ignore SA1019,SA4006 reason',
                       '//lint:file-ignore SA1019 reason', '// lint:ignore SA1019 reason',
                       '//lint:ignore SA1019 // reason',
                       '//lint:ignore SA1019 reason //nolint:all // hidden',
                       '//lint:ignore SA1019 reason //lint:file-ignore SA4006 hidden'):
            with self.subTest(source=source):
                self.assertIn('rule11-suppression', [r for r, _, _ in self.scan('a.go', source)])

    def test_alternative_suppressions_are_rejected(self):
        for source in ('// #nosec G204 -- trusted executable', '#nosec G304',
                       '// #nosec G204 // trusted executable',
                       '//lint:file-ignore SA1019 migration',
                       '//gosec:disable G204 -- reason',
                       '//nolint:gosec // reason //nolint:all // hidden reason',
                       '//lint:ignore SA1019', '// lint:ignore SA1019 // migration',
                       '//nolint:gosec // reason; #nosec G204'):
            with self.subTest(source=source):
                self.assertIn('rule11-suppression', [r for r, _, _ in self.scan('a.go', source)])

    def test_registered_sunset_is_bound_to_source_path(self):
        record = dict(id='old-mode', since='2026-01-01', until='2027-01-31',
                      owner='maintainer', replacement='new mode', paths=['allowed.go'])
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ('allowed.go', 'other.go'):
                (root / name).write_text('// temporary path [old-mode]')
                rules = [r for r, _, _ in CHECK.scan(root, [name], {'entries': {}}, [record])]
                self.assertEqual('law4-unregistered' in rules, name == 'other.go')

    def test_sunset_warning_window(self):
        from datetime import date
        from contextlib import redirect_stderr
        from io import StringIO
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'surface-registry.json').write_text('{"entries":{}}')
            record = dict(id='old-mode', since='2026-01-01', until='2027-01-31',
                          owner='maintainer', replacement='new mode')
            import json
            (root / 'deprecations.yaml').write_text(json.dumps({'deprecations':[record]}))
            for today, warning in ((date(2026,12,31), False), (date(2027,1,1), True),
                                   (date(2027,1,30), True)):
                output = StringIO()
                with patch.object(CHECK, 'date', wraps=date) as clock, redirect_stderr(output):
                    clock.today.return_value = today
                    CHECK.validate_registries(root)
                self.assertEqual('WARNING' in output.getvalue(), warning)
                if warning:
                    self.assertIn('old-mode', output.getvalue())
                    self.assertIn('maintainer', output.getvalue())
                    self.assertIn('2027-01-31', output.getvalue())
            with patch.object(CHECK, 'date', wraps=date) as clock:
                clock.today.return_value = date(2027,1,31)
                with self.assertRaisesRegex(ValueError, 'expired'):
                    CHECK.validate_registries(root)



if __name__ == "__main__":
    unittest.main()
