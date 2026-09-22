#!/usr/bin/env python3
import hashlib
import importlib.util
import json
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "build_release.py"


def load_builder():
    spec = importlib.util.spec_from_file_location("build_release", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ReleaseBuilderTest(unittest.TestCase):
    def test_cli_rejects_unsafe_version(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = subprocess.run(
                [sys.executable, SCRIPT, "--version", "v1.2.3;bad", "--output", tmp],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("vX.Y.Z", result.stderr)

    def test_cli_refuses_nonempty_output_directory(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "dist"
            output.mkdir()
            marker = output / "keep.txt"
            marker.write_text("keep", encoding="utf-8")
            result = subprocess.run(
                [sys.executable, SCRIPT, "--version", "v1.2.3", "--output", output],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(marker.read_text(encoding="utf-8"), "keep")
            self.assertEqual(list(output.iterdir()), [marker])

    def test_packages_tar_and_zip_with_expected_contents_and_modes(self):
        builder = load_builder()
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            repo = base / "repo"
            output = base / "dist"
            repeated = base / "repeated"
            repo.mkdir()
            output.mkdir()
            repeated.mkdir()
            (repo / "README.md").write_text("readme\n", encoding="utf-8")
            (repo / "docs").mkdir()
            (repo / "docs" / "usage.md").write_text("usage\n", encoding="utf-8")
            (repo / "docs" / "compatibility.md").write_text("compat\n", encoding="utf-8")
            (repo / "docs" / "releases.md").write_text("releases\n", encoding="utf-8")

            linux_bin = base / "cdf2ms-linux"
            linux_bin.write_bytes(b"linux")
            win_bin = base / "cdf2ms.exe"
            win_bin.write_bytes(b"windows")
            tar_artifact = builder.package_artifact(
                repo, linux_bin, output, "v1.2.3", "linux", "amd64", 1_700_000_000
            )
            zip_artifact = builder.package_artifact(
                repo, win_bin, output, "v1.2.3", "windows", "arm64", 1_700_000_000
            )
            builder.package_artifact(
                repo, linux_bin, repeated, "v1.2.3", "linux", "amd64", 1_700_000_000
            )
            builder.package_artifact(
                repo, win_bin, repeated, "v1.2.3", "windows", "arm64", 1_700_000_000
            )

            with tarfile.open(output / tar_artifact["name"], "r:gz") as archive:
                names = {member.name for member in archive.getmembers()}
                self.assertEqual(
                    names,
                    {"cdf2ms", "README.md", "docs/usage.md", "docs/compatibility.md", "docs/releases.md", "VERSION"},
                )
                self.assertEqual(archive.getmember("cdf2ms").mode, 0o755)
                self.assertEqual(archive.getmember("cdf2ms").mtime, 1_700_000_000)
                self.assertEqual(archive.getmember("README.md").mode, 0o644)
                self.assertEqual(archive.extractfile("VERSION").read(), b"v1.2.3\n")

            with zipfile.ZipFile(output / zip_artifact["name"]) as archive:
                self.assertEqual(
                    set(archive.namelist()),
                    {"cdf2ms.exe", "README.md", "docs/usage.md", "docs/compatibility.md", "docs/releases.md", "VERSION"},
                )
                mode = archive.getinfo("cdf2ms.exe").external_attr >> 16
                self.assertEqual(stat.S_IMODE(mode), 0o755)
                self.assertEqual(
                    archive.getinfo("cdf2ms.exe").date_time, (2023, 11, 14, 22, 13, 20)
                )
                self.assertEqual(archive.read("VERSION"), b"v1.2.3\n")
            self.assertEqual(
                (output / tar_artifact["name"]).read_bytes(),
                (repeated / tar_artifact["name"]).read_bytes(),
            )
            self.assertEqual(
                (output / zip_artifact["name"]).read_bytes(),
                (repeated / zip_artifact["name"]).read_bytes(),
            )

    def test_metadata_checksums_only_archives(self):
        builder = load_builder()
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp)
            first = output / "cdf2ms_v1.2.3_linux_amd64.tar.gz"
            second = output / "cdf2ms_v1.2.3_windows_arm64.zip"
            first.write_bytes(b"linux archive")
            second.write_bytes(b"windows archive")
            artifacts = [
                {"target": "linux/amd64", "name": first.name},
                {"target": "windows/arm64", "name": second.name},
            ]

            builder.write_metadata(output, "v1.2.3", "a" * 40, artifacts, "27")

            expected_first = hashlib.sha256(first.read_bytes()).hexdigest()
            expected_second = hashlib.sha256(second.read_bytes()).hexdigest()
            self.assertEqual(
                (output / "SHA256SUMS").read_text(encoding="utf-8"),
                f"{expected_first}  {first.name}\n{expected_second}  {second.name}\n",
            )
            manifest = json.loads((output / "release.json").read_text(encoding="utf-8"))
            self.assertEqual(manifest["version"], "v1.2.3")
            self.assertEqual(manifest["commit"], "a" * 40)
            self.assertEqual(manifest["pipeline_number"], "27")
            self.assertEqual(
                manifest["artifacts"],
                [
                    {"target": "linux/amd64", "name": first.name, "checksum": expected_first},
                    {"target": "windows/arm64", "name": second.name, "checksum": expected_second},
                ],
            )

    def test_native_smoke_rejects_wrong_embedded_revision(self):
        builder = load_builder()
        info = {
            "version": "v1.2.3",
            "revision": "wrong",
            "os": "darwin",
            "arch": "arm64",
            "pureGo": True,
        }
        with self.assertRaisesRegex(ValueError, "revision"):
            builder.validate_build_info(
                info, "v1.2.3", "a" * 40, "darwin", "arm64"
            )


if __name__ == "__main__":
    unittest.main()
