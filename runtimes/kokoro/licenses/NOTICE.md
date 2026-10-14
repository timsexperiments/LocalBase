# Corresponding source

The runtime includes `phonemizer` 1.2.1, whose exact source is [xenova/phonemizer.js at commit `6835144b7ee9043129222549c1ed2f6a27216278`](https://github.com/xenova/phonemizer.js/tree/6835144b7ee9043129222549c1ed2f6a27216278). Its package build command is `npm run build`. That source contains generated eSpeak-ng WASM and data, but does not identify the eSpeak-ng revision or document how those artifacts were built. We cannot establish the exact eSpeak-ng source revision from the available upstream materials.

For three years from distribution, the LocalBase maintainers will provide corresponding source for covered components on request: [LocalBase maintainers](https://github.com/timsexperiments/LocalBase/issues).

Bun 1.3.14 statically links WebKit/JavaScriptCore from commit [`5488984d20e0dbfe4be2c3ba8fb18eb81a5e0e8b`](https://github.com/oven-sh/WebKit/tree/5488984d20e0dbfe4be2c3ba8fb18eb81a5e0e8b). See [Bun's license notice](https://raw.githubusercontent.com/oven-sh/bun/0d9b296af33f2b851fcbf4df3e9ec89751734ba4/LICENSE.md) for its license details and relinking instructions. LocalBase/Kokoro source is public under AGPL-3.0-or-later, allowing users to relink by rebuilding with a modified Bun.
