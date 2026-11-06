# details-check

Opens every `<details>` a page shows and records where its content lands, so two browsers can be compared.
Browsers box a `<details>` differently: `::details-content` arrived in Chrome 131, so a layout can look right
on an older browser and wrong on a current one. A `<details>` set to `display: contents` is always reported,
because that is the shape that broke the Image viewer (DESIGN §27.5).

    LBGO_CHROME=<one browser>     tools/details-check.sh http://127.0.0.1:<port> a.json
    LBGO_CHROME=<another browser> tools/details-check.sh http://127.0.0.1:<port> b.json
    bun tools/details-check/compare.ts a.json b.json

It exits 1 on a difference or on `display: contents`. It reads a gateway with `--no-auth`. It sees only what the
page shows, and says how many it found on each page, so run it against a gateway seeded with results.

These need data it doesn't create, so a run without that data doesn't cover them: Admin's confirms and permission
lists (a user, a role, a registration, an API key), Voices' Delete (a custom voice), Get models' `discover-files`
and alternatives (a search result), and Chat's compaction divider (a compacted chat).
