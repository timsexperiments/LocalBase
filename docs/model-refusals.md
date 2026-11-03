# Why a model was refused: 422 or 503

Before loading a model, LocalBase checks it fits. The status code tells you whether trying again can help.

| Response | Meaning | What to do |
| --- | --- | --- |
| **422 `model_too_large`** | This model can never fit on this machine, even with nothing else running. No `Retry-After` | Don't retry. Use a smaller model or quantization |
| **503 `insufficient_memory`** with `Retry-After` | Not enough memory is free right now | Retry after closing other apps or when idle models have been evicted, or use a smaller model |

**Video is different.** A video request is accepted first and runs as a job, so a refusal shows as a job in the `failed` state with `insufficient_memory`, even when the model can never fit. Its message says to retry, although that can't help when the model is too large for the machine.

`local-base models recommend` lists what fits this machine.
