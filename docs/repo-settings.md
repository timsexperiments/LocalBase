# Recommended repository settings

For the repository owner. Unticked boxes haven't been applied. Ticked ones were on when read on 2026-11-04.

## Description and topics

- **Description:** `Self-hosted, OpenAI-compatible gateway for local AI models: chat, speech, images and video, with memory-safe model loading.`
- **Topics:** `local-ai`, `self-hosted`, `openai-compatible`, `llm`, `llama-cpp`, `whisper`, `stable-diffusion`, `go`, `htmx`
- **Social preview:** upload one once the UI screenshots exist. It's a 1280×640 image, set under Settings → General.

## The `main` ruleset

The `default` ruleset covers the default branch.

- [x] Require a pull request before merging, with squash the only merge method. A rebase merge stamps your profile name as committer, and a merge commit keeps history you may not want on `main`.
- [x] Block force pushes and deletions. Any history rewrite means lifting this deliberately.
- [x] Require `go-checks`, from `go.yml`. It passes only when every Go job that should run has passed, and passes when a change doesn't touch the Go tree.
- [ ] Remove the `validate` check from the ruleset when the pull request that deletes `ci.yml` merges. That check no longer exists afterwards, and a required check that never runs blocks every merge.
- [ ] After #403 merges, also require `hygiene` and `secrets`. A required check has to exist on `main` first. `release-go.yml` runs only when its paths change, and a required check that doesn't run blocks the merge, so don't require its jobs.
- [ ] After #403 merges, turn off merge commits and rebase merging in Settings → General, so the options match the ruleset.

## Release tags

A `v*` tag starts a release, and nothing protects those tags yet.

- [ ] Add a tag ruleset for `refs/tags/v*` that restricts creation, updates and deletion and blocks force pushes, with repository admins able to bypass it. Runtime tags such as `whisper-v0.0.2` don't match.
- [ ] Consider immutable releases, so published files and their tag can't change. No workflow changes a release after creating it, and `release-go.yml` publishes a draft, so anything added must be in the draft before it's published.

## Security

- [ ] Enable private vulnerability reporting, which `SECURITY.md` relies on.
- [x] Secret scanning and push protection, on top of the gitleaks job.
- [ ] Enable Dependabot alerts. Version updates are already configured.
- [ ] After #403 merges, enable Dependabot security updates. Before that, they'd open pull requests against the tree that merge replaces.

## Actions

- [x] Default workflow permissions: read-only. Workflows ask for more per job.
- [x] Require approval to run workflows for first-time contributors.
- [x] Actions can't approve pull requests.
- [ ] Allow selected actions only, and require full-length commit SHAs. Every action is already pinned. Allow GitHub's own actions plus `docker/setup-qemu-action`, `fsfe/reuse-action`, `golangci/golangci-lint-action`, `oven-sh/setup-bun` and `softprops/action-gh-release`.
- [ ] Add a `release` environment with you as required reviewer, limited to `v*` tags and `main`. Keep `prevent_self_review` off, or a sole maintainer can't approve their own release. Add `environment: release` to every job that reads the signing secrets **before** moving those secrets into it, or releases fail.

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

## Release credentials

The `release` environment above is where the Apple secrets go. For the credentials themselves:

- [ ] Put `RELEASE_AUTOMATION_PRIVATE_KEY` in the same environment, after adding `environment: release` to the jobs that read it, and give that GitHub App only what the manifest sync needs, on this repository alone.
- [ ] Use an app-specific password for notarizing, as [install](install.md#signing-setup-for-maintainers) says, not the account's password.
- [ ] Replace the certificate and that password when a maintainer leaves, or if a job holding them ever ran something it shouldn't have.
