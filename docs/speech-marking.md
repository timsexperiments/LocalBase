# AI-content marking in synthesized speech

Some speech runtimes mark what they generate. This note says what LocalBase does.

| Runtime | With marking on (the default) | With marking off |
| --- | --- | --- |
| CrispASR | An inaudible AI-content watermark in the audio. **No C2PA manifest** | No watermark |
| tts-server | Not marked | Not marked |

**Why no C2PA:** CrispASR can write a C2PA manifest into its own WAV files, but LocalBase keeps only the audio samples and writes its own WAV header, so the manifest isn't passed through. Whether to pass it through is an open project decision.

**Changing it:**

```sh
local-base speech marking        # show the setting
local-base speech marking off    # or: on
```

**Turning marking off makes you responsible.** LocalBase then passes CrispASR's `--no-watermark`, `--no-c2pa` and `--no-spoken-disclaimer`, along with `--accept-marking-responsibility`, on your behalf. Some places have rules about labelling synthetic media or cloned voices, and those rules apply to you whichever setting you choose. This note isn't legal advice.

**Don't rely on the watermark as proof.** Editing or re-encoding audio may remove it, and this project hasn't tested its robustness.
