#!/usr/bin/env python3
"""Build and package cdf2ms release binaries for supported platforms."""

import argparse
import datetime
import gzip
import hashlib
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import zipfile
from pathlib import Path


TARGETS = tuple(
    (goos, goarch)
    for goos in ("linux", "darwin", "windows")
    for goarch in ("amd64", "arm64")
)
VERSION_RE = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+\Z")
DOCUMENTS = ("README.md", "docs/usage.md", "docs/compatibility.md")


def validate_version(value):
    if not VERSION_RE.fullmatch(value):
        raise argparse.ArgumentTypeError("version must use strict vX.Y.Z form")
    return value


def prepare_output(path):
    path.mkdir(parents=True, exist_ok=True)
    if any(path.iterdir()):
        raise ValueError(f"output directory is not empty: {path}")


def _archive_entries(repo, binary, version, goos):
    binary_name = "cdf2ms.exe" if goos == "windows" else "cdf2ms"
    yield binary_name, binary.read_bytes(), 0o755
    for name in DOCUMENTS:
        yield name, (repo / name).read_bytes(), 0o644
    yield "VERSION", (version + "\n").encode(), 0o644


def _write_tar(path, entries, epoch):
    with path.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                for name, content, mode in entries:
                    info = tarfile.TarInfo(name)
                    info.size = len(content)
                    info.mode = mode
                    info.mtime = epoch
                    archive.addfile(info, io.BytesIO(content))


def _write_zip(path, entries, epoch):
    stamp = datetime.datetime.fromtimestamp(epoch, datetime.UTC)
    timestamp = (stamp.year, stamp.month, stamp.day, stamp.hour, stamp.minute, stamp.second)
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for name, content, mode in entries:
            info = zipfile.ZipInfo(name, date_time=timestamp)
            info.create_system = 3
            info.external_attr = (0o100000 | mode) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(info, content)


def package_artifact(repo, binary, output, version, goos, goarch, epoch=0):
    suffix = ".zip" if goos == "windows" else ".tar.gz"
    name = f"cdf2ms_{version}_{goos}_{goarch}{suffix}"
    entries = tuple(_archive_entries(repo, binary, version, goos))
    if goos == "windows":
        _write_zip(output / name, entries, epoch)
    else:
        _write_tar(output / name, entries, epoch)
    return {"target": f"{goos}/{goarch}", "name": name}


def _sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_metadata(output, version, commit, artifacts, pipeline_number=None):
    completed = []
    checksum_lines = []
    for artifact in artifacts:
        checksum = _sha256(output / artifact["name"])
        completed.append({**artifact, "checksum": checksum})
        checksum_lines.append(f"{checksum}  {artifact['name']}\n")
    (output / "SHA256SUMS").write_text("".join(checksum_lines), encoding="utf-8")
    manifest = {"version": version, "commit": commit, "artifacts": completed}
    if pipeline_number:
        manifest["pipeline_number"] = pipeline_number
    (output / "release.json").write_text(
        json.dumps(manifest, indent=2) + "\n", encoding="utf-8"
    )


def validate_build_info(info, version, commit, goos, goarch):
    expected = {
        "version": version,
        "revision": commit,
        "os": goos,
        "arch": goarch,
        "pureGo": True,
    }
    for key, value in expected.items():
        if info.get(key) != value:
            raise ValueError(
                f"native smoke check {key}: expected {value!r}, got {info.get(key)!r}"
            )


def build_release(repo, output, version):
    prepare_output(output)
    revision = subprocess.check_output(
        ["git", "show", "-s", "--format=%H%n%ct", "HEAD"], cwd=repo, text=True
    ).splitlines()
    commit, epoch = revision[0], int(revision[1])
    host = subprocess.check_output(
        ["go", "env", "GOHOSTOS", "GOHOSTARCH"], cwd=repo, text=True
    ).splitlines()
    host_target = tuple(host)
    artifacts = []
    with tempfile.TemporaryDirectory(prefix="cdf2ms-release-") as tmp:
        staging = Path(tmp)
        for goos, goarch in TARGETS:
            binary_name = "cdf2ms.exe" if goos == "windows" else "cdf2ms"
            binary = staging / f"{goos}-{goarch}-{binary_name}"
            env = os.environ.copy()
            env.update({"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch})
            subprocess.run(
                [
                    "go",
                    "build",
                    "-trimpath",
                    "-buildvcs=true",
                    "-ldflags",
                    f"-s -w -X main.version={version}",
                    "-o",
                    binary,
                    "./cmd/cdf2ms",
                ],
                cwd=repo,
                env=env,
                check=True,
            )
            if (goos, goarch) == host_target:
                info = json.loads(
                    subprocess.check_output(
                        [binary, "version", "-json"], cwd=repo, text=True
                    )
                )
                validate_build_info(info, version, commit, goos, goarch)
            artifacts.append(
                package_artifact(repo, binary, staging, version, goos, goarch, epoch)
            )
        for artifact in artifacts:
            shutil.move(staging / artifact["name"], output / artifact["name"])
    pipeline_number = os.environ.get("CI_PIPELINE_NUMBER")
    write_metadata(output, version, commit, artifacts, pipeline_number)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, type=validate_version)
    parser.add_argument("--output", default="dist", type=Path)
    args = parser.parse_args(argv)
    repo = Path(__file__).resolve().parents[1]
    try:
        build_release(repo, args.output.resolve(), args.version)
    except (OSError, subprocess.CalledProcessError, ValueError) as error:
        parser.exit(1, f"build_release: {error}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
