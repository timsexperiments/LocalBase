# Recommended repository settings

For the repository owner. **Nothing here has been applied.**

## Description and topics

- **Description:** `Self-hosted, OpenAI-compatible gateway for local AI models: chat, speech, images and video, with memory-safe model loading.`
- **Topics:** `local-ai`, `self-hosted`, `openai-compatible`, `llm`, `llama-cpp`, `whisper`, `stable-diffusion`, `go`, `htmx`
- **Social preview:** upload one once the UI screenshots exist. It's a 1280×640 image, set under Settings → General.

## Branch protection for `main`

- [ ] Require a pull request before merging.
- [ ] Require status checks: `hygiene`, `secrets`, `licensing`, `test`, `test-linux`, `binaries`, `package`, and `smoke` for each runner. `release-go.yml` runs only when its paths change, and a required check that doesn't run blocks the merge, so use path-independent checks or a ruleset that handles skipping.
- [ ] Require branches to be up to date.
- [ ] Block force pushes and deletions. Any history rewrite means lifting this deliberately.
- [ ] Require linear history, with squash-only merging. A rebase merge stamps your profile name as committer, and a merge commit keeps history you may not want on `main`.
- [ ] Add a tag ruleset limiting `v*` to maintainers, because a tag starts a release.

## Security

- [ ] Enable private vulnerability reporting, which `SECURITY.md` relies on.
- [ ] Enable secret scanning and push protection, on top of the gitleaks job.
- [ ] Enable Dependabot alerts. Version updates are already configured.

## Actions

- [ ] Default workflow permissions: read-only. Workflows ask for more per job.
- [ ] Require approval to run workflows for first-time contributors.
- [ ] Don't let Actions create or approve pull requests, unless a sync workflow turns out to need it. The manifest sync workflows should be checked before you change this.
- [ ] Consider a `release` environment with a required reviewer for the Apple secrets. `release-go.yml` would need `environment: release` added to `sign-macos`.

## General

- [ ] Delete head branches on merge is on today. **Keep the old stack branches until TypeScript retires**, as `go/docs/merge-readiness.md` says, so hold off merging those PRs individually.
- [ ] Turn on Discussions if you want a place for questions, and link it from `SUPPORT.md`. It's off today.
- [ ] Turn off the Wiki and Projects if unused, so documentation stays in the repo.
- [ ] Create the labels proposed in [labels](labels.md).

## Documentation site

The site builds on every docs change, but nothing is published. To turn it on:

- [ ] Settings → Pages: set the source to **GitHub Actions**.
- [ ] Settings → Secrets and variables → Actions → Variables: set `DOCS_PAGES_ENABLED` to `true`.
- [ ] Run the **Docs site** workflow from `main`, or push a docs change.

It publishes to `https://timsexperiments.github.io/LocalBase/`. Only the publish job can write to Pages.
