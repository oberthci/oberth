"""Inspections for the intended production permission graph."""
import copy
import json
from pathlib import Path
import runpy
import unittest
from unittest import mock


PARITY = runpy.run_path(str(Path(__file__).with_name("parity.py")))


class DependencyGraphTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.data = json.loads((PARITY["ROOT"] / "docs/plans/dependencies.json").read_text())
        cls.directories = set(cls.data["reference_packages"])

    def graph(self, data=None):
        return PARITY["intended_dependency_graph"](data if data is not None else self.data, self.directories)

    def assert_guard_rejects(self, data):
        with mock.patch.object(PARITY["json"], "loads", return_value=data):
            with self.assertRaisesRegex(AssertionError, "intended dependency permission cycles"):
                PARITY["dependency_contracts"]()

    def test_current_full_graph_and_contracts_are_acyclic(self):
        self.assertEqual([], PARITY["dependency_cycles"](self.graph()))
        self.assertEqual([], PARITY["dependency_contracts"]()["cyclic_components"])

    def test_approved_pure_helpers_import_their_values(self):
        graph = self.graph()
        self.assertIn("internal/publicerr", graph["internal/strictjson"])
        self.assertIn("internal/reponame", graph["internal/sourcecontract"])

    def test_pinned_runtime_contracts_keep_their_runtime_kind(self):
        graph = self.graph()
        for name in ("internal/restartretry",):
            self.assertIn(name, self.data["shared"])
            self.assertEqual("runtime", self.data["packages"][name]["kind"])
            self.assertEqual([], graph[name])
        self.assertNotIn("internal/redact", self.data["shared"])
        self.assertNotIn("internal/redact", graph)
        self.assertIn("internal/redact/**", self.data["retired"])

    def test_pure_contract_cannot_import_a_pinned_runtime_contract(self):
        data = copy.deepcopy(self.data)
        data["shared_dependencies"]["internal/actor"] = ["internal/restartretry"]
        with mock.patch.object(PARITY["json"], "loads", return_value=data):
            with self.assertRaisesRegex(AssertionError, "shared imports runtime"):
                PARITY["dependency_contracts"]()

    def test_sealing_is_restricted_without_blocking_read_access(self):
        permitted = PARITY["symbol_permitted"]
        target = "internal/admissioncontract"
        self.assertTrue(permitted(self.data, "pkg/argoworkflow", target, "Seal"))
        for caller in ("internal/app", "internal/argojob", "internal/workflowpolicy"):
            self.assertFalse(permitted(self.data, caller, target, "Seal"))
            self.assertTrue(permitted(self.data, caller, target, "Seal", test=True))
            self.assertTrue(permitted(self.data, caller, target, "PreparedView"))

    def test_wiring_calls_admission_with_policy_and_typed_error(self):
        permitted = PARITY["symbol_permitted"]
        target = "pkg/argoworkflow"
        self.assertIn(target, self.graph()["internal/app"])
        for symbol in ("Prepare", "Policy", "AdmissionError"):
            for caller in ("internal/app", "pkg/argoworkflow"):
                self.assertTrue(permitted(self.data, caller, target, symbol))
            for caller in ("internal/argojob", "internal/cronsync"):
                self.assertFalse(permitted(self.data, caller, target, symbol))
        self.assertFalse(permitted(self.data, "cmd/oberth", target, "PlannedSteps"))
        for symbol in ("Decode", "Prepare", "Policy", "AdmissionError"):
            self.assertTrue(permitted(self.data, "cmd/oberth", target, symbol))

    def test_owner_symbols_extend_common_symbols_without_granting_seal(self):
        data = copy.deepcopy(self.data)
        rule = next(r for r in data["rules"] if r["id"] == 15)
        rule["symbols"] = {"*": ["PreparedView"], "C5": ["ChildSpecDigest"]}
        permitted = PARITY["symbol_permitted"]
        target = "internal/admissioncontract"
        for caller in ("internal/argojob", "internal/app"):
            self.assertTrue(permitted(data, caller, target, "PreparedView"))
            self.assertFalse(permitted(data, caller, target, "Seal"))
        self.assertTrue(permitted(data, "internal/argojob", target, "ChildSpecDigest"))
        self.assertFalse(permitted(data, "internal/app", target, "ChildSpecDigest"))

    def test_convention_support_is_not_a_production_dependency(self):
        paths = [parent + "/" + suffix for parent in ("internal/gitport", "internal/cronsync", "internal/app") for suffix in ("fake", "contracttest", "steptest", "catalogtest")]
        self.assertIsNone(PARITY["test_support_parent"](self.data, "fake"))
        owned_root = copy.deepcopy(self.data)
        owned_root["components"]["C9"].append(".")
        self.assertEqual(".", PARITY["test_support_parent"](owned_root, "fake"))
        graph = PARITY["intended_dependency_graph"](self.data, set(paths) | self.directories)
        for path in paths:
            self.assertNotIn(path, graph)
            self.assertEqual(path.rsplit("/", 1)[0], PARITY["test_support_parent"](self.data, path))

    def test_product_cannot_gain_a_fake_through_an_allow_entry(self):
        data = copy.deepcopy(self.data)
        data["allow"]["C3"].append("internal/gitport/fake")
        with mock.patch.object(PARITY["json"], "loads", return_value=data):
            with self.assertRaisesRegex(AssertionError, "production permission to test support"):
                PARITY["dependency_contracts"]()

    def test_status_catalog_support_is_tests_only(self):
        support = "internal/stepmodel/steptest"
        self.assertNotIn(support, self.graph())
        self.assertEqual("internal/stepmodel", PARITY["test_support_parent"](self.data, support))
        self.assertNotIn(support, self.data["packages"])
        data = copy.deepcopy(self.data)
        data["allow"]["C5"].append(support)
        with mock.patch.object(PARITY["json"], "loads", return_value=data):
            with self.assertRaisesRegex(AssertionError, "unowned package directories"):
                PARITY["dependency_contracts"]()

    def test_ordinary_production_package_ending_in_test_is_audited(self):
        data = copy.deepcopy(self.data)
        package = "internal/runlog/latest"
        data["allow"]["C8"].append(package)
        data["package_allow"][package] = []
        self.assertIsNone(PARITY["test_support_parent"](data, package))
        self.assertIn(package, self.graph(data))
        with mock.patch.object(PARITY["json"], "loads", return_value=data):
            self.assertEqual([], PARITY["dependency_contracts"]()["cyclic_components"])

    def test_later_source_and_execution_cannot_readmit_yaml(self):
        target = "pkg/argoworkflow"
        self.assertNotIn(target, self.graph()["internal/sourcedelivery"])
        for symbol in ("Admit", "Prepare", "Decode"):
            self.assertFalse(PARITY["symbol_permitted"](self.data, "internal/argojob", target, symbol))

    def test_runtime_client_cannot_import_its_installer_consumer(self):
        data = copy.deepcopy(self.data)
        data["package_allow"]["internal/client"].append("internal/installer")
        self.assertIn(["internal/client", "internal/installer"], PARITY["dependency_cycles"](self.graph(data)))
        self.assert_guard_rejects(data)

    def test_three_runtime_packages_form_a_cycle_without_reciprocal_pairs(self):
        data = copy.deepcopy(self.data)
        data["package_allow"]["internal/client"] = ["internal/installer"]
        data["package_allow"]["internal/installer"] = ["internal/secretstore"]
        data["package_allow"]["internal/secretstore"] = ["internal/client"]
        self.assertIn(["internal/client", "internal/installer", "internal/secretstore"], PARITY["dependency_cycles"](self.graph(data)))
        self.assert_guard_rejects(data)

    def test_same_owner_parser_helper_cycle_is_rejected(self):
        data = copy.deepcopy(self.data)
        data["package_allow"]["pkg/argoworkflow/defaults"] = ["pkg/argoworkflow/specsize"]
        data["package_allow"]["pkg/argoworkflow/specsize"] = ["pkg/argoworkflow/defaults"]
        self.assert_guard_rejects(data)

    def test_shared_cycle_is_rejected(self):
        data = copy.deepcopy(self.data)
        data["shared_dependencies"]["internal/actor"] = ["internal/model"]
        self.assert_guard_rejects(data)

    def test_new_package_named_only_in_permissions_is_audited(self):
        data = copy.deepcopy(self.data)
        helper = "internal/client/helper"
        data["allow"]["C11"].append(helper)
        data["package_allow"][helper] = ["internal/client"]
        data["package_allow"]["internal/client"].append(helper)
        self.assertIn(["internal/client", helper], PARITY["dependency_cycles"](self.graph(data)))
        self.assert_guard_rejects(data)

    def test_deny_wins_over_component_permission(self):
        data = copy.deepcopy(self.data)
        data["allow"]["C3"].append("internal/store")
        self.assertNotIn("internal/store", self.graph(data)["internal/app"])

    def test_overrides_shared_permissions_and_test_exceptions_stay_separate(self):
        data = copy.deepcopy(self.data)
        data["test_allow"]["internal/client"] = ["internal/installer"]
        graph = self.graph(data)
        self.assertNotIn("internal/installer", graph["internal/client"])
        self.assertNotIn("internal/api", graph["internal/client"])
        self.assertNotIn("internal/clientcert/issuer", graph["internal/model"])
        self.assertIn("internal/sourcedelivery", graph["internal/sourceplugin"])
        self.assertNotIn("internal/testdb", graph)
        self.assertNotIn("hack/upgrade-e2e/fixture", graph)

    def test_strongly_connected_sets_are_complete_and_deterministic(self):
        graph = {"a": ["b"], "b": ["c"], "c": ["a"], "d": ["e"], "e": ["d"], "f": []}
        expected = [["a", "b", "c"], ["d", "e"]]
        self.assertEqual(expected, PARITY["dependency_cycles"](graph))
        self.assertEqual(expected, PARITY["dependency_cycles"](dict(reversed(list(graph.items())))))


if __name__ == "__main__":
    unittest.main()
