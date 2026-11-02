# Installing LocalBase

LocalBase is one binary with the UI built in. It downloads model runtimes and models when you ask for them, into its data directory (`~/.local/share/local-base` by default).

## Install script

The script verifies the archive against the release's `checksums.txt`, never uses sudo and installs to `~/.local/bin`. Read it before you run it:

```sh
curl -fsSLO https://github.com/timsexperiments/LocalBase/releases/latest/download/install.sh
less install.sh
sh install.sh
```

| Variable | Default | Meaning |
| --- | --- | --- |
| `LOCALBASE_VERSION` | `latest` | Release to install, such as `0.2.0` |
| `LOCALBASE_INSTALL_DIR` | `~/.local/bin` | Where the binary goes |
| `LOCALBASE_LICENSE_DIR` | `~/.local/share/local-base/licenses` | Where the license texts go |
| `LOCALBASE_RELEASE_URL` | GitHub releases | Download from this base URL instead |

## Manual install and verification

Download an archive and `checksums.txt` from the [releases page](https://github.com/timsexperiments/LocalBase/releases).

| Platform | Archive |
| --- | --- |
| macOS, Apple Silicon | `local-base-macos-arm64.zip` |
| macOS, Intel | `local-base-macos-x64.zip` |
| Linux, x86-64 | `local-base-linux-x64.tar.gz` |
| Linux, ARM64 | `local-base-linux-arm64.tar.gz` |

See the [README](../README.md) for what each platform supports. Then verify and install:

```sh
shasum -a 256 -c --ignore-missing checksums.txt   # or: sha256sum -c --ignore-missing checksums.txt
gh attestation verify local-base-linux-x64.tar.gz --repo timsexperiments/LocalBase
tar -xzf local-base-linux-x64.tar.gz              # macOS: unzip local-base-macos-arm64.zip
mkdir -p ~/.local/bin
mv local-base-linux-x64 ~/.local/bin/local-base
```

Each release also carries an SPDX SBOM, `local-base.spdx.json`.

## Homebrew

Releases include a formula, `local-base.rb`, generated from [`packaging/homebrew/local-base.rb.tmpl`](../packaging/homebrew/local-base.rb.tmpl). There is no published tap yet. A maintainer has to create one, such as `timsexperiments/homebrew-tap`, and commit that file to it. The formula has passed a Ruby syntax check only, not `brew audit` or a real install.

## Running as a service

`local-base start` installs, enables and starts a per-user service. It needs no root. `local-base stop`, `restart` and `status` manage it, and `local-base logs` reads its logs.

| Platform | Manager | Where it lives |
| --- | --- | --- |
| macOS | launchd | a LaunchAgent named `com.localbase.gateway.<hash>` |
| Linux | systemd `--user` | `~/.config/systemd/user/local-base-gateway-<hash>.service` |

The hash comes from the data directory, so separate roots get separate services.

On Linux a user service stops when you log out, unless lingering is on:

```sh
loginctl enable-linger "$USER"
```

Output goes to the user journal (`journalctl --user -u 'local-base-gateway-*'`). To run without a service manager, use `local-base serve`.

**Linux status: untested as an end-user install.** The service flow last passed on 2026-10-21 in a disposable Debian container (`go/tools/systemd-check.sh`), not on a real Linux host, and has not been re-run on the current tree.

## Uninstall

```sh
local-base stop
local-base uninstall --yes                     # removes the service and the data directory
rm -f ~/.local/bin/local-base ~/.local/bin/local-base.previous
```

**`uninstall` deletes all LocalBase data, including downloaded models, API keys and custom voices.** To keep the data, run only `local-base stop` and remove the binary. Chat history lives in your browser and isn't affected.

## Signing setup for maintainers

Tagged releases sign and notarize the macOS binaries in `.github/workflows/release-go.yml`. Linux archives aren't signed; they rely on checksums and GitHub attestations. The workflow needs these repository secrets:

| Secret | What it holds |
| --- | --- |
| `APPLE_CERTIFICATE` | Developer ID Application certificate, as a base64-encoded `.p12` |
| `APPLE_CERTIFICATE_PASS` | Password of that `.p12` |
| `APPLE_ID` | Apple ID used for notarization |
| `APPLE_ID_PASSWORD` | App-specific password for that Apple ID |
| `APPLE_TEAM_ID` | Apple developer team ID |

Pull requests run the same build unsigned. They receive no secrets and publish nothing.
