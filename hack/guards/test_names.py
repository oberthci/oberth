"""Check private literal patterns across tracked paths, bytes and reachable metadata."""
import os
import json
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest.mock import patch

import test_run


class NameTests(unittest.TestCase):
    setUp = test_run.RepositoryFixture.setUp
    git = test_run.RepositoryFixture.git
    write = test_run.RepositoryFixture.write

    def policy(self, term="fixture-screen-name"):
        file = tempfile.NamedTemporaryFile(mode="w", dir=self.root.parent)
        self.addCleanup(file.close)
        file.write(term+"\n")
        file.flush()
        environment = patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE": file.name})
        environment.start()
        self.addCleanup(environment.stop)

    def test_case_insensitive_substrings_in_paths_and_binary_contents(self):
        self.policy()
        file = Path("prefix-Fixture-Screen-Name-suffix/data.bin")
        file.parent.mkdir()
        file.write_bytes(b"before\x00FIXTURE-SCREEN-NAME\x00after")
        self.git("add", str(file))
        rows = test_run.GUARDS.names()
        self.assertEqual(2, len(rows), rows)
        self.git("commit", "-qm", "Add opaque fixture")
        self.assertEqual(2, len(test_run.GUARDS.names("HEAD")))
        file.unlink()
        self.assertEqual(2, len(test_run.GUARDS.names("HEAD")))
        self.assertEqual(1, len(test_run.GUARDS.names()))

    def test_symlink_target_is_checked_without_reading_outside_the_tree(self):
        self.policy()
        Path("pointer").symlink_to("../fixture-screen-name/missing")
        self.git("add", "pointer")
        self.assertEqual(1, len(test_run.GUARDS.names()))
        self.git("commit", "-qm", "Add link fixture")
        self.assertEqual(1, len(test_run.GUARDS.names("HEAD")))

    def test_paths_with_colons_newlines_and_spaces_are_not_split(self):
        self.policy()
        file = "prefix:4\n fixture-screen-name.txt"
        self.write(file, "safe")
        rows = test_run.GUARDS.names()
        self.assertEqual(1, len(rows), rows)
        self.assertTrue(rows[0].startswith("[masked]:"), rows)
        self.assertNotIn("fixture-screen-name", rows[0].lower())
        self.assertIsNotNone(test_run.GUARDS.RULE.search(rows[0]))
        self.git("commit", "-qm", "Add path fixture")
        self.assertEqual(1, len(test_run.GUARDS.names("HEAD")))

    def test_every_reachable_message_including_merged_ancestors_is_checked(self):
        self.policy()
        self.git("checkout", "-qb", "topic")
        self.git("commit", "--allow-empty", "-qm", "Mention fixture-screen-name")
        bad = self.git("rev-parse", "HEAD").strip()
        self.git("checkout", "main")
        self.git("commit", "--allow-empty", "-qm", "Clean branch")
        self.assertEqual([], test_run.GUARDS.names())
        self.git("merge", "--no-ff", "-qm", "Merge fixture", "topic")
        rows = test_run.GUARDS.names()
        self.assertEqual(1, len(rows), rows)
        self.assertIn("commit/"+bad, rows[0])
        self.assertEqual(1, len(test_run.GUARDS.names("HEAD")))

    def test_author_and_committer_names_and_different_addresses_are_checked(self):
        self.policy()
        fields = (("GIT_AUTHOR_NAME", "author"), ("GIT_AUTHOR_EMAIL", "author"),
                  ("GIT_COMMITTER_NAME", "committer"), ("GIT_COMMITTER_EMAIL", "committer"))
        for variable, field in fields:
            with self.subTest(field=variable):
                self.git("checkout", "-q", "main")
                self.git("checkout", "-qb", variable.lower())
                with patch.dict(os.environ, {variable: "prefix-fixture-screen-name-suffix"}):
                    self.git("commit", "--allow-empty", "-qm", "Add metadata fixture")
                rows = test_run.GUARDS.names()
                self.assertEqual(1, len(rows), rows)
                self.assertIn("commit/", rows[0])
                self.assertEqual(1, len(test_run.GUARDS.names("HEAD")))

    def test_required_email_exception_is_exact_and_metadata_only(self):
        email = self.git("config", "user.email").strip()
        self.policy(email)
        self.assertEqual([], test_run.GUARDS.names())
        self.write("data.txt", email)
        self.assertEqual(1, len(test_run.GUARDS.names()))
        self.git("commit", "-qm", "Mention "+email)
        self.assertEqual(2, len(test_run.GUARDS.names()))
        with patch.dict(os.environ, {"GIT_AUTHOR_EMAIL": "prefix-"+email}):
            self.git("commit", "--allow-empty", "-qm", "Different metadata address")
        self.assertEqual(3, len(test_run.GUARDS.names()))

    def test_annotated_tag_messages_taggers_and_nested_tags_are_scanned(self):
        self.policy()
        self.git("tag", "-a", "inner", "-m", "fixture-screen-name")
        self.git("tag", "-a", "outer", "inner", "-m", "Safe outer message")
        self.git("tag", "-d", "inner")
        self.assertEqual(1, len(test_run.GUARDS.names()))
        with patch.dict(os.environ, {"GIT_COMMITTER_NAME": "fixture-screen-name"}):
            self.git("tag", "-a", "tagger-case", "-m", "Safe message")
        rows = test_run.GUARDS.names()
        self.assertEqual(2, len(rows), rows)
        self.assertTrue(all(row.startswith("tag/") for row in rows))

    def test_lightweight_tag_reaches_commits_outside_the_selected_history(self):
        self.policy()
        self.git("checkout", "-qb", "topic")
        self.git("commit", "--allow-empty", "-qm", "fixture-screen-name")
        sha = self.git("rev-parse", "HEAD").strip()
        self.git("tag", "saved", sha)
        self.git("checkout", "main")
        self.git("branch", "-D", "topic")
        rows = test_run.GUARDS.names()
        self.assertEqual(1, len(rows), rows)
        self.assertIn("commit/"+sha, rows[0])

    def test_gitlink_path_masks_the_whole_matching_segment(self):
        self.policy()
        sha = self.git("rev-parse", "HEAD").strip()
        self.git("update-index", "--add", "--cacheinfo", "160000,"+sha+",prefix-fixture-screen-name-suffix")
        self.git("commit", "-qm", "Add submodule fixture")
        for ref in ("", "HEAD"):
            rows = test_run.GUARDS.names(ref)
            self.assertEqual(1, len(rows), rows)
            self.assertTrue(rows[0].startswith("[masked]:"), rows)
            self.assertNotIn("fixture-screen-name", rows[0])

    def test_extra_headers_and_tag_paths_are_masked(self):
        self.policy()
        raw = self.git("cat-file", "commit", "HEAD").replace("\n\n", "\nextra fixture-screen-name\n\n", 1)
        sha = subprocess.check_output(["git", "hash-object", "-w", "-t", "commit", "--stdin"], input=raw.encode()).decode().strip()
        self.git("update-ref", "refs/heads/main", sha)
        self.git("tag", "fixture-screen-name-tag")
        rows = test_run.GUARDS.names()
        self.assertEqual(2, len(rows), rows)
        self.assertTrue(any(row.startswith("refs/tags/[masked]") for row in rows))
        self.assertTrue(all("fixture-screen-name" not in row.lower() for row in rows))

    def test_email_case_variants_are_not_exempt(self):
        email = self.git("config", "user.email").strip()
        self.policy(email)
        for index, variant in enumerate((email.upper(), email.split("@")[0]+"@"+email.split("@")[1].upper())):
            self.git("checkout", "-q", "main")
            self.git("checkout", "-qb", "case-"+str(index))
            with patch.dict(os.environ, {"GIT_AUTHOR_EMAIL": variant}):
                self.git("commit", "--allow-empty", "-qm", "Changed case")
            self.assertEqual(1, len(test_run.GUARDS.names()))

    def test_literal_guard_does_not_decode_or_join_lines(self):
        self.policy()
        self.write("split.txt", "fixture-screen-\nname")
        Path("encoded.txt").write_bytes("fixture-screen-name".encode("utf-16"))
        self.git("add", "encoded.txt")
        self.assertEqual([], test_run.GUARDS.names())

    def test_subdirectory_invocation_covers_the_whole_tree_and_policy_boundary(self):
        self.policy()
        self.write("outside.txt", "fixture-screen-name")
        Path("nested").mkdir()
        os.chdir("nested")
        self.assertEqual(1, len(test_run.GUARDS.names()))
        internal = self.root/"internal-policy.txt"
        internal.write_text("fixture-screen-name\n")
        with patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE": str(internal)}):
            self.assertEqual([test_run.GUARDS.finding("NAME-POLICY", detail="Names file must resolve outside the repository.")],
                             test_run.GUARDS.names())

    def test_internal_names_file_has_a_fixed_policy_finding(self):
        term = "fixture-confidential-selector"
        internal = self.root/(term+".txt")
        internal.write_text(term+"\n")
        self.write("hack/plans/policy.py", "")
        with tempfile.TemporaryDirectory(dir=self.root.parent) as directory:
            link = Path(directory)/"policy"
            link.symlink_to(internal)
            for policy in (internal, link):
                with self.subTest(symlink=policy == link):
                    env = {**os.environ, "OBERTH_GUARDS_NAMES_FILE": str(policy)}
                    result = subprocess.run(["python3", str(test_run.GUARDS.ROOT/"hack/guards/run.py"), "names", "HEAD"],
                                            env=env, capture_output=True, text=True, timeout=30)
                    self.assertEqual(1, result.returncode)
                    self.assertEqual(test_run.GUARDS.finding("NAME-POLICY", detail="Names file must resolve outside the repository.")+"\n", result.stdout)
                    self.assertEqual("", result.stderr)
                    self.assertNotIn(term, result.stdout+result.stderr)
                    self.assertNotIn(str(policy), result.stdout+result.stderr)

    def test_invalid_ref_fails_instead_of_passing_an_empty_scan(self):
        self.policy()
        with self.assertRaises(ValueError):
            test_run.GUARDS.names("missing-reference")

    def test_name_formatter_emits_only_the_rule_and_location(self):
        term = "fixture-confidential-selector"
        row = test_run.GUARDS.finding("NAME", "source.txt", 7, term)
        self.assertEqual("source.txt:7: [NAME]", row)
        self.assertNotIn(term, row)
        self.assertIsNotNone(test_run.GUARDS.RULE.search(row))

    def test_real_names_entrypoint_never_prints_matches_under_shell_tracing(self):
        term = "fixture-confidential-selector"
        self.policy(term)
        self.write("prefix-"+term+"/source.txt", term+"\n")
        self.write("hack/plans/policy.py", "")
        self.git("commit", "-qm", "Message mentions "+term)
        sha = self.git("rev-parse", "HEAD").strip()
        result = subprocess.run(["bash", "-x", "-c", 'source "$1/hack/steps.sh"; run python3 "$1/hack/guards/run.py" names HEAD', "guard-trace",
                                 str(test_run.GUARDS.ROOT)],
                                capture_output=True, text=True, timeout=30)
        self.assertEqual(1, result.returncode)
        self.assertNotIn(term, result.stdout+result.stderr)
        self.assertIn("[masked]/source.txt:1: [NAME]", result.stdout)
        self.assertIn("commit/"+sha, result.stdout)
        self.assertTrue(all(row.endswith("[NAME]") for row in result.stdout.splitlines() if test_run.GUARDS.RULE.search(row)))

    def test_names_command_errors_never_echo_private_stderr(self):
        term = "fixture-confidential-selector"
        self.policy(term)
        self.write("hack/plans/policy.py", "")
        self.git("commit", "-qm", "Establish error fixture")
        binary = Path("bin/git")
        binary.parent.mkdir()
        real_git = subprocess.check_output(["which", "git"], text=True).strip()
        env = {**os.environ, "PATH": str(self.root/"bin")+os.pathsep+os.environ["PATH"]}
        for stage in ("rev-list", "for-each-ref", "cat-file"):
            with self.subTest(stage=stage):
                binary.write_text('#!/bin/sh\nif [ "$1" = '+stage+' ]; then\n  printf "%s\\n" '+term+'\n  printf "%s\\n" '+term+' >&2\n  exit 2\nfi\nexec '+real_git+' "$@"\n')
                binary.chmod(0o700)
                result = subprocess.run(["bash", "-x", "-c", 'source "$1/hack/steps.sh"; run python3 "$1/hack/guards/run.py" names HEAD', "guard-trace",
                                         str(test_run.GUARDS.ROOT)],
                                        env=env, capture_output=True, text=True, timeout=30)
                self.assertNotEqual(0, result.returncode)
                self.assertNotIn(term, result.stdout+result.stderr)
                self.assertNotIn("Traceback", result.stdout+result.stderr)
                self.assertIn("[TOOL]", result.stderr)

    def test_names_policy_errors_do_not_echo_private_paths(self):
        term = "fixture-confidential-selector"
        env = {**os.environ, "OBERTH_GUARDS_NAMES_FILE": str(self.root.parent/(term+self.root.name))}
        result = subprocess.run(["bash", "-x", "-c", 'source "$1/hack/steps.sh"; run python3 "$1/hack/guards/run.py" names HEAD', "guard-trace",
                                 str(test_run.GUARDS.ROOT)], env=env, capture_output=True, text=True, timeout=30)
        self.assertNotEqual(0, result.returncode)
        self.assertNotIn(term, result.stdout+result.stderr)
        self.assertNotIn("Traceback", result.stdout+result.stderr)
        self.assertIn("[TOOL]", result.stderr)

    def test_names_policy_decode_error_has_no_detail(self):
        with tempfile.NamedTemporaryFile(dir=self.root.parent) as policy:
            policy.write(b"fixture-confidential-selector\xff")
            policy.flush()
            env = {**os.environ, "OBERTH_GUARDS_NAMES_FILE": policy.name}
            result = subprocess.run(["python3", str(test_run.GUARDS.ROOT/"hack/guards/run.py"), "names", "HEAD"],
                                    env=env, capture_output=True, text=True, timeout=30)
        self.assertEqual(1, result.returncode)
        self.assertEqual("", result.stdout)
        self.assertEqual(test_run.GUARDS.finding("TOOL")+"\n", result.stderr)
        self.assertNotIn("fixture-confidential-selector", result.stdout+result.stderr)
        self.assertNotIn("named command", result.stderr)

    def test_marked_hosts_are_allowed_in_plugin_paths_and_imports(self):
        self.policy("host:fixture-host\nfixture-vendor\nfixture-model")
        self.write("agent/fixture-host/module.go", 'package plugin\nimport _ "example.invalid/fixture-host/sdk"')
        self.write(".fixture-host-plugin/marketplace.json", '{"name":"fixture-host", "author":{"name":"Fixture maintainer"}}')
        self.assertEqual([], test_run.GUARDS.names())
        self.git("commit", "-qm", "Add plugin fixture")
        self.assertEqual([], test_run.GUARDS.names("HEAD"))

    def test_host_names_stay_refused_outside_the_allowed_paths(self):
        self.policy("host:fixture-host")
        for path in ("cmd/fixture-host/main.go", ".fixture-host-plugin/other.json", "hack/plans/oracle.txt"):
            self.write(path, "fixture-host")
        rows = test_run.GUARDS.names()
        self.assertEqual(5,len(rows),rows)
        self.assertTrue(all("fixture-host" not in row.lower() for row in rows))

    def test_vendor_and_model_names_have_no_plugin_exception(self):
        self.policy("host:fixture-host\nfixture-vendor\nfixture-model")
        self.write("agent/fixture-host/module.go", "fixture-vendor\nfixture-model")
        self.write(".fixture-host-plugin/marketplace.json", "fixture-vendor\nfixture-model")
        self.write("README.md", "## Agent plugins\nfixture-vendor\nfixture-model\n")
        self.assertEqual(6,len(test_run.GUARDS.names()))

    def test_host_install_docs_allow_only_the_named_section(self):
        self.policy("host:fixture-host")
        self.write("README.md", "fixture-host\n## Agent plugins\ninstall fixture-host\n### Notes\nfixture-host\n## Development\nfixture-host\n")
        rows = test_run.GUARDS.names()
        self.assertEqual(2,len(rows),rows)
        self.assertTrue(any(row.startswith("README.md:1:") for row in rows))
        self.assertTrue(any(row.startswith("README.md:7:") for row in rows))

    def test_plugin_section_requires_one_level_two_heading(self):
        self.policy("host:fixture-host")
        for file in ("README.md", "docs/install.md"):
            for level in (1, 3, 4, 5, 6):
                with self.subTest(file=file, level=level):
                    self.write(file, "#"*level+" Agent plugins\nfixture-host\n")
                    rows = test_run.GUARDS.names()
                    self.assertTrue(any("level 2" in row for row in rows), rows)
                    self.assertTrue(any("[NAME]" in row for row in rows), rows)
            self.write(file, "## Agent plugins\nfixture-host\n## Other\n## Agent plugins\nfixture-host\n")
            for ref in ("", "HEAD"):
                if ref:
                    self.git("commit", "-qm", "Add duplicate section fixture")
                rows = test_run.GUARDS.names(ref)
                self.assertTrue(any("duplicate" in row.lower() for row in rows), rows)
                self.assertTrue(any(row.startswith(file+":5: [NAME]") for row in rows), rows)
            self.write(file, "## Agent plugins\nfixture-host\n")

    def test_plugin_section_ends_at_level_one_or_two_heading(self):
        self.policy("host:fixture-host")
        for heading in ("# Other", "## Other", "#", "## "):
            with self.subTest(heading=heading):
                self.write("README.md", "## Agent plugins\nfixture-host\n"+heading+"\nfixture-host\n")
                rows = test_run.GUARDS.names()
                self.assertEqual(1, len(rows), rows)
                self.assertTrue(rows[0].startswith("README.md:4: [NAME]"), rows)

    def test_fenced_plugin_headings_never_open_close_or_duplicate_the_section(self):
        self.policy("host:fixture-host")
        for fence, closer in (("```md", "```"), ("~~~~text", "~~~~~"), ("   ```", "   ```")):
            with self.subTest(fence=fence):
                self.write("README.md", fence+"\n## Agent plugins\n"+closer+"\nfixture-host\n"
                           "## Agent plugins\nfixture-host\n"+fence+"\n# Other\n## Agent plugins\n"+closer+"\nfixture-host\n")
                for ref in ("", "HEAD"):
                    if ref:
                        self.git("commit", "-qm", "Add fenced section fixture")
                    rows = test_run.GUARDS.names(ref)
                    self.assertEqual(1, len(rows), rows)
                    self.assertTrue(rows[0].startswith("README.md:4: [NAME]"), rows)

    def test_shorter_or_other_fence_delimiters_do_not_close_a_fence(self):
        self.policy("host:fixture-host")
        self.write("README.md", "````\n```\n~~~\n## Agent plugins\nfixture-host\n````\nfixture-host\n")
        rows = test_run.GUARDS.names()
        self.assertEqual(2, len(rows), rows)
        self.assertTrue(all("[NAME]" in row for row in rows), rows)

    def test_backtick_in_info_string_does_not_open_a_fence(self):
        self.policy("host:fixture-host")
        self.write("README.md", "## Agent plugins\n```invalid`info\n## Other\nfixture-host\n")
        rows = test_run.GUARDS.names()
        self.assertEqual(1, len(rows), rows)
        self.assertTrue(rows[0].startswith("README.md:4: [NAME]"), rows)

    def test_plugin_credit_verbs_require_a_host_match(self):
        self.policy("host:fixture-host")
        for verb in ("Built", "Made", "Powered"):
            for preposition in ("with", "by"):
                for subject, refused in (("fixture-host", True), ("fixture tooling", False)):
                    with self.subTest(verb=verb, preposition=preposition, subject=subject):
                        self.write("agent/credits.txt", f"{verb} {preposition} {subject}\n")
                        rows = test_run.GUARDS.names()
                        self.assertEqual(refused, any("[CREDIT]" in row for row in rows), rows)

    def test_plugin_manifest_credit_keys_cover_json_and_yaml(self):
        self.policy("host:fixture-host")
        for field in ("author", "authors", "contributor", "contributors", "owner", "maintainers"):
            for suffix in ("json", "yaml", "yml"):
                file = "agent/manifest."+suffix
                for subject, refused in (("fixture-host", True), ("Fixture maintainer", False)):
                    with self.subTest(field=field, suffix=suffix, subject=subject):
                        body = {"plugin": {field: [{"name": subject}]}, "name": "fixture-host"}
                        text = json.dumps(body, indent=2) if suffix=="json" else f"name: fixture-host\nplugin:\n  {field}:\n    - name: {subject}\n"
                        self.write(file, text)
                        for ref in ("", "HEAD"):
                            if ref:
                                self.git("commit", "-qm", "Add manifest credit fixture")
                            rows = test_run.GUARDS.names(ref)
                            self.assertEqual(refused, any("[CREDIT]" in row for row in rows), rows)
                self.git("rm", "-f", file)
                self.git("commit", "-qm", "Remove manifest credit fixture")

    def test_yaml_aliases_documents_and_quoted_keys_cannot_hide_credit(self):
        self.policy("host:fixture-host")
        for body in ('name: fixture-host\n---\nowner: fixture-host\n',
                     'identity: &identity\n  name: fixture-host\nmaintainers: [*identity]\n',
                     '"owner":\n  name: "fixture-\\u0068ost"\n'):
            with self.subTest(body=body):
                self.write("agent/manifest.yaml", body)
                self.assertTrue(any("[CREDIT]" in row for row in test_run.GUARDS.names()))

    def test_invalid_plugin_yaml_fails_without_exposing_its_contents(self):
        self.policy("host:fixture-host")
        self.write("agent/manifest.yaml", 'owner: [fixture-host\n')
        with self.assertRaises(test_run.GUARDS.NamesError) as raised:
            test_run.GUARDS.names()
        self.assertNotIn("fixture-host", str(raised.exception))

    def test_host_names_have_no_commit_message_or_identity_exception(self):
        self.policy("host:fixture-host")
        self.write("agent/module.go", "fixture-host")
        self.git("commit", "-qm", "Mention fixture-host")
        self.assertEqual(1,len(test_run.GUARDS.names()))
        with patch.dict(os.environ,{"GIT_COMMITTER_NAME":"fixture-host"}):
            self.git("commit","--allow-empty","-qm","Safe fixture")
        self.assertEqual(2,len(test_run.GUARDS.names()))

    def test_contributor_credit_is_refused_inside_plugin_allowances(self):
        self.policy("host:fixture-host")
        self.write("agent/module.go", "// Generated with fixture-host\n// Co-authored-by: fixture-host\n// Code generated by fixture-host; DO NOT EDIT.\n")
        self.write(".fixture-host-plugin/marketplace.json", '{"author":{"name":"fixture-host"}}')
        self.write("README.md", "## Agent plugins\nGenerated with fixture-host\n")
        rows = test_run.GUARDS.names()
        self.assertEqual(5,len(rows),rows)
        self.assertTrue(all("[CREDIT]" in row for row in rows),rows)

    def test_contributor_credit_stays_forbidden_without_a_host_match(self):
        self.policy("host:fixture-host")
        self.write("agent/ordinary.go", "// Code generated by a fixture compiler; DO NOT EDIT.\n// Generated with fixture tooling\n")
        rows = test_run.GUARDS.names()
        self.assertEqual(1,len(rows),rows)
        self.assertIn("[CREDIT]",rows[0])

    def test_unmarked_host_spelling_remains_an_unconditional_literal(self):
        self.policy("fixture-host")
        self.write("agent/fixture-host/module.go", "fixture-host")
        self.assertEqual(2,len(test_run.GUARDS.names()))

    def test_robot_credit_is_refused_but_escaped_matcher_source_is_allowed(self):
        self.policy("host:fixture-host")
        self.write("agent/credits.txt", "// \U0001f916 contributor\n")
        self.write("matcher.py", 'CREDIT_TEXT = ("\\U0001f916",)\n')
        rows = test_run.GUARDS.names()
        self.assertEqual(1,len(rows),rows)
        self.assertIn("[CREDIT]",rows[0])
        self.git("commit","-qm","Add matcher and credit fixtures")
        self.assertEqual(1,len(test_run.GUARDS.names("HEAD")))

    def test_marked_host_has_no_tag_exception(self):
        self.policy("host:fixture-host")
        self.git("tag","-a","plugin-fixture","-m","fixture-host")
        rows = test_run.GUARDS.names()
        self.assertEqual(1,len(rows),rows)
        self.assertTrue(rows[0].startswith("tag/"),rows)

    def test_multiline_author_objects_and_contributor_arrays_have_no_exception(self):
        self.policy("host:fixture-host")
        for field in ("author", "contributors"):
            value = {"name":"fixture-host"} if field=="author" else ["fixture-host"]
            self.write(".fixture-host-plugin/marketplace.json",json.dumps({"name":"fixture-host",field:value},indent=2))
            rows = test_run.GUARDS.names()
            self.assertTrue(any("[CREDIT]" in row for row in rows),rows)
        self.write(".fixture-host-plugin/marketplace.json",json.dumps({"name":"fixture-host","author":{"name":"Fixture maintainer"}},indent=2))
        self.assertEqual([],test_run.GUARDS.names())

    def test_plain_author_labels_do_not_count_as_host_naming(self):
        self.policy("host:fixture-host")
        self.write("agent/credits.txt","Author: fixture-host\nContributors: fixture-host\n")
        rows = test_run.GUARDS.names()
        self.assertEqual(2,len(rows),rows)
        self.assertTrue(all("[CREDIT]" in row for row in rows))

    def test_blank_host_marker_is_invalid(self):
        self.policy("host:")
        self.assertIn("[NAME-POLICY]",test_run.GUARDS.names()[0])


if __name__ == "__main__":
    unittest.main()
