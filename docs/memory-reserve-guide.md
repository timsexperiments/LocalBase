# The memory reserve

LocalBase keeps a reserve of memory free for the system. By default it's **15% of total memory, and at least 8 GiB**.

| Check | Rule | Result |
| --- | --- | --- |
| Can never fit | Model is larger than total minus reserve, or than the GPU working-set limit | 422 `model_too_large` |
| Pressure, no new loads | Free is under 1.25 × reserve | 503 `insufficient_memory` |
| Not enough free now | Free minus pending minus model is under the reserve | 503 `insufficient_memory` |
| Pressure monitor, critical | Free is under the reserve | **Running models are stopped** |

"Free" means free, speculative and inactive pages together.

## What that means on each size of machine

| Total GiB | Reserve (stops models below this) | No new loads below | Free needed, 1 GiB model | Free needed, 4 GiB | Free needed, 8 GiB | Largest model ever |
| --- | --- | --- | --- | --- | --- | --- |
| 8 | 8.0 | 10.0 | can never load | can never load | can never load | 0.0 |
| 16 | 8.0 | 10.0 | 10.0 | 12.0 | 16.0 | 8.0 |
| 24 | 8.0 | 10.0 | 10.0 | 12.0 | 16.0 | 16.0 |
| 32 | 8.0 | 10.0 | 10.0 | 12.0 | 16.0 | 21.3 |
| 64 | 9.6 | 12.0 | 12.0 | 13.6 | 17.6 | 48.0 |
| 128 | 19.2 | 24.0 | 24.0 | 24.0 | 27.2 | 96.0 |

These are computed by the real admission code on a simulated host, not measured on each machine. The last column uses the working-set heuristic, so a real Mac may allow more. A model's demand is its estimate, which can exceed its file size.

- **8 GiB:** nothing can load with the default reserve.
- **16 GiB:** an 8 GiB model needs all 16 GiB free, which can't happen on a running system. Running models stop as soon as free memory drops under 8 GiB.

**The default may change.** Options and the analysis are in [`go/docs/memory-reserve.md`](../go/docs/memory-reserve.md). No default has changed, and a new one would only reach new data directories.

## Help by checking a real Mac

Nobody has tested this on a real 8 or 16 GiB Mac, and any new default needs that first. From `go/`:

```sh
tools/reserve-check.sh sample 300     # read-only, during normal use with a browser and apps open
tools/reserve-check.sh probe          # allocates memory in steps; save your work first
```

`probe` refuses to start if LocalBase or a model runtime is running, or on more than 24 GiB. It stops at 4 GiB free, at any macOS pressure above normal, at half the machine's memory, or after 5 minutes. To test closer to a line, pass a lower floor such as `probe 3`, at your own judgement.

Please report how much is free in normal use, when the pressure level or compressor moves, and whether the machine stayed responsive. **The script hasn't been run on a real small Mac yet.**
