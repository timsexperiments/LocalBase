# Corresponding source

The runtime includes `phonemizer` 1.2.1, whose exact source is [xenova/phonemizer.js at commit `6835144b7ee9043129222549c1ed2f6a27216278`](https://github.com/xenova/phonemizer.js/tree/6835144b7ee9043129222549c1ed2f6a27216278). Its package build command is `npm run build`. That source contains generated eSpeak-ng WASM and data, but does not identify the eSpeak-ng revision or document how those artifacts were built. We cannot establish the exact eSpeak-ng source revision from the available upstream materials.

For three years from distribution, the LocalBase maintainers will provide corresponding source for covered components on request: [LocalBase maintainers](https://github.com/timsexperiments/LocalBase/issues).

## Bun

The `kokoro-tts` executable is the unmodified official prebuilt Bun 1.4.2 (commit [`744846f844374847c902b5e7fd59b4342a51ef99`](https://github.com/oven-sh/bun/tree/744846f844374847c902b5e7fd59b4342a51ef99), tag `bun-v1.4.2`). Bun is MIT-licensed; see `LICENSE.bun.md`, its upstream `LICENSE.md` at that commit, for the linked-library and embedded-polyfill inventory. License texts for the native and Rust components statically linked into Bun, including the Rust crate closure and the Rust standard library, are in `bun/`; `bun/SOURCES.txt` records where each one came from.

Bun 1.4.2 statically links WebKit/JavaScriptCore from commit [`2e2aa2290fac856d6f451ceacb58f7f5b44dd057`](https://github.com/oven-sh/WebKit/tree/2e2aa2290fac856d6f451ceacb58f7f5b44dd057), the `WEBKIT_VERSION` in Bun's `scripts/build/deps/webkit.ts` at the pinned commit. Under LGPL-2.1 §6, its corresponding source can be obtained with:

```sh
git clone https://github.com/oven-sh/bun.git
cd bun
git checkout 744846f844374847c902b5e7fd59b4342a51ef99
git clone https://github.com/oven-sh/WebKit.git vendor/WebKit
git -C vendor/WebKit checkout 2e2aa2290fac856d6f451ceacb58f7f5b44dd057
```

To relink, install the prerequisites in the pinned Bun `CONTRIBUTING.md` (Bun's Rust code uses the `nightly-2026-07-20` toolchain from its `rust-toolchain.toml`), build Bun with a modified WebKit/JavaScriptCore using `bun run build:local`, and replace `kokoro-tts` in this runtime with the resulting executable. `cli.js` and `node_modules` run unchanged on it. LocalBase/Kokoro source is public under AGPL-3.0-or-later.
