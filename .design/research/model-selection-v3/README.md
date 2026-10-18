# Model selection v3

Start with [SUMMARY.md](SUMMARY.md). Compare [the gallery](index.html), or open [REVIEW.html](REVIEW.html) for a self-contained thumbnail comparison. Full-size originals are linked in [screenshots/INDEX.md](screenshots/INDEX.md).

Each concepts/*/index.html file is standalone. It works without a server or internet. For loopback preview, run `bun .design/research/model-selection-v3/serve.mjs`; its default port is 24082. Set LOCALBASE_DESIGN_PORT to another permitted scratch port when needed. Stop the process with Ctrl+C.

Rebuild generated HTML with the installed tools:

```sh
bun .design/research/model-selection-v3/prototype-source/build.mjs
bun .design/research/model-selection-v3/prototype-source/build-review.mjs
bun .design/research/model-selection-v3/prototype-source/build-inline.mjs
```

The inline build uses macOS sips for small JPEG thumbnails and removes its own temporary directory. The original 92 PNG screenshots remain unchanged. Catalog snapshots are read-only source data, not a downloaded model cache. Memory, disk availability, installation state, timings, and operation controls are simulated. Refresh resets the prototype.

Direct state links accept `?state=picker&theme=dark`, or library, get, add, download, warning, empty, running. Compass also supports home, guide and recommend. On the page, Explore prototype states exposes recovery examples. Artifacts only, no PR or deployment.
