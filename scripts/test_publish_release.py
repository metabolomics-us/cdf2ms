import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import publish_release as release


class ReleaseTests(unittest.TestCase):
    def test_only_default_branch_push_can_publish(self):
        base = {'CI_PIPELINE_EVENT': 'push', 'CI_COMMIT_BRANCH': 'main',
                'CI_REPO_DEFAULT_BRANCH': 'main', 'CI_PIPELINE_NUMBER': '12',
                'CI_COMMIT_SHA': 'a' * 40, 'CI_REPO': 'metabolomics-us/cdf2ms'}
        self.assertEqual(release.ci_context(base), ('v0.1.12', 'a' * 40))
        for override in [{'CI_PIPELINE_EVENT': 'pull_request'},
                         {'CI_PIPELINE_EVENT': 'manual'},
                         {'CI_COMMIT_BRANCH': 'feature'},
                         {'CI_PIPELINE_NUMBER': '12;echo bad'},
                         {'CI_REPO': 'someone/fork'}]:
            with self.subTest(override=override), self.assertRaises(ValueError):
                release.ci_context(dict(base, **override))
        master = dict(base, CI_COMMIT_BRANCH='master', CI_REPO_DEFAULT_BRANCH='master')
        self.assertEqual(release.ci_context(master)[0], 'v0.1.12')

    def test_corrupt_or_missing_archive_prevents_publication(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            assets = []
            for target in ('linux_amd64', 'linux_arm64', 'darwin_amd64', 'darwin_arm64', 'windows_amd64', 'windows_arm64'):
                ext = '.zip' if target.startswith('windows') else '.tar.gz'
                name = 'cdf2ms_v0.1.12_' + target + ext
                (root / name).write_bytes(b'archive')
                assets.append((name, hashlib.sha256(b'archive').hexdigest()))
            (root / 'SHA256SUMS').write_text(''.join(f'{sha}  {name}\n' for name, sha in assets))
            (root / 'release.json').write_text(json.dumps({'version': 'v0.1.12', 'commit': 'a' * 40}))
            self.assertEqual(len(release.validate_artifacts(root, 'v0.1.12', 'a' * 40)), 8)
            (root / assets[0][0]).write_bytes(b'corrupt')
            with self.assertRaises(ValueError):
                release.validate_artifacts(root, 'v0.1.12', 'a' * 40)

    def test_wrong_commit_manifest_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'release.json').write_text(json.dumps({'version': 'v0.1.12', 'commit': 'b' * 40}))
            with self.assertRaises(ValueError):
                release.validate_artifacts(root, 'v0.1.12', 'a' * 40)

    def test_upload_failure_keeps_release_draft(self):
        api = Mock()
        api.request.side_effect = [None, None,
            {'id': 1, 'draft': True, 'upload_url': 'https://uploads.github.com/repos/x/y/releases/1/assets{?name}'},
            [], RuntimeError('upload interrupted')]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'artifact.zip'
            path.write_bytes(b'download')
            with self.assertRaises(RuntimeError):
                release.publish(api, 'v0.1.12', 'a' * 40, [path])
        self.assertFalse(any(call.args[0] == 'PATCH' for call in api.request.call_args_list))

    def test_retry_accepts_identical_published_assets_without_mutation(self):
        api = Mock()
        api.request.side_effect = [
            {'object': {'type': 'commit', 'sha': 'a' * 40}},
            {'id': 1, 'draft': False, 'target_commitish': 'a' * 40},
            [{'name': 'artifact.zip', 'size': 8,
              'digest': 'sha256:' + hashlib.sha256(b'download').hexdigest()}]]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'artifact.zip'
            path.write_bytes(b'download')
            release.publish(api, 'v0.1.12', 'a' * 40, [path])
        self.assertTrue(all(call.args[0] == 'GET' for call in api.request.call_args_list))

    def test_existing_tag_never_moves_to_another_commit(self):
        api = Mock()
        api.request.return_value = {'object': {'type': 'commit', 'sha': 'b' * 40}}
        with self.assertRaises(ValueError):
            release.publish(api, 'v0.1.12', 'a' * 40, [])
        self.assertEqual(api.request.call_count, 1)

    def test_retry_removes_only_incomplete_draft_upload(self):
        api = Mock()
        digest = 'sha256:' + hashlib.sha256(b'download').hexdigest()
        api.request.side_effect = [None,
            {'id': 1, 'draft': True, 'target_commitish': 'a' * 40,
             'upload_url': 'https://uploads.github.com/repos/x/y/releases/1/assets{?name}'},
            [{'id': 9, 'name': 'artifact.zip', 'state': 'starter', 'size': 0, 'digest': None}],
            None, {'size': 8, 'digest': digest}, {}]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'artifact.zip'
            path.write_bytes(b'download')
            release.publish(api, 'v0.1.12', 'a' * 40, [path])
        calls = api.request.call_args_list
        self.assertEqual(calls[3].args, ('DELETE', '/releases/assets/9'))
        self.assertEqual(calls[-1].args[0], 'PATCH')


if __name__ == '__main__':
    unittest.main()
