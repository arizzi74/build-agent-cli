#!/usr/bin/env python3
"""Offline regression tests for release metadata and draft publication safeguards."""

import contextlib
import datetime
import hashlib
import io
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest
import urllib.parse
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parent.parent
PUBLISH_CODE = (ROOT / 'scripts/publish-release.sh').read_text().split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
# Load the publisher's functions without its command-line entry point.
PUBLISH_CODE = PUBLISH_CODE.rsplit('\ntry:\n    main()', 1)[0]
COMMIT = 'a' * 40
VERSION = '2026.09.19.4'


class PublisherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.ns = {}
        exec(compile(PUBLISH_CODE, 'publish-release.sh', 'exec'), self.ns)
        self.dist = pathlib.Path(self.temp.name) / 'dist'
        self.dist.mkdir()
        self.ns['DIST'] = self.dist
        self.operations = []
        self.release = None
        self.latest = None
        self.tag = None
        self.bad_upload = False
        self.ns['run'] = self.run_command
        self.ns['api'] = self.api
        self.manifest = {
            'schemaVersion': 1, 'version': VERSION, 'sourceCommit': COMMIT,
            'sourceDirty': False, 'updateBaseURL': self.ns['LATEST'], 'files': {},
        }
        for target, name in self.ns['FILES'].items():
            raw = ('binary ' + name).encode()
            (self.dist / name).write_bytes(raw)
            self.manifest['files'][target] = {
                'url': self.ns['LATEST'].replace('/latest/download', '/download/v' + VERSION) + '/' + name,
                'sha256': hashlib.sha256(raw).hexdigest(),
            }
        for name in ['install.sh', 'install.ps1']:
            (self.dist / name).write_text('installer ' + name)
        self.save_manifest()
        self.dirty = False
        self.env = mock.patch.dict(os.environ)
        self.env.start()
        self.addCleanup(self.env.stop)
        os.environ.pop('VERSION', None)

    def save_manifest(self):
        (self.dist / 'version.json').write_text(json.dumps(self.manifest))
        self.checksums()

    def checksums(self):
        content = ''.join(self.ns['digest'](self.dist / name) + '  ' + name + '\n'
                          for name in self.ns['ASSETS'] if name != 'SHA256SUMS')
        (self.dist / 'SHA256SUMS').write_text(content)

    def asset(self, name):
        return {'id': self.ns['ASSETS'].index(name) + 1, 'name': name, 'state': 'uploaded',
                'size': (self.dist / name).stat().st_size,
                'digest': 'sha256:' + self.ns['digest'](self.dist / name)}

    def draft(self, assets=()):
        return {'id': 17, 'tag_name': 'v' + VERSION, 'target_commitish': COMMIT,
                'upload_url': 'https://uploads.github.com/' + self.ns['API'] + '/releases/17/assets{?name,label}',
                'draft': True, 'prerelease': False, 'assets': [self.asset(name) for name in assets]}

    def run_command(self, *args):
        if args[:3] == ('git', 'rev-parse', 'HEAD'):
            return COMMIT
        if args[:2] == ('git', 'status'):
            return ' M src/main.go' if self.dirty else ''
        self.fail('Unexpected command: ' + str(args))

    def api(self, endpoint, method='GET', payload=None, missing_ok=False, input_file=None, content_type=None):
        self.operations.append((method, endpoint, payload, input_file))
        base = self.ns['API']
        if endpoint.startswith('https://uploads.github.com/' + base + '/releases/17/assets?'):
            self.assertEqual(method, 'POST')
            self.assertTrue(self.release['draft'])
            name = urllib.parse.parse_qs(urllib.parse.urlsplit(endpoint).query)['name'][0]
            self.assertEqual(input_file, self.dist / name)
            self.assertIsNone(payload)
            asset = self.asset(name)
            if self.bad_upload:
                asset['digest'] = 'sha256:' + '0' * 64
            self.release['assets'].append(asset)
            return asset
        if endpoint == base:
            return {'private': False, 'permissions': {'push': True}}
        if endpoint == base + '/commits/' + COMMIT:
            return {'sha': COMMIT}
        if endpoint == base + '/releases/latest':
            return self.latest
        if '/git/ref/tags/' in endpoint:
            return self.tag
        if endpoint.startswith(base + '/releases?'):
            return [self.release] if self.release else []
        if method == 'POST' and endpoint == base + '/releases':
            self.assertTrue(payload['draft'])
            self.assertEqual(payload['target_commitish'], COMMIT)
            self.release = self.draft()
            return self.release
        if endpoint == base + '/releases/17':
            if method == 'PATCH':
                self.assertEqual(payload, {'draft': False, 'make_latest': 'true'})
                self.assertEqual(len(self.release['assets']), 10)
                self.release['draft'] = False
                self.latest = self.release
            return self.release
        self.fail('Unexpected API request: ' + method + ' ' + endpoint)

    def main(self, check=False):
        with mock.patch('sys.argv', ['publisher', '--check' if check else '']), contextlib.redirect_stdout(io.StringIO()):
            self.ns['main']()

    def test_check_is_offline(self):
        self.main(check=True)
        self.assertEqual(self.operations, [])

    def test_publish_uploads_all_assets_before_switching_latest(self):
        self.main()
        actions = [item[0] for item in self.operations]
        uploads = [index for index, item in enumerate(self.operations) if item[1].startswith('https://uploads.github.com/')]
        self.assertEqual(len(uploads), 10)
        self.assertLess(actions.index('POST'), min(uploads))
        self.assertLess(max(uploads), actions.index('PATCH'))
        self.assertFalse(self.release['draft'])

    def test_draft_resume_only_uploads_missing_identical_assets(self):
        self.release = self.draft(self.ns['ASSETS'][:3])
        self.main()
        self.assertFalse(any(item[:2] == ('POST', self.ns['API'] + '/releases') for item in self.operations))
        uploads = [item for item in self.operations if item[1].startswith('https://uploads.github.com/')]
        self.assertEqual(len(uploads), 7)
        self.assertEqual({item[3].name for item in uploads}, set(self.ns['ASSETS'][3:]))

    def test_dirty_build_rejected_before_network(self):
        self.manifest['sourceDirty'] = True
        self.save_manifest()
        with self.assertRaisesRegex(RuntimeError, 'current clean commit'):
            self.main()
        self.assertEqual(self.operations, [])

    def test_dirty_worktree_rejected_before_network(self):
        self.dirty = True
        with self.assertRaisesRegex(RuntimeError, 'uncommitted'):
            self.main()
        self.assertEqual(self.operations, [])

    def test_source_commit_mismatch(self):
        self.manifest['sourceCommit'] = 'b' * 40
        self.save_manifest()
        with self.assertRaisesRegex(RuntimeError, 'current clean commit'):
            self.main()

    def test_staging_manifest_rejected(self):
        self.manifest['updateBaseURL'] = 'https://staging.example/bacli'
        self.save_manifest()
        with self.assertRaisesRegex(RuntimeError, 'default GitHub'):
            self.main()

    def test_moving_latest_artifact_rejected(self):
        name = self.ns['FILES']['linux-arm64']
        self.manifest['files']['linux-arm64']['url'] = self.ns['LATEST'] + '/' + name
        self.save_manifest()
        with self.assertRaisesRegex(RuntimeError, 'immutable release'):
            self.main()

    def test_stale_checksum_rejected(self):
        (self.dist / 'install.sh').write_text('modified installer')
        with self.assertRaisesRegex(RuntimeError, 'SHA256SUMS'):
            self.main()

    def test_duplicate_checksum_rejected(self):
        path = self.dist / 'SHA256SUMS'
        path.write_text(path.read_text() + path.read_text().splitlines()[0] + '\n')
        with self.assertRaisesRegex(RuntimeError, 'duplicate SHA256SUMS'):
            self.main()

    def test_existing_published_release_not_modified(self):
        self.release = self.draft()
        self.release['draft'] = False
        with self.assertRaisesRegex(RuntimeError, 'published'):
            self.main()
        self.assertNotIn('PATCH', [item[0] for item in self.operations])

    def test_newer_latest_not_replaced(self):
        self.latest = {'tag_name': 'v2026.09.19.5'}
        with self.assertRaisesRegex(RuntimeError, 'newer latest'):
            self.main()

    def test_existing_tag_must_match_commit(self):
        self.tag = {'object': {'type': 'commit', 'sha': 'b' * 40}}
        with self.assertRaisesRegex(RuntimeError, 'another commit'):
            self.main()

    def test_existing_draft_asset_mismatch_not_overwritten(self):
        self.release = self.draft(self.ns['ASSETS'][:1])
        self.release['assets'][0]['digest'] = 'sha256:' + '0' * 64
        with self.assertRaisesRegex(RuntimeError, 'differs from local'):
            self.main()
        self.assertFalse(any(item[1].startswith('https://uploads.github.com/') for item in self.operations))

    def test_upload_mismatch_leaves_release_unpublished(self):
        self.bad_upload = True
        with self.assertRaisesRegex(RuntimeError, 'differs from local'):
            self.main()
        self.assertTrue(self.release['draft'])
        self.assertNotIn('PATCH', [item[0] for item in self.operations])

    def test_api_uses_explicit_host_and_json_boolean_and_latest_string(self):
        namespace = {}
        exec(compile(PUBLISH_CODE, 'publish-release.sh', 'exec'), namespace)
        response = subprocess.CompletedProcess([], 0, 'HTTP/2.0 200 OK\nContent-Type: application/json\n\n{"id": 17}', '')
        with mock.patch('subprocess.run', return_value=response) as request:
            result = namespace['api']('repos/arizzi74/build-agent-cli/releases/17', 'PATCH', {'draft': False, 'make_latest': 'true'})
        args, kwargs = request.call_args
        self.assertIn('github.com', args[0])
        self.assertEqual(json.loads(kwargs['input']), {'draft': False, 'make_latest': 'true'})
        self.assertEqual(result, {'id': 17})

    def test_binary_upload_uses_numeric_draft_id_and_file_body(self):
        namespace = {}
        exec(compile(PUBLISH_CODE, 'publish-release.sh', 'exec'), namespace)
        namespace['DIST'] = self.dist
        name = 'bacli-linux-arm64'
        asset = self.asset(name)
        response = subprocess.CompletedProcess([], 0, 'HTTP/2.0 201 Created\nContent-Type: application/json\n\n' + json.dumps(asset), '')
        with mock.patch('subprocess.run', return_value=response) as request:
            namespace['upload_asset'](self.draft(), name, {name: self.ns['digest'](self.dist / name)})
        args, kwargs = request.call_args
        command = args[0]
        self.assertEqual(command[:2], ['gh', 'api'])
        self.assertEqual(command[command.index('--hostname') + 1], 'github.com')
        self.assertEqual(command[command.index('--method') + 1], 'POST')
        self.assertIn('https://uploads.github.com/repos/arizzi74/build-agent-cli/releases/17/assets?name=' + name, command)
        self.assertEqual(command[command.index('--input') + 1], str(self.dist / name))
        self.assertIn('Content-Type: application/octet-stream', command)
        self.assertIn('Content-Length: ' + str((self.dist / name).stat().st_size), command)
        self.assertIsNone(kwargs['input'])
        self.assertNotIn('release', command)

    def test_upload_url_must_match_expected_github_draft(self):
        draft = self.draft()
        draft['upload_url'] = 'https://other.example/upload{?name,label}'
        with self.assertRaisesRegex(RuntimeError, 'Unexpected GitHub draft upload URL'):
            self.ns['upload_asset'](draft, 'install.sh', {})
        self.assertEqual(self.operations, [])


class BuildTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = pathlib.Path(self.temp.name) / 'repo'
        self.repo.mkdir()
        (self.repo / 'scripts').mkdir()
        (self.repo / 'dist').mkdir()
        shutil.copy2(ROOT / 'scripts/build-release.sh', self.repo / 'scripts/build-release.sh')
        for name in ['install.sh', 'install.ps1']:
            (self.repo / 'dist' / name).write_text('installer ' + name)
        (self.repo / 'dist/install.sh').chmod(0o755)
        self.fake_go = pathlib.Path(self.temp.name) / 'go'
        self.fake_go.write_text('#!/usr/bin/env python3\nimport pathlib,sys\npathlib.Path(sys.argv[sys.argv.index("-o")+1]).write_bytes(" ".join(sys.argv).encode())\n')
        self.fake_go.chmod(0o755)
        self.command('git', 'init', '-q')
        self.command('git', 'add', '.')
        self.command('git', '-c', 'user.name=Release Tests', '-c', 'user.email=tests@example.invalid', 'commit', '-qm', 'Fixture')

    def command(self, *args, env=None):
        return subprocess.run(args, cwd=self.repo, env=env, capture_output=True, text=True, check=True)

    def build(self, version=VERSION, base=None):
        env = os.environ.copy()
        env['GO'] = str(self.fake_go)
        for name in ['VERSION', 'BASE_URL']:
            env.pop(name, None)
        if version:
            env['VERSION'] = version
        if base:
            env['BASE_URL'] = base
        self.command('sh', 'scripts/build-release.sh', env=env)
        return json.loads((self.repo / 'dist/version.json').read_text())

    def test_default_github_release_uses_immutable_artifacts(self):
        manifest = self.build()
        self.assertFalse(manifest['sourceDirty'])
        self.assertEqual(manifest['sourceCommit'], self.command('git', 'rev-parse', 'HEAD').stdout.strip())
        self.assertTrue(manifest['updateBaseURL'].endswith('/releases/latest/download'))
        self.assertEqual(len(manifest['files']), 6)
        for artifact in manifest['files'].values():
            self.assertIn('/releases/download/v' + VERSION + '/', artifact['url'])
        subprocess.run(['sha256sum', '-c', 'SHA256SUMS'], cwd=self.repo / 'dist', check=True, capture_output=True)

    def test_staging_override_keeps_relative_artifacts_and_embedded_root(self):
        manifest = self.build(base='https://staging.example/bacli/')
        self.assertEqual(manifest['updateBaseURL'], 'https://staging.example/bacli')
        for artifact in manifest['files'].values():
            self.assertNotIn('/', artifact['url'])
        binary = (self.repo / 'dist/bacli-linux-arm64').read_text()
        self.assertIn('cliUpdateBaseURL=https://staging.example/bacli', binary)

    def test_dirty_source_is_recorded(self):
        (self.repo / 'dist/install.sh').write_text('changed')
        self.assertTrue(self.build()['sourceDirty'])

    def test_automatic_version_advances_largest_local_daily_number(self):
        today = datetime.datetime.now(datetime.timezone.utc).strftime('%Y.%m.%d')
        self.build(version=today + '.4')
        self.command('git', 'tag', 'v' + today + '.8')
        self.assertEqual(self.build(version=None)['version'], today + '.9')

    def test_non_numbered_version_rejected_before_build(self):
        with self.assertRaises(subprocess.CalledProcessError):
            self.build(version='2026.09.19.deadbeef')
        self.assertFalse((self.repo / 'dist/bacli-linux-arm64').exists())


if __name__ == '__main__':
    unittest.main(verbosity=2)
