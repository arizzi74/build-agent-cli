#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

case ${1:-} in
  '') ;;
  --check) ;;
  *) printf 'Usage: %s [--check]\n--check validates local release files without accessing GitHub.\n' "$0" >&2; exit 2 ;;
esac
if [ "$#" -gt 1 ]; then
  printf 'Only --check is supported.\n' >&2
  exit 2
fi

# gh api works with older GitHub CLI versions that have no "release edit" command.
# Authentication is handled by gh; this script never reads or prints credentials.
python3 - "${1:-}" <<'PY'
import datetime
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys

REPO = 'arizzi74/build-agent-cli'
API = 'repos/' + REPO
LATEST = 'https://github.com/' + REPO + '/releases/latest/download'
DIST = pathlib.Path('dist')
FILES = {
    'linux-arm64': 'bacli-linux-arm64',
    'linux-amd64': 'bacli-linux-amd64',
    'darwin-amd64': 'bacli-darwin-amd64',
    'darwin-arm64': 'bacli-darwin-arm64',
    'windows-amd64': 'bacli-windows-amd64.exe',
    'windows-arm64': 'bacli-windows-arm64.exe',
}
ASSETS = list(FILES.values()) + ['install.sh', 'install.ps1', 'version.json', 'SHA256SUMS']
SOURCE_PATHS = ['go.mod', 'go.sum', 'src', 'scripts', 'dist/install.sh', 'dist/install.ps1',
                'README.md', 'BUILD.md', 'SPECS.md']


def fail(message):
    raise RuntimeError(message)


def run(*args):
    result = subprocess.run(args, check=False, capture_output=True, text=True)
    if result.returncode:
        fail('Command failed: ' + ' '.join(args[:3]) + '\n' + result.stderr.strip())
    return result.stdout.strip()


def digest(path):
    hasher = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            hasher.update(chunk)
    return hasher.hexdigest()


def version_tuple(value):
    value = value[1:] if value.startswith('v') else value
    if not re.fullmatch(r'\d{4}\.\d{2}\.\d{2}\.[1-9][0-9]*', value):
        fail('Release version must use YYYY.MM.DD.N: ' + value)
    datetime.datetime.strptime(value.rsplit('.', 1)[0], '%Y.%m.%d')
    return tuple(int(part) for part in value.split('.'))


def validate_local():
    manifest = json.loads((DIST / 'version.json').read_text(encoding='utf-8'))
    version = manifest['version']
    version_tuple(version)
    if os.environ.get('VERSION', version) != version:
        fail('VERSION does not match dist/version.json; rebuild first')
    if manifest.get('schemaVersion') != 1 or set(manifest.get('files', {})) != set(FILES):
        fail('Manifest must contain schema version 1 and exactly the six supported platforms')
    if manifest.get('updateBaseURL') != LATEST:
        fail('Only a build using the default GitHub update URL can be published')
    commit = run('git', 'rev-parse', 'HEAD')
    if manifest.get('sourceCommit') != commit or manifest.get('sourceDirty') is not False:
        fail('Release was not built from the current clean commit. Commit changes, then rebuild')
    if run('git', 'status', '--porcelain', '--untracked-files=all', '--', *SOURCE_PATHS):
        fail('Relevant source, scripts, installers, or documentation are uncommitted; commit and rebuild')
    hashes = {}
    for name in ASSETS:
        path = DIST / name
        if not path.is_file() or path.is_symlink() or path.stat().st_size == 0:
            fail('Missing, empty, or symlinked release asset: ' + name)
        hashes[name] = digest(path)
    checksums = {}
    for line in (DIST / 'SHA256SUMS').read_text(encoding='utf-8').splitlines():
        match = re.fullmatch(r'([0-9a-f]{64}) [ *]([^/\\]+)', line)
        if not match or match[2] in checksums:
            fail('Invalid or duplicate SHA256SUMS entry')
        checksums[match[2]] = match[1]
    if checksums != {name: hashes[name] for name in ASSETS if name != 'SHA256SUMS'}:
        fail('SHA256SUMS must match exactly the six binaries, two installers, and manifest')
    release_base = LATEST.replace('/latest/download', '/download/v' + version)
    for target, name in FILES.items():
        artifact = manifest['files'][target]
        if artifact.get('url') != release_base + '/' + name or artifact.get('sha256') != hashes[name]:
            fail('Manifest URL/checksum does not match this immutable release: ' + name)
    return version, commit, hashes


def api(endpoint, method='GET', payload=None, missing_ok=False):
    command = ['gh', 'api', '--hostname', 'github.com', '--include', '--method', method,
               '-H', 'Accept: application/vnd.github+json', '-H', 'X-GitHub-Api-Version: 2022-11-28', endpoint]
    if payload is not None:
        command += ['--input', '-']
    result = subprocess.run(command, input=json.dumps(payload) if payload is not None else None,
                            capture_output=True, text=True, check=False)
    headers, separator, body = result.stdout.partition('\n\n')
    status = re.search(r'^HTTP/\S+\s+(\d+)', headers)
    if not separator or not status:
        fail('GitHub API returned no HTTP response for ' + method + ' ' + endpoint + '\n' + result.stderr.strip())
    code = int(status[1])
    if missing_ok and code == 404:
        return None
    if result.returncode or not 200 <= code < 300:
        fail('GitHub API failed: ' + method + ' ' + endpoint + ' (HTTP ' + str(code) + ')')
    return json.loads(body) if body.strip() else None


