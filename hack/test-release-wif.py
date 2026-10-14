#!/usr/bin/env python3
"""Run every accepted WIF fixture with the source-pinned test dependency."""
import importlib.util
import os
from pathlib import Path
import sys
import unittest

root = Path(__file__).resolve().parents[1]
dependency = Path(os.environ['OBERTH_TOOLS_DIR']) / 'python'
if not (dependency / 'yaml/__init__.py').is_file():
    raise RuntimeError('pinned PyYAML test dependency is missing')
sys.path.insert(0, str(dependency))
import yaml  # noqa: E402
if yaml.__version__ != '6.0.3':
    raise RuntimeError('wrong PyYAML test dependency')
spec = importlib.util.spec_from_file_location('accepted_wif_tests', root / 'scripts/test_release_wif.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromModule(module))
if not result.wasSuccessful() or result.skipped:
    sys.exit(1)
