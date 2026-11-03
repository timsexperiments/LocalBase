# Recommended repository settings

For the repository owner. These are recommendations, and most aren't applied yet. A ruleset on `main` already does what's ticked below, with no one able to bypass it. Check a box against the settings page rather than this page, since nothing keeps the two in step.

## Description and topics

- **Description:** `Self-hosted, OpenAI-compatible gateway for local AI models: chat, speech, images and video, with memory-safe model loading.`
- **Topics:** `local-ai`, `self-hosted`, `openai-compatible`, `llm`, `llama-cpp`, `whisper`, `stable-diffusion`, `go`, `htmx`
- **Social preview:** upload one once the UI screenshots exist. It's a 1280×640 image, set under Settings → General.

## Branch protection for `main`

- [x] Require a pull request before merging.
- [ ] Require more status checks. Only `validate` is required today, and it goes when TypeScript does. Add `hygiene`, `secrets`, `licensing`, `test`, `test-linux`, `binaries`, `package`, and `smoke` for each runner. `release-go.yml` runs only when its paths change, and a required check that doesn't run blocks the merge, so use path-independent checks or a ruleset that handles skipping.
- [ ] Require branches to be up to date.
- [x] Block force pushes and deletions. Any history rewrite means lifting this deliberately.
- [ ] Require linear history, with squash-only merging. A rebase merge stamps your profile name as committer, and a merge commit keeps history you may not want on `main`.
- [ ] Add a tag ruleset limiting `v*`, `whisper-v*`, `sd-server-v*`, `tts-server-v*` and `kokoro-v*` to maintainers, and blocking their deletion and any move. A `v*` tag starts a release, and a moved tag changes what a published release means.

## Security

- [ ] Enable private vulnerability reporting, which `SECURITY.md` relies on.
- [ ] Enable secret scanning and push protection, on top of the gitleaks job.
- [ ] Enable Dependabot alerts and the dependency graph. Version updates are already configured for Go modules, Actions, Bun and Docker.

## Actions

- [ ] Default workflow permissions: read-only. Workflows ask for more per job.
- [ ] Require approval to run workflows for first-time contributors.
- [ ] Don't let Actions create or approve pull requests, unless a sync workflow turns out to need it. The manifest sync workflows should be checked before you change this.
- [ ] Allow only actions pinned to a full commit SHA, and only the actions this repository uses. The repo guard checks pins on pull requests, and this setting holds for every run.

## Release credentials

- [ ] Put the Apple secrets in a `release` environment with a required reviewer, and add `environment: release` to each job that reads them. `sign-macos` in `release-go.yml` is one. The runtime workflows have others, and `scripts/check-signing-jobs.sh` shows which jobs may hold them.
- [ ] Put `RELEASE_AUTOMATION_PRIVATE_KEY` in the same environment, and give that GitHub App only what the manifest sync needs, on this repository alone.
- [ ] Use an app-specific password for notarizing, as [install](install.md#signing-setup-for-maintainers) says, not the account's password.
- [ ] Replace the certificate and that password when a maintainer leaves, or if a job holding them ever ran something it shouldn't have.

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
