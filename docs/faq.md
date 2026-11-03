# FAQ

**Does anything leave my machine?**
No prompts or outputs. LocalBase contacts the network to download what you ask for (models, runtimes) and to fetch its catalog feed. Telemetry export is off unless you configure an OTLP endpoint.

**Why does a request need a `model`?**
The API routes by the model you name, and doesn't substitute a different one. `GET /v1/models` lists the choices.

**Can I run several models at once?**
One per type: one chat model, one speech model and so on, if memory allows.

**Why was my model refused when I have free memory?**
LocalBase plans for what the model needs while running, plus a safety reserve, not only the file size. `local-base models show <id>` shows the figures.

**Can I use a model that isn't in the catalog?**
Yes: import it from Hugging Face, a URL or a local file. Imported models aren't qualified by the project. Their fit figures are estimates.

**Does it work on Windows?**
No, and it isn't planned.

**Does it work with NVIDIA GPUs?**
Not qualified yet. See [platforms and hardware](../README.md#platforms-and-hardware).

**Is it safe to expose to the internet?**
Only through an authenticated HTTPS proxy or tunnel with a sign-in provider configured. See [authentication](authentication.md).

**Who owns the data in my chats?**
Chat history is stored in your browser, not on the server. See `go/DESIGN.md` §21.6 for the short-lived server checkpoints.

**What's the license?**
AGPL-3.0-or-later. Models have their own licenses, shown in the catalog.