def find_release(tag):
    # Listing includes drafts for authenticated users with repository push access.
    page = 1
    while True:
        releases = api(API + '/releases?per_page=100&page=' + str(page))
        for release in releases:
            if release['tag_name'] == tag:
                return release
        if len(releases) < 100:
            return None
        page += 1


def verify_tag(tag, commit):
    ref = api(API + '/git/ref/tags/' + tag, missing_ok=True)
    if ref is None:
        return
    obj = ref['object']
    for _ in range(10):
        if obj['type'] == 'commit':
            if obj['sha'] != commit:
                fail('Existing release tag points to another commit; use a new version')
            return
        if obj['type'] != 'tag':
            break
        obj = api(API + '/git/tags/' + obj['sha'])['object']
    fail('Cannot resolve the existing release tag to a commit')


def verify_asset(asset, hashes):
    name = asset['name']
    if name not in hashes or asset.get('state') != 'uploaded' or asset.get('size') != (DIST / name).stat().st_size:
        fail('Unexpected or incomplete remote asset: ' + name)
    expected = 'sha256:' + hashes[name]
    if asset.get('digest'):
        if asset['digest'] != expected:
            fail('Remote asset differs from local build: ' + name + '; use a new version or repair the draft manually')
        return
    # Older assets may lack GitHub's digest field; verify their actual bytes instead.
    command = ['gh', 'api', '--hostname', 'github.com', API + '/releases/assets/' + str(asset['id']),
               '-H', 'Accept: application/octet-stream']
    hasher = hashlib.sha256()
    with subprocess.Popen(command, stdout=subprocess.PIPE) as process:
        for chunk in iter(lambda: process.stdout.read(1024 * 1024), b''):
            hasher.update(chunk)
        if process.wait() or hasher.hexdigest() != hashes[name]:
            fail('Remote asset checksum verification failed: ' + name)


def verify_draft(release, tag, commit, hashes, complete=False):
    if release.get('tag_name') != tag or release.get('draft') is not True or release.get('prerelease'):
        fail('Refusing to change a published, prerelease, or mismatched release')
    if release.get('target_commitish') != commit:
        fail('Existing draft targets another commit; use a new version or repair the draft manually')
    names = [asset['name'] for asset in release['assets']]
    if len(names) != len(set(names)) or not set(names).issubset(ASSETS):
        fail('Draft contains unexpected or duplicate assets; inspect it manually')
    if complete and set(names) != set(ASSETS):
        fail('Draft is incomplete; it will not be published')
    for asset in release['assets']:
        verify_asset(asset, hashes)
    return set(names)


def main():
    version, commit, hashes = validate_local()
    tag = 'v' + version
    print('Validated all ten local assets for ' + tag + ' at ' + commit, flush=True)
    if sys.argv[1] == '--check':
        return
    repo = api(API)
    if repo.get('private') is not False or repo.get('permissions', {}).get('push') is not True:
        fail('Publishing requires push access to the public repository ' + REPO)
    remote_commit = api(API + '/commits/' + commit, missing_ok=True)
    if remote_commit is None or remote_commit.get('sha') != commit:
        fail('The build commit is not on GitHub. Push the source commit before publishing')
    latest = api(API + '/releases/latest', missing_ok=True)
    if latest and version_tuple(latest['tag_name']) >= version_tuple(version):
        fail('GitHub already has this or a newer latest release; increment the version and rebuild')
    verify_tag(tag, commit)
    release = find_release(tag)
    if release is not None:
        present = verify_draft(release, tag, commit, hashes)
    else:
        print('Creating unpublished draft ' + tag, flush=True)
        release = api(API + '/releases', 'POST', {
            'tag_name': tag, 'target_commitish': commit, 'name': 'Build Agent CLI ' + version,
            'body': 'Cross-platform installers and binaries. Source commit: ' + commit + '.',
            'draft': True, 'prerelease': False,
        })
        present = verify_draft(release, tag, commit, hashes)
    missing = [str(DIST / name) for name in ASSETS if name not in present]
    if missing:
        print('Uploading ' + str(len(missing)) + ' assets to the draft', flush=True)
        run('gh', 'release', 'upload', tag, *missing, '--repo', 'github.com/' + REPO)
    release = api(API + '/releases/' + str(release['id']))
    verify_draft(release, tag, commit, hashes, complete=True)
    verify_tag(tag, commit)
    # Recheck the local build and current latest immediately before the public switch.
    if validate_local() != (version, commit, hashes):
        fail('Local release changed during upload; draft will not be published')
    latest = api(API + '/releases/latest', missing_ok=True)
    if latest and version_tuple(latest['tag_name']) >= version_tuple(version):
        fail('Another release became latest during upload; draft will not be published')
    print('All ten remote assets verified; publishing ' + tag + ' as latest', flush=True)
    published = api(API + '/releases/' + str(release['id']), 'PATCH', {'draft': False, 'make_latest': 'true'})
    if published.get('draft') is not False or published.get('tag_name') != tag:
        fail('Unexpected publish response; inspect the release on GitHub before retrying')
    latest = api(API + '/releases/latest')
    if latest.get('id') != release['id']:
        fail('Release was published but is not latest; inspect GitHub before retrying')
    print('Published https://github.com/' + REPO + '/releases/tag/' + tag)


try:
    main()
except (RuntimeError, OSError, ValueError, KeyError, TypeError) as error:
    print('Release not completed: ' + str(error), file=sys.stderr)
    sys.exit(1)
PY
