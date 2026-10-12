#!/usr/bin/env python3
"""Synthetic-only publication gate regression tests."""
import tempfile
import unittest
import io
import tarfile
from pathlib import Path

import safe_publish as gate


class PublicationGateTests(unittest.TestCase):
    def test_synthetic_credentials_are_detected_without_value_output(self):
        fake_key = b"rk_test_" + b"a" * 30
        fake_webhook = b"whsec_" + b"b" * 30
        with tempfile.TemporaryDirectory() as temp:
            fixture = Path(temp) / "fixture.txt"
            fixture.write_bytes(fake_key + b"\n" + fake_webhook)
            self.assertEqual(set(gate.scan_file(fixture)), {"provider_key", "webhook_secret"})

    def test_local_data_and_review_evidence_are_not_exported(self):
        for name in (".env", ".env.stage0.secrets", "tmp/report.json", "docs/reviews/live.md",
                     "docs/stage-0-owner-setup.md", "local.sqlite", "cmd/../../.env"):
            self.assertFalse(gate.eligible(name), name)
        for name in ("cmd/api/main.go", "internal/api/ui.go", "migrations/00027_reviewed_contact_link.sql",
                     ".env.example", "docs/architecture.md", "deploy/hubspot-project/hsproject.json",
                     "docs/licenses/vanilla-calendar-pro-MIT.txt", "docs/licenses/htmx-0BSD.txt"):
            self.assertTrue(gate.eligible(name), name)

    def test_runtime_assets_and_local_test_inputs_are_exported(self):
        for name in ("internal/api/assets/app.css", "internal/api/assets/historical-picker.css",
                     "internal/api/assets/lenoirdesk-mark.svg", "internal/api/assets/vanilla-calendar.css",
                     "deploy/dex/password.html", "deploy/dex/lenoirdesk.css",
                     "db/queries/health.sql", "tests/historical_picker.test.cjs",
                     "docs/screenshots/overview.jpg"):
            self.assertTrue(gate.eligible(name), name)
        self.assertFalse(gate.eligible("docs/reviews/assets/private.jpg"))

    def test_required_notice_omission_blocks_export(self):
        included = ["internal/api/assets/vanilla-calendar.js", "internal/api/assets/htmx.min.js"]
        self.assertEqual(set(gate.missing_notices(included)), set(gate.REQUIRED_NOTICES))
        self.assertEqual(gate.missing_notices(included + list(gate.REQUIRED_NOTICES)), [])

    def test_examples_are_not_actual_credentials(self):
        self.assertEqual(gate.categories(b"RELAYDESK_OIDC_CLIENT_SECRET=demo-only-placeholder-value"), [])
        self.assertEqual(gate.categories(b"RELAYDESK_OIDC_CLIENT_SECRET=" + b"z" * 30), ["app_secret_assignment"])

    def test_oci_layer_is_scanned_including_large_files(self):
        synthetic = b"rk_test_" + b"c" * 30
        with tempfile.TemporaryDirectory() as temp:
            outer = Path(temp) / "image.tar"
            nested = io.BytesIO()
            with tarfile.open(fileobj=nested, mode="w") as layer:
                data = b"x" * (gate.MAX_SOURCE_BYTES + 1) + synthetic
                item = tarfile.TarInfo("app/binary")
                item.size = len(data)
                layer.addfile(item, io.BytesIO(data))
            blob = nested.getvalue()
            with tarfile.open(outer, mode="w") as archive:
                item = tarfile.TarInfo("blobs/sha256/synthetic-layer")
                item.size = len(blob)
                archive.addfile(item, io.BytesIO(blob))

            findings = gate.check_image_archive(outer)
            self.assertIn(("image layer", "provider_key"), findings)
            self.assertIn(("image layer", "missing_notice:vanilla-calendar-pro-MIT.txt"), findings)


if __name__ == "__main__":
    unittest.main()
