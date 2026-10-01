#!/usr/bin/env python3
"""Contract test: the Oberth release finalizer prunes orphan objects from latest/.

Structural contract test verifying the prune block exists in release.sh with
the correct converged-set definition, fail-closed semantics, and
AGENT-CONTRACT documentation. (#703/#719)
"""
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


class PruneContractTests(unittest.TestCase):
    def test_finalize_contains_prune_block(self):
        """The finalize action must LIST oberth/latest/ and DELETE orphans."""
        script = (ROOT / '.oberth/release.sh').read_text()
        self.assertIn('list-type=2&prefix=oberth/latest/', script)
        self.assertIn('--request DELETE', script)
        self.assertIn('pruned orphan oberth/latest/', script)

    def test_prune_fails_closed_on_list_failure(self):
        """A failed listing must abort the release without any deletion."""
        script = (ROOT / '.oberth/release.sh').read_text()
        self.assertIn('listing transport failed', script)
        self.assertIn('listing returned status', script)

    def test_prune_refuses_keys_outside_prefix(self):
        """Keys outside oberth/latest/ must abort the release."""
        script = (ROOT / '.oberth/release.sh').read_text()
        self.assertIn('listing returned key outside oberth/latest/ prefix', script)
        self.assertIn('key outside prefix', script)

    def test_prune_converged_set_definition(self):
        """The converged set must include the Oberth-specific fixed aliases."""
        script = (ROOT / '.oberth/release.sh').read_text()
        # Oberth uses SHA256SUMS.sigstore.json (cosign bundle), not .sig.
        # No release.json or release.json.sig.
        self.assertIn('"VERSION", "SHA256SUMS", "SHA256SUMS.sigstore.json", "cosign.pub"', script)

    def test_prune_does_not_include_cli_artifacts(self):
        """The converged set must not include non-Oberth artifacts."""
        script = (ROOT / '.oberth/release.sh').read_text()
        # Find the inline python prune script section.
        prune_start = script.index('list-type=2&prefix=oberth/latest/')
        prune_section = script[prune_start - 500:prune_start + 2000]
        # Oberth does not ship release.json or SHA256SUMS.sig.
        self.assertNotIn('"release.json"', prune_section)
        self.assertNotIn('"SHA256SUMS.sig"', prune_section)

    def test_prune_uses_r2_curl_config(self):
        """The prune must use the same R2 credential config as finalize."""
        script = (ROOT / '.oberth/release.sh').read_text()
        prune_start = script.index('list-type=2&prefix=oberth/latest/')
        prune_section = script[prune_start - 500:prune_start + 2000]
        self.assertIn('r2_curl_config', prune_section)

    def test_prune_fails_closed_on_truncation(self):
        """A truncated listing must abort the release."""
        script = (ROOT / '.oberth/release.sh').read_text()
        self.assertIn('listing truncated', script)

    def test_agent_contract_documents_prune(self):
        """AGENT-CONTRACT.md must state the prune contract."""
        contract = (ROOT / 'AGENT-CONTRACT.md').read_text()
        self.assertIn('no unlisted objects survive', contract.lower())
        self.assertIn('#719', contract)


if __name__ == '__main__':
    unittest.main()
