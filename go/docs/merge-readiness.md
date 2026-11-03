# Merge readiness: the Go rewrite stack into main

Status on 2026-11-02. Nothing here has been merged. The decision is the user's,
through the operator. Measured on `rewrite/go-merge-readiness` (draft #403), which
is the exact tree proposed for main.

**Summary**

- **Recommendation:** one squash of #403 into main, then close the other 34 PRs as
  superseded. No rebase or history rewrite is needed.
- **Three things the stack could not have shown, found by this pass and fixed only
  on #403:** a 23 MB binary at the tip, a test leak that made root `bun test` fail
  96 tests, and #396's last 4 commits missing from the FE stack.
- **#403 is green on all five checks, including the three that only run on a PR
  into main.**
- **Merging does not retire TS.** `src/` is untouched. Retiring is a separate
  step, gated on the checklist below.

## 1. Every open PR, CI at its own head

| PR | Base | Head | CI | Checks that ran |
| --- | --- | --- | --- | --- |
| #369 | `main` | `b36a7129` | RED | runtime-smoke-linux, test, validate, validate-macos |
| #370 | `go` | `0aa8ef9a` | RED | test |
| #371 | `go-model-import` | `97e575a7` | green | test |
| #372 | `go-m4` | `6a48a6d5` | RED | test |
| #373 | `go-m5` | `cd5dc171` | RED | test |
| #374 | `go-m3-ui` | `087bf779` | green | test, test-linux |
| #375 | `go-m6` | `c1836211` | green | test, test-linux |
| #376 | `go-postcutover` | `6b908f76` | green | test, test-linux |
| #377 | `go-decision-overflow` | `2b77dbdb` | green | test, test-linux |
| #378 | `go-observability` | `cbba4b1f` | green | test, test-linux |
| #379 | `go-placement` | `c30025a8` | green | test, test-linux |
| #380 | `go-placement-check` | `98e854c3` | green | test, test-linux |
| #381 | `go-placement-wiring` | `36ff275d` | green | test, test-linux |
| #382 | `go-placement-nvidia` | `c08270b3` | green | test, test-linux |
| #383 | `go-unload` | `1c870aa8` | green | test, test-linux |
| #384 | `go-placement-ui` | `aaadba93` | green | test, test-linux |
| #385 | `go-runs` | `7f0fa38f` | green | test, test-linux |
| #386 | `go-fe-redesign` | `d21373a9` | green | test, test-linux |
| #387 | `go-fe-chat` | `02c0f1a6` | green | test, test-linux |
| #388 | `go-fe-models` | `6aa6c0f1` | green | test, test-linux |
| #389 | `go-runs-lab` | `e8e20194` | green | test, test-linux |
| #390 | `go-fe-running` | `e6e5571a` | green | test, test-linux |
| #391 | `go-fe-lab` | `066e911c` | green | test, test-linux |
| #392 | `go-fe-voices` | `dc549ecf` | green | test, test-linux |
| #393 | `go-fe-settings` | `23e4c024` | green | test, test-linux |
| #394 | `go-fe-studio-base` | `c74e6d83` | green | test, test-linux |
| #395 | `go-unload` | `f5ab48b5` | green | test, test-linux |
| #396 | `go-studio-api` | `8f901879` | green | test, test-linux |
| #397 | `go-fe-studio-landing` | `ba2dcf43` | green | test, test-linux |
| #398 | `go-fe-studio-image` | `1b9eb2b2` | green | test, test-linux |
| #399 | `go-fe-studio-decisions` | `32881c6e` | green | test, test-linux |
| #400 | `go-fe-studio-video` | `f3ae0d62` | green | test, test-linux |
| #401 | `go-fe-studio-speech` | `aae9ef77` | green | test, test-linux |
| #402 | `go-fe-studio-transcribe` | `bdacfb0c` | green | test, test-linux |
| #403 | `main` | see the PR | green | test, test-linux, validate, validate-macos, runtime-smoke-linux |

**The four red PRs are old heads, fixed higher up.** Their last runs were on
2026-10-21:

| PR | Failing tests | Cause | Fixed in |
|---|---|---|---|
| #369, #370 | `TestBuiltInCatalogMatchesSource` | main's #368 changed the catalog sources; the generated Go catalog was out of date | #374 (`e57bed13`, the #368 port) |
| #372, #373 | the same, plus `TestAnnotationApplicationGolden`, `TestDiscoveryGolden` | the same catalog change | #374 |

`test-linux` does not appear on #369–#373 because that job did not exist yet.

**Green here means less than it looks.** Stacked PRs run only `go.yml` (`test`,
`test-linux`). `ci.yml` (`validate`, `validate-macos`, `runtime-smoke-linux`) runs only
on PRs into main, so of all these only #369 ever ran it, on an old head. That is how
the `bun test` failure stayed hidden. #402's head would still fail `validate`.

**One known flake:** `TestUnloadScope` failed once on an intermediate #400 commit, in
temp-dir cleanup only (DESIGN §24).

## 2. Merge strategy

### What #403 contains

```text
#402 (bdacfb0c): the top of the stack
  + merge of #396 (8f901879): its last 4 commits were not in the FE stack
  + remove go/localbase, and ignore it
  + go/tools/jstest/runs.test.ts: restore the globals it stubs
  + this document
```

The dry-run merge of #396 into #402 had no conflicts. No existing PR was changed.

### The options

| | Single squash of #403 (recommended) | Bottom-up, 34 merges | Rewrite history |
|---|---|---|---|
| Binaries in main's history | **none** | 4 blobs, 38.1 MB packed, in every clone for good (the repo is 58 MB today) | none |
| Effort | one merge | 34 retargets and CI runs | force-push about 30 branches |
| Studio-base branch | irrelevant | needs handling (below) | needs handling |
| Red intermediate commits on main | none | #369, #370, #372, #373 land red | same as bottom-up |
| Per-PR history on main | lost: one large commit (586 files, about 154,000 lines) | kept | kept |

**Why squash.** The history is messy, with red points and binaries, and the squash
removes that without any rewriting. DESIGN.md is the real record, and every PR stays
readable on GitHub. The cost is that `git bisect` on main cannot look inside the
rewrite.

**The binaries.** There are two, and the one everybody knew about is the smaller:

| File | Blobs | Packed | State |
|---|---|---|---|
| `go/localbase` | 3 (M5, in `2ff82c36`, `db2f5174`, `454d4266`) | 33.2 MB | **still at #402's tip**; removed only on #403 |
| `go/fakebackend` | 1 (#380) | 4.9 MB | removed in #384 |

- A squash of #403 keeps both out of main without a rewrite. **A squash of #402 would
  put `go/localbase` in main.**
- GitHub keeps them under `refs/pull/*`, as it does all these commits. Normal clones
  don't fetch those. **Don't tag or branch an old stack head into a normal ref
  afterwards**, or clones get the binaries back.
- A rewrite gains nothing here and would invalidate the SHAs the deployed binaries are
  named after.

### Exact steps (single squash)

1. Update #403 if the user's checks change anything. **#411 and #421 must land with
   #403:** merge #411, then #421, into `rewrite/go-merge-readiness` before the squash,
   and rerun CI and `-impl=both` on the result. Without #411 a model that can never
   fit answers 503 instead of TS's 422. Without #421 a fresh root can't install its
   default model from the API. Both are draft and green at their own heads (#411 at
   `f9e9beb2`, #421 at `b68f9a3b`). A trial merge of the two reports no conflicts. They
   have not yet been tested together.
2. Mark #403 ready. Confirm all five checks at its final head.
3. Squash-merge #403. The title should say what it is, not "DO NOT MERGE".
4. Check the result: `git log -1 --format='%an %cn'` must show the account's profile
   name "Tim" and no other name, and `git ls-tree -r main | grep -E 'go/(localbase|fakebackend)$'`
   must find nothing.
5. Close #369–#402 as superseded by the squash, with a link. **Keep the branches until
   TS is retired**, so the deployed SHAs stay easy to find.

### If bottom-up is chosen instead

Order: #369, #370, #371, #372, #373, #374, #375, #376, #377, #378, #379, #380, #381,
#382, #384, #385, #389, #395, #396, #383, #386, #387, #388, #390, #391, #392, #393,
#394, #397, #398, #399, #400, #401, #402, then the two fix commits from #403.

- Use merge commits, not squash: a squash breaks the ancestry each next PR relies on.
- **The studio-base trick:** `rewrite/go-fe-studio-base` (the operator's note says
  `go-studio-base`) has no PR. It is one merge commit, `3864f52f`, of #393
  (`23e4c024`) and #395 (`f5ab48b5`). Once both are on main, retarget #394 to main;
  the branch then adds nothing.
- #396 enters twice: `415821af` through #400's merge `e5376c60`, and whole if landed
  before #394. That merge resolved `lab.go` and `lab.html` to the FE's code, which
  keeps the seeding through `imageForm`/`startImages`.

## 3. Drift against main

**None.** Main (`cdb45205`, #368) is an ancestor of every stack head from #374 up and
of #369. It has 0 commits the stack lacks, and #403 has 178 commits main lacks. No
rebase or merge from main is needed. GitHub reports #403 `MERGEABLE`/`CLEAN`.

- #368 is already ported (`e57bed13`).
- `src/`, `drizzle/`, `package.json` and `bun.lock` are unchanged from main, so TS is
  byte-for-byte what main has. Outside `go/` the stack changes only `.github/workflows/go.yml`
  (new), `.gitignore`, `README.md` and `.design/research/chat/BRIEF.md` (new).
- The operator's local `main` checkout is 53 commits behind origin. That
  doesn't affect any of this, and I did not touch it.
- **Recheck** if anything lands on main first, such as a Dependabot update.

## 4. Migration and rollback

**Schema.** `goSchemaVersion` stays 1 (`TestGoSchemaVersionForRollback`). The drizzle
journal is unchanged, so rollback stays a binary swap. Go tables are `go_schema`,
`imported_models`, `model_backend_selections`, `go_settings` and `model_placements`;
only the last is new since `75626a4`.

**On disk.** `<root>/runs/` is new: checkpoints with model output in plaintext
(0600), deleted on dismiss, on expiry, and by the startup sweep of a build that knows
runs.

**Drill, run on this tree (2026-11-02), on a scratch root** with
`tools/placement-rollback-check.sh`: `local-base.go-75626a4`, `local-base.ts-0.1.0` and
this build each served `/health` and `/v1/models` against a database with a
`model_placements` row; `go_schema` stayed 1 and the row was kept. It requests no
inference, and `runs/` was empty.

**Live is already past `75626a4`:** its binary is dated Oct 31 and its root has
`runs/`. The binary carries no commit stamp, so the operator should confirm which
commit it is.

| | Rollback to `local-base.go-75626a4` | Rollback to `local-base.ts-0.1.0` |
|---|---|---|
| Starts on the database | yes (drill) | yes (drill), **if the active models' files are installed**: TS refuses to start otherwise (found in the §20 drill) |
| Lost: features | detached runs, placement, explicit unload, observability fixes (#377), the redesign and all studios, seed echo, speech-cut flag | all of that, plus everything Go-only (§17): the fit-aware default model, imports and backend selection, #375–#376 |
| Kept, unused | `model_placements` rows | those, plus `imported_models`, `model_backend_selections`, `go_settings` |
| In flight | runs die; pages find no run | the same |
| Pages | `/app/studio/*` is gone; bookmarks there break | the same; the old UI |
| Browser history | stays in the browser | stays in the browser |

**After either rollback, delete `<root>/runs/*.json` by hand.** Those binaries don't
sweep it, so model output stays on disk in plaintext until a newer build starts.
Restoring a placement choice needs nothing: rows are kept for when Go returns.

**Not rehearsed:** a rollback from this tree on a copy of the live root, as §17.7 did at
cutover. Do one before relying on the TS path.

## 5. Sign-off checklist for the user

**Your decisions**

- [ ] Squash #403 (recommended), or bottom-up?
- [ ] Transcription with no language: keep English, or auto-detect on multilingual
      models (recommended; DESIGN §24)? This also decides chat dictation.

**Verified, no action needed**

- [x] CI green on #403 at its head, including the checks that only run for main.
- [x] Dual contract suite 97 pass, 5 skip. An earlier run on the same tree failed on
      memory (`TestEmbeddings`) while the machine was short; see section 6.
- [x] Rollback drill above. Drift: none. `.design/` adds one text file and no images.

**Before merging: on a real phone** (nothing here has run on one; all of it was
simulators and test browsers)

- [ ] Sign in with Google at the public hostname.
- [ ] Start a chat, switch tabs, lock the phone, come back: it carried on and shows
      the whole answer, over the real tunnel. **This is the feature you asked for.**
- [ ] Keyboard in chat and in a studio's dock.
- [ ] Microphone in Transcribe and dictation: permission, recording format on iOS,
      locking mid-recording.
- [ ] Share sheet from Image. A video plays (real H.264).
- [ ] VoiceOver or TalkBack on Chat and one studio. Contrast in daylight.

**Before merging: by ear and eye**

- [ ] Listen to a long Speech script: joins, clicks. Nobody has yet.
- [ ] Read Decisions results and say whether the wording is right.

**Before retiring TS (planned about 2026-11-04, two weeks after cutover)**

- [ ] Real video through the page. It needs 25.6 GiB available, and has never had it.
- [ ] **GPU test host, operator with you, through its serialized queue:** NVIDIA placement (auto
      split, GPU-only refusal, the startup check), GPU admission; whisper, speech and
      image on Linux. None of this has run on real NVIDIA hardware.
- [ ] A rollback rehearsal from this tree on a copy of the live root.
- [ ] Confirm no rollback happened since cutover, so the two weeks actually count.

The date is the earliest possible, not a deadline: on current evidence I expect the
GPU test host and video items to decide it. Merging can come first, since TS stays in the
repo and the TS binary stays on disk.

**After merging, before tagging**

- [ ] **Do not push a `v*` tag yet.** Follow `docs/release-checklist.md`, which needs the Go release job in `release-go.yml`.
- [ ] Separate later PR: retire TS sources and bun CI, and move `drizzle/` (§17.3).

## 6. Evidence and limits

- **Run on the tree (`154fa896` plus the `go/localbase` removal):** gofmt, vet, unit tests,
  race on gateway, web, runtime, obs and store, Tailwind check, typecheck, `bun run check`,
  `bun test go/tools/jstest`, dual contract, rollback drill.
- **One thing this pass cost:** the first contract run coincided with live evicting its
  resident Clef Flash for memory at 09:21:55 UTC. The pressure came from this run. Live
  was idle, and the model reloads on its next request. The re-run caused no eviction.
- **Reviewed, no change:** the gzip asset change in `web.go` compresses text assets once
  under a lock, sends `Vary` on both variants, and has a round-trip test.
- **Worth a look, not blocking:** `go/internal/gateway/testdata/video.json` is 6.3 MB of
  text, and it stays in the squash. `.design/` was tracked on main despite a local
  `.git/info/exclude`; it is now untracked and in `.gitignore`.
- **Not checked:** PR-by-PR code review of the 586 files (this pass is about merging, not
  a re-review), and anything on a real phone or NVIDIA hardware.
