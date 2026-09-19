#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

GITHUB_LATEST=https://github.com/arizzi74/build-agent-cli/releases/latest/download
BASE_URL=${BASE_URL:-$GITHUB_LATEST}
BASE_URL=${BASE_URL%/}
DIST=dist
# Number releases from the UTC date and the highest locally known daily number.
# The publisher also checks GitHub, so a stale checkout cannot replace a newer release.
VERSION=${VERSION:-$(python3 - "$DIST/version.json" <<'PY'
import datetime, json, pathlib, re, subprocess, sys
today = datetime.datetime.now(datetime.timezone.utc).strftime('%Y.%m.%d')
versions = subprocess.check_output(['git', 'tag', '--list', 'v' + today + '.*'], text=True).splitlines()
manifest = pathlib.Path(sys.argv[1])
if manifest.exists():
    versions.append(json.loads(manifest.read_text(encoding='utf-8')).get('version', ''))
numbers = [int(match.group(1)) for version in versions
           if (match := re.fullmatch(r'v?' + re.escape(today) + r'\.([1-9][0-9]*)', version))]
print(today + '.' + str(max(numbers, default=0) + 1))
PY
)}
python3 - "$VERSION" <<'PY'
import datetime, re, sys
version = sys.argv[1]
if not re.fullmatch(r'\d{4}\.\d{2}\.\d{2}\.[1-9][0-9]*', version):
    sys.exit('VERSION must use YYYY.MM.DD.N (UTC date and a positive daily number)')
datetime.datetime.strptime(version.rsplit('.', 1)[0], '%Y.%m.%d')
PY
SOURCE_COMMIT=$(git rev-parse HEAD)
SOURCE_STATE=$(git status --porcelain --untracked-files=all -- go.mod go.sum src scripts dist/install.sh dist/install.ps1 README.md BUILD.md SPECS.md)
if [ -x /usr/local/go/bin/go ]; then
  GO=${GO:-/usr/local/go/bin/go}
else
  GO=${GO:-go}
fi
LDFLAGS="-s -w -buildid= -X build-agent-go-cli/src/core.cliVersion=$VERSION -X build-agent-go-cli/src/core.cliUpdateBaseURL=$BASE_URL"

mkdir -p "$DIST"
rm -f "$DIST"/bacli-linux-arm64 "$DIST"/bacli-linux-amd64 "$DIST"/bacli-darwin-amd64 "$DIST"/bacli-darwin-arm64 "$DIST"/bacli-windows-amd64.exe "$DIST"/bacli-windows-arm64.exe "$DIST"/version.json

build() {
  goos=$1
  goarch=$2
  output=$3
  printf 'Building %s/%s -> %s\n' "$goos" "$goarch" "$output"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$GO" build -trimpath -ldflags="$LDFLAGS" -o "$DIST/$output" ./src/cmd/bacli
}

build linux arm64 bacli-linux-arm64
build linux amd64 bacli-linux-amd64
build darwin amd64 bacli-darwin-amd64
build darwin arm64 bacli-darwin-arm64
build windows amd64 bacli-windows-amd64.exe
build windows arm64 bacli-windows-arm64.exe
chmod 755 "$DIST/install.sh" "$DIST/bacli-linux-arm64" "$DIST/bacli-linux-amd64" "$DIST/bacli-darwin-amd64" "$DIST/bacli-darwin-arm64"
chmod 644 "$DIST/install.ps1" "$DIST/bacli-windows-amd64.exe" "$DIST/bacli-windows-arm64.exe"

SOURCE_DIRTY=false
if [ -n "$SOURCE_STATE" ] || [ "$SOURCE_COMMIT" != "$(git rev-parse HEAD)" ] || [ -n "$(git status --porcelain --untracked-files=all -- go.mod go.sum src scripts dist/install.sh dist/install.ps1 README.md BUILD.md SPECS.md)" ]; then
  SOURCE_DIRTY=true
fi

python3 - "$VERSION" "$BASE_URL" "$DIST" "$GITHUB_LATEST" "$SOURCE_COMMIT" "$SOURCE_DIRTY" <<'PY'
import hashlib, json, pathlib, sys
version, base, dist, github_latest, commit, dirty = sys.argv[1:]
d = pathlib.Path(dist)
files = {
    'linux-arm64': 'bacli-linux-arm64',
    'linux-amd64': 'bacli-linux-amd64',
    'darwin-amd64': 'bacli-darwin-amd64',
    'darwin-arm64': 'bacli-darwin-arm64',
    'windows-amd64': 'bacli-windows-amd64.exe',
    'windows-arm64': 'bacli-windows-arm64.exe',
}
out = {'schemaVersion': 1, 'version': version, 'sourceCommit': commit,
       'sourceDirty': dirty == 'true', 'updateBaseURL': base, 'files': {}}
for target, name in files.items():
    raw = (d / name).read_bytes()
    # Pin each binary to this release even if "latest" changes after manifest fetch.
    url = github_latest.replace('/latest/download', '/download/v' + version) + '/' + name if base == github_latest else name
    out['files'][target] = {'url': url, 'sha256': hashlib.sha256(raw).hexdigest()}
(d / 'version.json').write_text(json.dumps(out, indent=2) + '\n', encoding='utf-8')
PY

printf 'Release %s built in %s/\n' "$VERSION" "$DIST"
(
  cd "$DIST"
  sha256sum bacli-linux-arm64 bacli-linux-amd64 bacli-darwin-amd64 bacli-darwin-arm64 bacli-windows-amd64.exe bacli-windows-arm64.exe install.sh install.ps1 version.json > SHA256SUMS
)
chmod 644 "$DIST/version.json" "$DIST/SHA256SUMS"
if [ "$SOURCE_DIRTY" = true ]; then
  printf '%s\n' 'Local test build: relevant files are uncommitted. Commit, rebuild, and push before publishing.'
fi
