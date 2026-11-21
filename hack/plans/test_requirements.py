"""Negative controls for compact requirement ownership and inspections."""
import copy
from pathlib import Path
import runpy
import unittest

PARITY = runpy.run_path(str(Path(__file__).with_name("parity.py")))


class RequirementContractTests(unittest.TestCase):
    def setUp(self):
        self.data = {"version": 1, "requirements": []}
        self.specs = {}
        for number in (1, 2):
            rid = f"REQ-C{number}-001"
            name = f"TestBoundary{number}Contract"
            self.data["requirements"].append({
                "id": rid, "component": f"C{number}", "class": "security",
                "requirement": "Refuse an unbound identity.",
                "loss_if_absent": "Without this, another identity gains access.",
                "inspection": {"kind": "contract_test", "name": name},
            })
            self.specs[f"docs/plans/c{number}-boundary.md"] = (
                f"## Invariants and budgets\n\nKeep identity bound.\n\n## Inspections\n\n{rid}: {name}\n")

    def validate(self):
        return PARITY["requirement_contracts"](self.data, self.specs)

    def test_valid_register_has_one_owner_and_inspection(self):
        self.assertEqual(2, self.validate()["requirements"])

    def test_evidence_cannot_return_to_a_requirement(self):
        self.data["requirements"][0]["evidence"] = [{"file": "old.go"}]
        with self.assertRaisesRegex(AssertionError, "non-normative fields"):
            self.validate()

    def test_review_metadata_cannot_return_to_register(self):
        self.data["source_reports"] = []
        with self.assertRaisesRegex(AssertionError, "schema"):
            self.validate()

    def test_missing_component_is_rejected(self):
        self.data["requirements"][0]["component"] = "C3"
        with self.assertRaisesRegex(AssertionError, "identity"):
            self.validate()

    def test_uncited_rule_is_rejected(self):
        self.specs["docs/plans/c1-boundary.md"] = "## Invariants\n\n## Inspections\n"
        with self.assertRaisesRegex(AssertionError, "exactly its owning spec"):
            self.validate()

    def test_two_specs_cannot_claim_one_requirement(self):
        self.specs["docs/plans/c2-boundary.md"] += "\nREQ-C1-001\n"
        with self.assertRaisesRegex(AssertionError, "exactly its owning spec"):
            self.validate()

    def test_unknown_citation_is_rejected(self):
        self.specs["docs/plans/c1-boundary.md"] += "\nREQ-C1-999\n"
        with self.assertRaisesRegex(AssertionError, "unknown cited"):
            self.validate()

    def test_wrong_owner_is_rejected(self):
        self.specs["docs/plans/c1-boundary.md"] = "## Invariants\n\n## Inspections\n"
        self.specs["docs/plans/c2-boundary.md"] += "\nREQ-C1-001\n"
        with self.assertRaisesRegex(AssertionError, "exactly its owning spec"):
            self.validate()

    def test_missing_inspection_is_rejected(self):
        self.data["requirements"][0]["inspection"]["name"] = ""
        with self.assertRaisesRegex(AssertionError, "inspection missing"):
            self.validate()

    def test_inspection_must_be_in_its_section(self):
        self.specs["docs/plans/c1-boundary.md"] = "## Invariants\n\nREQ-C1-001 TestBoundary1Contract\n\n## Inspections\n"
        with self.assertRaisesRegex(AssertionError, "inspection absent"):
            self.validate()

    def test_duplicate_id_and_unknown_class_are_rejected(self):
        self.data["requirements"].append(copy.deepcopy(self.data["requirements"][0]))
        with self.assertRaisesRegex(AssertionError, "duplicate requirement"):
            self.validate()
        self.data["requirements"].pop()
        self.data["requirements"][0]["class"] = "ops"
        with self.assertRaisesRegex(AssertionError, "invalid requirement class"):
            self.validate()

    def test_user_loss_is_required(self):
        self.data["requirements"][0]["loss_if_absent"] = ""
        with self.assertRaisesRegex(AssertionError, "empty or multiline"):
            self.validate()

    def test_register_cannot_exceed_compact_budget(self):
        self.data["requirements"][0]["requirement"] = "x" * 100000
        with self.assertRaisesRegex(AssertionError, "compact budget"):
            self.validate()

    def test_ux_has_a_walk(self):
        entry = self.data["requirements"][0]
        entry["class"] = "ux"
        with self.assertRaisesRegex(AssertionError, "needs a walk"):
            self.validate()
        entry["inspection"]["kind"] = "walk_step"
        self.assertEqual(1, self.validate()["by_component_class"]["C1:ux"])

    def test_default_component_limit_is_ten(self):
        for number in range(3, 13):
            entry = copy.deepcopy(self.data["requirements"][0])
            entry["id"] = f"REQ-C1-{number:03}"
            self.data["requirements"].append(entry)
        with self.assertRaisesRegex(AssertionError, "component requirement limit exceeded"):
            self.validate()

    def test_c11_exception_accepts_thirteen_and_refuses_fourteen(self):
        self.data["component_limits"] = {"C11": 13}
        self.data["requirements"] = []
        self.specs = {"docs/plans/c11-boundary.md": "## Invariants\n\n## Inspections\n"}
        for number in range(1, 15):
            rid = f"REQ-C11-{number:03}"
            self.data["requirements"].append({
                "id": rid, "component": "C11", "class": "security",
                "requirement": "Refuse an unbound identity.",
                "loss_if_absent": "Another identity gains access.",
                "inspection": {"kind": "contract_test", "name": "TestBoundaryContract"},
            })
            self.specs["docs/plans/c11-boundary.md"] += f"{rid} TestBoundaryContract\n"
            if number == 13:
                self.assertEqual(13, self.validate()["requirements"])
        with self.assertRaisesRegex(AssertionError, "component requirement limit exceeded"):
            self.validate()

    def test_exception_does_not_raise_other_components_limit(self):
        self.data["component_limits"] = {"C1": 12}
        with self.assertRaisesRegex(AssertionError, "unsupported component requirement limit"):
            self.validate()

    def test_retired_id_cannot_be_reused(self):
        self.data["retired_ids"] = {"REQ-C1-001": "merged into REQ-C2-001"}
        with self.assertRaisesRegex(AssertionError, "retired requirement id reused"):
            self.validate()

    def test_rendering_is_deterministic_and_concise(self):
        render = PARITY["render_requirements"]
        text = render(self.data, "a" * 64)
        self.assertEqual(text, render(self.data, "a" * 64))
        self.assertIn("### REQ-C1-001", text)
        self.assertIn("Without this", text)
        self.assertNotIn("Evidence", text)
        self.assertLess(len(text.splitlines()), 25)


if __name__ == "__main__":
    unittest.main()
