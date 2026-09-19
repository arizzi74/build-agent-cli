# Build and release

Build Agent CLI releases are cross-compiled as six pure-Go, stripped binaries. All public release files are assembled under `./dist` and published as GitHub Release assets. The installed executable is always named `bacli` (`bacli.exe` on Windows).

## Supported targets

| Target | Release file |
| --- | --- |
| Linux aarch64 / arm64 | `dist/bacli-linux-arm64` |
| Linux Intel/AMD / amd64 | `dist/bacli-linux-amd64` |
| macOS Intel / amd64 | `dist/bacli-darwin-amd64` |
| macOS Apple Silicon / arm64 | `dist/bacli-darwin-arm64` |
| Windows amd64 / x86-64 | `dist/bacli-windows-amd64.exe` |
| Windows ARM64 | `dist/bacli-windows-arm64.exe` |

Every build uses:

- `CGO_ENABLED=0` for a pure-Go binary with no C runtime dependency.
- `-trimpath` to remove local source paths.
- `-ldflags='-s -w -buildid= ...'` to strip symbols/debug metadata and omit the Go build ID.
- Embedded `build-agent-go-cli/src/core.cliVersion` and `build-agent-go-cli/src/core.cliUpdateBaseURL` values for startup updates.

The macOS clipboard integration uses only `/usr/bin/osascript` and `/usr/bin/sips` at runtime, and the Telegram channel uses the Go standard library. Neither feature introduces CGO, Node.js, npm, or a project-supplied dynamic library into the bacli executable. Node/npm remain optional local application-build prerequisites, not bacli runtime dependencies.

The executable package is `./src/cmd/bacli`; its application code and co-located tests are in `./src/core`. The release script builds that command package directly.

Linux is statically linked in the normal ELF sense. Go's `CGO_ENABLED=0` macOS and Windows outputs contain no project-supplied dynamic libraries or CGO runtime, although platform inspection tools may still describe normal operating-system loader/framework imports.

## Release build

Numbered releases use `YYYY.MM.DD.N` based on the UTC date. Start `N` at `1` on each new UTC date and increment it for every additional release on that date.

Run from the repository root:

```bash
VERSION=2026.09.19.4 ./scripts/build-release.sh
```

If `VERSION` is omitted, the script uses today's UTC date and increments the highest daily number found in local release tags or `dist/version.json` (starting at `1`). Fetch release tags before building on another machine; the publisher also refuses a version at or below GitHub's current latest release. Override the publication root only when staging:

```bash
VERSION=2026.09.19.4 BASE_URL=https://staging.example/bacli ./scripts/build-release.sh
```

The script creates:

```text
dist/
├── bacli-linux-arm64
├── bacli-linux-amd64
├── bacli-darwin-amd64
├── bacli-darwin-arm64
├── bacli-windows-amd64.exe
├── bacli-windows-arm64.exe
├── install.sh
├── install.ps1
├── version.json
└── SHA256SUMS
```

Do not build or publish dynamic/non-stripped binaries for normal releases. A non-stripped binary is allowed only as a temporary debugging exception when explicitly requested.

## Publication layout

GitHub Releases is the only production distribution host. The stable latest-release base is:

```text
https://github.com/arizzi74/build-agent-cli/releases/latest/download
```

The following assets are available below that base:

```text
install.sh
install.ps1
version.json
SHA256SUMS
bacli-linux-arm64
bacli-linux-amd64
bacli-darwin-amd64
bacli-darwin-arm64
bacli-windows-amd64.exe
bacli-windows-arm64.exe
```

`version.json` is the source of truth for installers and self-update. Production manifests use version-pinned binary URLs such as `https://github.com/arizzi74/build-agent-cli/releases/download/v2026.09.19.4/bacli-linux-arm64`, never moving latest links. This avoids checksum races if a newer release is published between the manifest and binary downloads. SHA-256 checks provide integrity, not a separate digital signature.

