"""Pin the optional browser gate's explicit skip and missing-runtime outcomes."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class BrowserGate(unittest.TestCase):
    def run_gate(self, browser, commands=None):
        root = Path(__file__).resolve().parents[1]
        scratch = Path(os.environ.get('GOTMPDIR', Path.home() / '.cache/go-tmp'))
        scratch.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=scratch) as directory:
            if browser:
                executable = Path(directory) / 'chromium'
                executable.write_text('#!/bin/sh\nexit 0\n')
                executable.chmod(0o755)
            for name, source in (commands or {}).items():
                executable = Path(directory) / name
                executable.write_text(source)
                executable.chmod(0o755)
            environment = {**os.environ, 'PATH': directory}
            environment.pop('OBERTH_UI_ARTIFACTS', None)
            return subprocess.run(['/bin/bash', 'hack/ui-check.sh'], cwd=root,
                                  env=environment,
                                  capture_output=True, text=True, check=False)

    def test_missing_browser_is_an_explicit_skip(self):
        result = self.run_gate(False)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('ui-browser: SKIPPED (chromium is not on PATH)', result.stdout)

    def test_present_browser_requires_the_checker_runtime(self):
        result = self.run_gate(True)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn('node is required', result.stderr)

    def test_hard_limit_reports_the_stalled_step_and_uses_ignored_output(self):
        result = self.run_gate(True, {
            'node': '#!/bin/sh\nexit 0\n',
            'mkdir': '#!/bin/sh\nexec /bin/mkdir "$@"\n',
            'timeout': '''#!/bin/sh
[ "$1" = --signal=KILL ] && [ "$2" = 60s ] || exit 2
[ "$OBERTH_UI_ARTIFACTS" = "$PWD/.cache/webui" ] || exit 3
printf 'stalled browser assertion' > "$OBERTH_UI_STEP_FILE"
exit 137
''',
        })
        self.assertEqual(result.returncode, 137, result.stdout + result.stderr)
        self.assertEqual(result.stderr,
                         'ui-browser: FAIL (60s limit: stalled browser assertion)\n')
        self.assertNotIn('ui-browser: PASS', result.stdout)
