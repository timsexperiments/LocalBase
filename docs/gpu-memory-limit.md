# GPU memory limit on Apple Silicon

Unified memory doesn't all go to the GPU. A model larger than the GPU's working-set limit is refused with 422 `model_too_large`.

LocalBase picks the limit in this order:

1. **`LOCALBASE_GPU_WORKING_SET_GB`,** if you set it.
2. **What Metal reports.** `serve` reads this in the background at startup, and before a model switch if a runtime has been installed since.
3. **A heuristic:** 2/3 of memory up to 36 GiB, and 3/4 above.

Two things use the heuristic even on a machine where Metal can answer:

- **The first load on a fresh install,** because the runtime that reads Metal's figure isn't installed yet.
- **CLI commands such as `local-base doctor`.** Only `serve` reads Metal, so `doctor` can report a lower limit than the running gateway uses.

As an example, a 64 GiB M4 Max reports 53084 MiB against the heuristic's 48 GiB. Other Mac sizes haven't been measured.
