# 0.2.0 release checklist

**Status: no-go until the blockers below are done.** Don't push a tag before then. A `v*` tag starts the release workflow.

## Blockers

- [ ] The maintainer signs off #403, and it's squash-merged. See `go/docs/merge-readiness.md`.
- [ ] The stack on top is merged: #404, #413-#417 and this PR, and the backend fixes #405, #406, #409 and #411.
- [ ] Transcription default decided: English, or auto-detect on multilingual models.
- [ ] `go-dual` green. It needs #411.
- [ ] The first-run story is accepted: **a fresh install has no browser sign-in** until a provider is set up.
- [ ] Set the date on the `0.2.0` entry in `CHANGELOG.md`, and regenerate notices if dependencies moved: `scripts/release/generate-notices.sh`.

## Decisions needed from the owner

Drafted for the owner; the recommendations are proposals, not decisions.

| Decision | Options | Recommended |
| --- | --- | --- |
| **Disclosure** of the 0.1.0 issue in `go/docs/merge-readiness.md` §4 | A security advisory and docs; docs only; or patch 0.1.0 first. Publish now or with 0.2.0 | An advisory and docs, published with 0.2.0 at the latest |
| **Identity cleanup** | Support purge of closed PR refs; also rewrite `main`; or accept the address on `main` | Support purge, accept the address on `main`, and fix account settings before squashing #403 |
| **Memory reserve default** | Keep 15%, at least 8 GiB, which stops any model loading on an 8 GiB Mac; or change it, such as 10%, at least 3 GiB. See `go/docs/memory-reserve.md` | 10%, at least 3 GiB, **after** a check on a real 8 or 16 GiB Mac. Otherwise keep today's and say so in the README |
| **Speech marking and C2PA** | Marking on, no C2PA, as today; pass C2PA through; or marking off by default | Keep marking on. Leave C2PA for later |
| **Transcription language default** | English, as 0.1.0 does; or auto-detect on multilingual models | Auto-detect on multilingual models |
| **DCO** | No sign-off, inbound = outbound, as drafted; or require sign-off and enforce it in CI | No sign-off |
| **`good first issue` issues** | Create the proposed list; change it; or none yet | Create it after 0.2.0 ships, when contributors can use a release |
| **TypeScript retirement date** | A gate; or a fixed date. See [retirement](typescript-retirement.md) | The gate. A date adds nothing while its checks are open |
| **Video, microphone and NVIDIA verification** | Each one can block the release, or ship as documented as unverified | Microphone and real-phone checks block, because they cover what people use first. Run video if a 25.6 GiB window comes up, otherwise ship it marked unverified. NVIDIA doesn't block, because the docs say it isn't qualified |

## Merge order

Docs merge after what they describe. Arrows are stacks; each PR merges after the one before it.

| Order | PRs |
| --- | --- |
| 1 | #403, then #411 and #421, as `go/docs/merge-readiness.md` says |
| 2 | #404, #408, #444. #444's lint job skips until #456 brings its config |
| 3 | #413 → #414 → #415 → #416 → #417 → #418. #414's commit and date stamping needs #406 |
| 4 | Go features: #421, #405 → #409 → #424, #423, #425, #426, #427, #430, #406, #422, #432, #447, #456. #424 and #406 both change the same line in `go/cmd/localbase/serve.go`, so whichever goes second needs a small conflict fix |
| 5 | Feature docs, each after its feature: #436 (#409), #437 (#424), #438 (#426), #439 (#411), #440 (#423), #441 (#430), #442 (#425), #443 (#427) |
| 6 | UI v2: #410, then #448 directly after it, then the rest of its stack. #418 must already be in, because it allowlists the fonts and icons and annotates their license |
| 7 | #446, last, because it links to all of the feature docs |

The Go order within step 4 belongs to the Go thread. If it changes, keep the doc PRs after their features.

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
