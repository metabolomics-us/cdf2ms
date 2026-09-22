#!/usr/bin/env python3
"""Publish verified default-branch CI artifacts as an immutable GitHub release."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import urllib.error
import urllib.parse
import urllib.request

REPO = 'metabolomics-us/cdf2ms'


def ci_context(env):
    branch = env.get('CI_COMMIT_BRANCH')
    if (env.get('CI_PIPELINE_EVENT') != 'push' or branch not in ('main', 'master')
            or branch != env.get('CI_REPO_DEFAULT_BRANCH') or env.get('CI_REPO') != REPO):
        raise ValueError('Releases require a push to this repository default branch (main/master)')
    number = env.get('CI_PIPELINE_NUMBER', '')
    commit = env.get('CI_COMMIT_SHA', '')
    if not re.fullmatch(r'[1-9][0-9]*', number) or not re.fullmatch(r'[0-9a-f]{40}', commit):
        raise ValueError('Invalid CI build number or commit SHA')
    return 'v0.1.' + number, commit


def validate_artifacts(root, version, commit):
    manifest = json.loads((root / 'release.json').read_text())
    if manifest.get('version') != version or manifest.get('commit') != commit:
        raise ValueError('Artifact version/commit does not match this pipeline')
    expected = {f'cdf2ms_{version}_{system}_{arch}' + ('.zip' if system == 'windows' else '.tar.gz')
                for system in ('linux', 'darwin', 'windows') for arch in ('amd64', 'arm64')}
    checksums = {}
    for line in (root / 'SHA256SUMS').read_text().splitlines():
        digest, name = line.split('  ', 1)
        if name in checksums or name not in expected or not re.fullmatch('[0-9a-f]{64}', digest):
            raise ValueError('Unexpected or duplicate checksum entry')
        checksums[name] = digest
    if set(checksums) != expected:
        raise ValueError('Release must contain all six platform archives')
    for name, digest in checksums.items():
        if hashlib.sha256((root / name).read_bytes()).hexdigest() != digest:
            raise ValueError('Checksum mismatch: ' + name)
    return [root / name for name in sorted(expected)] + [root / 'SHA256SUMS', root / 'release.json']


class GitHub:
    def __init__(self, token):
        self.token = token

    def request(self, method, path, data=None, raw=False):
        url = path if path.startswith('https://uploads.github.com/') else 'https://api.github.com/repos/' + REPO + path
        headers = {'Authorization': 'Bearer ' + self.token, 'Accept': 'application/vnd.github+json',
                   'X-GitHub-Api-Version': '2022-11-28', 'User-Agent': 'cdf2ms-release'}
        if data is not None:
            headers['Content-Type'] = 'application/octet-stream' if raw else 'application/json'
            if not raw:
                data = json.dumps(data).encode()
        req = urllib.request.Request(url, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=180) as response:
                body = response.read()
                return json.loads(body) if body else None
        except urllib.error.HTTPError as error:
            if error.code == 404 and method == 'GET':
                return None
            raise RuntimeError(f'GitHub {method} failed with HTTP {error.code}') from None


def publish(api, version, commit, files):
    tag = api.request('GET', '/git/ref/tags/' + version)
    if tag and (tag['object']['type'] != 'commit' or tag['object']['sha'] != commit):
        raise ValueError('Existing release tag points to a different commit')
    release = api.request('GET', '/releases/tags/' + version)
    if release and release['target_commitish'] != commit:
        raise ValueError('Existing release belongs to a different commit')
    if not release:
        release = api.request('POST', '/releases', {
            'tag_name': version, 'target_commitish': commit, 'name': 'cdf2ms ' + version,
            'draft': True, 'prerelease': False,
            'body': f'Automated build {version} from commit `{commit}`.\n\n'
                    'Download the archive for your operating system and CPU. Verify it using SHA256SUMS. '
                    'macOS archives use darwin; amd64 means x64, arm64 includes Apple Silicon.\n\n'
                    'All six targets are cross-compiled with CGO disabled. Native CI checks run on Linux; '
                    'Windows and macOS binaries are unsigned. See README.md in each archive.'})
    assets = api.request('GET', f"/releases/{release['id']}/assets?per_page=100")
    existing = {item['name']: item for item in assets}
    if set(existing) - {p.name for p in files}:
        raise ValueError('Existing release has unexpected assets')
    for path in files:
        payload = path.read_bytes()
        digest = 'sha256:' + hashlib.sha256(payload).hexdigest()
        asset = existing.get(path.name)
        # GitHub can leave a zero-byte starter after an interrupted upload.
        # Only this unfinished draft state is safe to discard on retry.
        if asset and release['draft'] and asset.get('state') == 'starter':
            api.request('DELETE', f"/releases/assets/{asset['id']}")
            asset = None
        if asset:
            if asset.get('digest') != digest or asset.get('size') != len(payload):
                raise ValueError('Refusing to overwrite differing release asset: ' + path.name)
        else:
            if not release['draft']:
                raise ValueError('Published release is incomplete; refusing to mutate it')
            url = release['upload_url'].split('{')[0] + '?name=' + urllib.parse.quote(path.name)
            asset = api.request('POST', url, payload, raw=True)
            if asset.get('digest') != digest or asset.get('size') != len(payload):
                raise ValueError('Uploaded asset verification failed: ' + path.name)
    if release['draft']:
        api.request('PATCH', f"/releases/{release['id']}", {'draft': False, 'make_latest': 'legacy'})
    print(f'https://github.com/{REPO}/releases/tag/{version}')


def validate_checkout(commit, repo=Path('.')):
    head = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip()
    status = subprocess.check_output(['git', 'status', '--porcelain'], cwd=repo, text=True).strip()
    if head != commit or status:
        raise ValueError('Release checkout must be clean and match the pipeline commit')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--artifacts', type=Path, default=Path('dist'))
    args = parser.parse_args()
    version, commit = ci_context(os.environ)
    validate_checkout(commit)
    files = validate_artifacts(args.artifacts, version, commit)
    token = os.environ.get('GITHUB_TOKEN')
    if not token:
        raise ValueError('GITHUB_TOKEN is required')
    publish(GitHub(token), version, commit, files)


if __name__ == '__main__':
    main()
