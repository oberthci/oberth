#!/usr/bin/env python3
"""Mandatory assertion identities without publishing unittest diagnostics."""
import json
from pathlib import Path
import re
import sys
import unittest


class Events(unittest.TestResult):
    def __init__(self, output):
        super().__init__()
        self.output, self.failed = output, set()

    def emit(self, test, action):
        test = getattr(test, 'test_case', test)
        name = '.'.join((test.__class__.__module__, test.__class__.__name__, test._testMethodName))
        if not re.fullmatch(r'[A-Za-z0-9_.]{1,240}', name):
            name = 'unavailable'
        self.output.write(json.dumps({'test': name, 'action': action}) + '\n')
        self.output.flush()

    def startTest(self, test):
        super().startTest(test)
        self.emit(test, 'run')

    def addError(self, test, error):
        super().addError(test, error)
        self.failed.add(test)
        self.emit(test, 'fail')

    def addFailure(self, test, error):
        super().addFailure(test, error)
        self.failed.add(test)
        self.emit(test, 'fail')

    def addSubTest(self, test, subtest, error):
        super().addSubTest(test, subtest, error)
        if error is not None:
            self.failed.add(test)
            self.emit(test, 'fail')

    def addSkip(self, test, reason):
        super().addSkip(test, reason)
        self.failed.add(test)
        self.emit(test, 'skip')

    def stopTest(self, test):
        if test not in self.failed:
            self.emit(test, 'pass')
        super().stopTest(test)


def run_suite(suite, output):
    result = Events(output)
    suite.run(result)
    return result.testsRun > 0 and result.wasSuccessful() and not result.skipped


def main():
    directory = str(Path(__file__).resolve().parent)
    sys.path.insert(0, directory)
    suite = unittest.defaultTestLoader.discover(directory, pattern='test_*.py')
    return 0 if run_suite(suite, sys.stdout) else 1


if __name__ == '__main__':
    sys.exit(main())
