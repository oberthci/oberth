"""Production command roots are explicit paths in the closed plan schema."""
import copy
import json
from pathlib import Path
import runpy
import unittest


VALIDATE = runpy.run_path(str(Path(__file__).with_name("policy.py")))["dependencies"]
ROOT = Path(__file__).resolve().parents[2]


class ProductionCommands(unittest.TestCase):
    def setUp(self):
        self.data = json.loads((ROOT/"docs/plans/dependencies.json").read_text())

    def test_exact_future_commands_are_allowed_without_source(self):
        self.data["production_commands"] = ["cmd/future", "cmd/nested/future"]
        self.data["product_command"] = "cmd/future"
        VALIDATE(self.data)

    def test_empty_list_cannot_mark_a_product(self):
        self.data["production_commands"] = []
        with self.assertRaisesRegex(AssertionError, "product_command"):
            VALIDATE(self.data)

    def test_product_must_name_one_listed_command(self):
        for product in (None, 1, [], ["cmd/oberth"], "", "cmd/unlisted"):
            with self.subTest(product=product):
                self.data["product_command"] = product
                with self.assertRaisesRegex(AssertionError, "product_command"):
                    VALIDATE(self.data)

    def test_missing_product_marker_is_a_schema_error(self):
        del self.data["product_command"]
        with self.assertRaisesRegex(AssertionError, "product_command"):
            VALIDATE(self.data)

    def test_roots_reject_patterns_duplicates_and_non_command_paths(self):
        for commands in (None, "cmd/oberth", [1], [""], ["cmd/oberth", "cmd/oberth"],
                         ["cmd/*"], ["cmd/oberth/**"], ["@C13"], ["internal/release"],
                         ["cmd/../outside"], ["cmd/oberth/"], [" cmd/oberth"]):
            with self.subTest(commands=commands):
                data = copy.deepcopy(self.data)
                data["production_commands"] = commands
                with self.assertRaisesRegex(AssertionError, "production_commands"):
                    VALIDATE(data)

    def test_missing_command_list_is_a_schema_error(self):
        del self.data["production_commands"]
        with self.assertRaisesRegex(AssertionError, "production_commands"):
            VALIDATE(self.data)