Building requires Go, Python 3.9 or newer, and `sha256sum`. Publication additionally requires authenticated GitHub CLI access with permission to create releases (`gh auth login --hostname github.com`). The publisher uses `gh api` with direct draft-ID asset uploads, including compatibility with GitHub CLI 2.4; it does not rely on finding unpublished drafts by tag. Git-over-SSH authentication alone does not authenticate the GitHub Releases API. Installers and updates are public and need neither a token nor `gh`.

Commit the intended source, documentation, scripts, and installer changes, then build from that clean commit and push it before publishing:

```bash
VERSION=2026.09.19.4 ./scripts/build-release.sh
git push origin master
./scripts/publish-release.sh --check
./scripts/publish-release.sh
```

The publisher validates local checksums, all six targets, version-pinned URLs, and build provenance (`sourceCommit`, `sourceDirty`, and `updateBaseURL` in the manifest) against the clean source commit. `--check` performs only local validation. Publication checks that the source commit exists on GitHub, creates a draft for tag `v<version>`, uploads and verifies all ten assets, then publishes the complete release as latest. It refuses to overwrite an already published release; corrections require a new version. An interrupted draft can be resumed safely. No separate web server, rsync transfer, or mirror cron job participates in publication. The retired mirror script is archived under `purge/scripts/`; remove its old cron entry on any legacy mirror host when retiring that infrastructure.

Older installed binaries cannot discover a changed embedded update URL automatically. Close bacli and rerun the GitHub installer once to migrate; configuration and app projects are preserved. Remove any old `BACLI_BASE_URL` or `BACLI_UPDATE_BASE_URL` override. The new release has no fallback to the previous distribution host.

## Installer behavior

- Unix installer: detects Linux arm64, Linux amd64, macOS Intel, or macOS Apple Silicon; verifies SHA-256; installs as `~/.local/bin/bacli`; and adds an idempotent PATH line to `.zshrc`, `.bashrc`, or `.profile` when needed.
- Windows installer: detects Windows amd64 or Windows ARM64, including Windows PowerShell 5.1/WOW64 hosts; verifies SHA-256; installs as `%USERPROFILE%\.local\bin\bacli.exe`; and safely creates or extends the user PATH when needed.

Environment overrides for staging/testing:

```text
BACLI_BASE_URL
BACLI_INSTALL_DIR
BACLI_UPDATE_BASE_URL
BACLI_NO_UPDATE=1
```

## Startup update protocol

Every versioned `bacli` startup requests `https://github.com/arizzi74/build-agent-cli/releases/latest/download/version.json` with a five-second timeout. Failures are non-fatal so an offline user can continue working.

When the manifest version is newer, the CLI:

1. Selects its current OS/architecture artifact.
2. Downloads its version-pinned URL with a 128 MiB bound and a separate two-minute timeout, allowing time for GitHub CDN redirects and transfers.
3. Verifies the manifest SHA-256.
4. Replaces the current Unix executable atomically, or stages a Windows replacement helper because a running `.exe` cannot overwrite itself.
5. Exits with a message asking the user to relaunch `bacli`.

Development binaries with embedded version `dev` skip the update request. `BACLI_NO_UPDATE=1` disables it explicitly.

## Verification

Before release:

```bash
go test -race -count=1 ./...
go test -count=1 ./...
go vet ./...
python3 scripts/test-release.py
git diff --check
VERSION=2026.09.19.4 ./scripts/build-release.sh
```

Inspect artifacts:

```bash
file dist/bacli-*
ldd dist/bacli-linux-arm64 || true
ldd dist/bacli-linux-amd64 || true
(cd dist && sha256sum -c SHA256SUMS)
ls -lh dist/
```

Expected Linux output includes `ELF 64-bit`, `ARM aarch64`, `statically linked`, and `stripped`; `ldd` reports that it is not a dynamic executable. Darwin outputs are Mach-O for their requested architectures. Windows outputs are PE32+ for x86-64 and Aarch64 respectively.
