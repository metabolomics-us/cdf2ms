# Downloading and releasing cdf2ms

Download binaries from https://github.com/metabolomics-us/cdf2ms/releases/latest.
The repository is private: downloads require GitHub access to the repository.

| Operating system | CPU | Archive suffix |
| --- | --- | --- |
| Linux | x64 | linux_amd64.tar.gz |
| Linux | ARM64 | linux_arm64.tar.gz |
| macOS | Intel | darwin_amd64.tar.gz |
| macOS | Apple Silicon | darwin_arm64.tar.gz |
| Windows | x64 | windows_amd64.zip |
| Windows | ARM64 | windows_arm64.zip |

Extract the archive and run `cdf2ms version` (`cdf2ms.exe version` on Windows).
No Go installation, Python installation, or NetCDF library is needed to run it.
The binaries are unsigned; macOS and Windows may require approval under local
security policy. Cross-compilation verifies that each target builds; automated
runtime tests run on the Linux CI host, not on all six operating-system/CPU pairs.

Each release includes `SHA256SUMS` and `release.json`. The manifest records the
source commit and platform artifacts. Verify an archive using `sha256sum -c
SHA256SUMS --ignore-missing` on Linux, `shasum -a 256 ARCHIVE` on macOS, or
`Get-FileHash ARCHIVE -Algorithm SHA256` in PowerShell, comparing its hash with
the matching entry in `SHA256SUMS`.

## Automatic releases

Woodpecker runs lint, Go tests, end-to-end conversion verification, release-tool
tests, and all six cross-compilations. Pull requests targeting `main` or `master`
run these checks without publishing. A push to the repository's default branch
(`main` or `master`), including a merged pull request, publishes after they pass.
Manual builds and tag events do not publish releases.

Versions are **v0.1.BUILD**, where BUILD is the repository's Woodpecker pipeline
number. For example, pipeline 27 publishes `v0.1.27`, with downloads named
`cdf2ms_v0.1.27_darwin_arm64.tar.gz`. The same version is embedded in each binary
and its conversion provenance. Pipeline numbers consumed by PRs or failed builds
leave intentional gaps. Rerunning a pipeline keeps its version.

A release remains a draft until all six archives, checksums, and the manifest are
uploaded and their SHA-256 digests verified. A retry reuses identical assets and
refuses different bytes or a tag pointing to another commit. Published releases
are never overwritten. GitHub selects the latest release using its legacy date
and semantic-version ordering rather than forcing every rerun to become latest.
If source or toolchain changes are needed, publish a new build instead of replacing
old assets.

The `github_release_token` Woodpecker repository secret is available only to
push events and needs GitHub permission to write releases in this repository.
Never place its value in Git. GitHub Actions remains a separate validation
workflow; release publication runs on the laboratory's Woodpecker fleet.

## Local cross-compilation

With Go 1.25+ and Python 3.11+ installed:

```sh
make test-release
make release VERSION=v0.1.123
```

The output is written to `dist/`, which must be empty or absent. Use a new output
folder for subsequent builds:

```sh
python3 scripts/build_release.py --version v0.1.124 --output /tmp/cdf2ms-v0.1.124
```

Local packaging does not publish anything. Publication additionally requires a
clean checkout matching the CI commit and the default-branch push context.
