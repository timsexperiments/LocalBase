# 0.2.0 release checklist

**Status: no-go until the blockers below are done.** Don't push a tag before then. A `v*` tag starts the release workflow.

## Blockers

- [ ] The maintainer signs off #403, and it's squash-merged. See `go/docs/merge-readiness.md`.
- [ ] The stack on top is merged: #404, #413-#417 and this PR, and the backend fixes #405, #406, #409 and #411.
- [ ] Transcription default decided: English, or auto-detect on multilingual models.
- [ ] `go-dual` green. It needs #411.
- [ ] The first-run story is accepted: **a fresh install has no browser sign-in** until a provider is set up.
- [ ] Set the date on the `0.2.0` entry in `CHANGELOG.md`, and regenerate notices if dependencies moved: `scripts/release/generate-notices.sh`.

## What's verified, and what isn't

| Area | State |
| --- | --- |
| Unit, race and contract suites on macOS and Linux CI | Verified |
| Differential Go and TS suite on a macOS runner, no real models | Verified with #411 on a test branch |
| Cross-compile, archive, checksum, SBOM generation | Verified in dry run |
| Installer: fresh, upgrade, tampered, missing checksum, serve | Verified on Ubuntu x64 and ARM64, macOS ARM64 and Intel |
| Fresh install to first chat with a real model, macOS ARM64 | Verified, with foreground `serve` |
| Secrets scan of all file versions, including closed-PR refs | Clean. Binary files and archives weren't scanned |
| Rollback by binary swap, scratch root, no inference | Verified. **No rehearsal on a long-lived root** |
| **macOS signing, notarization, attestation, draft creation** | **Never run. Tag-only** |
| **`local-base start` on a real Linux host** | **Not verified.** A disposable container passed on 2026-10-21 |
| **Real NVIDIA hardware** (placement, GPU admission, Linux media runtimes) | **Not verified** |
| **Real phone** (sign-in, tab switching and locking, keyboard, share sheet, screen readers) | **Not verified** |
| **Real microphone** (Transcribe, dictation) | **Not verified** |
| **Real video through the page** (needs 25.6 GiB free) | **Not verified.** The API has run on a Mac |
| **Listening to long speech, reading Decisions wording** | **Not verified** |
| Homebrew formula | Syntax check only. No tap exists |

Everything marked not verified is stated in the README or docs, so shipping with it is a judgment call rather than a hidden risk. **The maintainer decides which of these block.**

## Cutting the release

From a clean, up-to-date `main`:

```sh
git switch main && git pull --ff-only
gh run list --branch main --limit 5                # everything green
go/tools/build.sh /tmp/lb-check && /tmp/lb-check version
git tag -a v0.2.0 -m "LocalBase 0.2.0"
git push origin v0.2.0
gh run watch "$(gh run list --workflow release-go.yml --limit 1 --json databaseId -q '.[0].databaseId')"
```

The workflow checks the tag matches the source version and that the changelog has an entry. It ends by creating a **draft** release, so nothing is public yet.

## Before publishing the draft

Test the signed macOS build on a real Mac, because CI runners don't enforce everything newer macOS does.

```sh
mkdir -p /tmp/lb-rc && cd /tmp/lb-rc
gh release download v0.2.0 -p 'local-base-macos-arm64.zip' -p 'checksums.txt' -p 'install.sh' -p 'local-base.spdx.json'
shasum -a 256 -c --ignore-missing checksums.txt
gh attestation verify local-base-macos-arm64.zip --repo timsexperiments/LocalBase
unzip -q local-base-macos-arm64.zip
xattr -w com.apple.quarantine "0081;00000000;Test;" local-base-macos-arm64   # act like a browser download
spctl --assess -vv --type open --context context:primary-signature local-base-macos-arm64
./local-base-macos-arm64 --version && ./local-base-macos-arm64 version
./local-base-macos-arm64 --root /tmp/lb-rc/root --non-interactive doctor
```

The assessment should report a notarized Developer ID.

## Publish

```sh
gh release edit v0.2.0 --draft=false --latest
gh api repos/timsexperiments/LocalBase/releases/latest -q .tag_name   # must print v0.2.0
```

**Check "latest" again after any runtime release.** The install script downloads from `releases/latest`, so a runtime release marked latest would break it.

## After publishing

- [ ] Run the install script from the real URL on a clean account or machine.
- [ ] Commit `local-base.rb` from the release to a tap, if there is one.
- [ ] Add screenshots to the README.
- [ ] Apply the [repository settings](repo-settings.md).
- [ ] Remove the "hasn't been released yet" note from the README.
