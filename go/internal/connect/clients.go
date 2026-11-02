package connect

// Each client's fields come from its own current documentation, not from memory, and
// were last checked on Checked. Run marks the ones whose snippets have been run for real
// against a gateway (DESIGN §28.7). Setting it for another client means having run that
// client, and every other one says it was not (docs/connect.md asks for help with them).
//
// Marks: @BASE@ the /v1 URL, @ROOT@ the same without /v1, @MODEL@, @KEY@, @ENV@ the key's
// variable, @CONTEXT@ and @OUTPUT@ token budgets. A value goes inside quotes in every
// template, so escape can keep it a value.

// Checked is when these were last compared with each client's documentation.
const Checked = "2026-11-02"

const exportKey = `export @ENV@='@KEY@'`

func setKey() Snippet { return Snippet{Label: "Set your key", Language: "sh", Text: exportKey} }

var clientTemplates = []Client{
	{ID: "curl", Name: "curl", Group: GroupCode, Docs: "/openapi.json", Run: true, Snippets: []Snippet{
		setKey(),
		{Label: "Send a message", Language: "sh", Main: true, Text: `curl '@BASE@/chat/completions' \
  -H "Authorization: Bearer $@ENV@" \
  -H 'Content-Type: application/json' \
  -d '{"model": "@MODEL@", "messages": [{"role": "user", "content": "Say hello."}]}'
`},
	}},
	{ID: "python", Name: "Python (OpenAI SDK)", Group: GroupCode, Docs: "https://github.com/openai/openai-python", Run: true, Snippets: []Snippet{
		{Label: "Install it, and set your key", Language: "sh", Text: "pip install openai\n" + exportKey},
		{Label: "Send a message", Language: "python", Main: true, Text: `import os

from openai import OpenAI

client = OpenAI(
    base_url="@BASE@",
    api_key=os.environ["@ENV@"],
)

reply = client.chat.completions.create(
    model="@MODEL@",
    messages=[{"role": "user", "content": "Say hello."}],
)
print(reply.choices[0].message.content)
`},
	}, Note: "Use chat.completions. LocalBase has no Responses API."},
	{ID: "javascript", Name: "JavaScript (OpenAI SDK)", Group: GroupCode, Docs: "https://github.com/openai/openai-node", Run: true, Snippets: []Snippet{
		{Label: "Install it, and set your key", Language: "sh", Text: "npm install openai\n" + exportKey},
		{Label: "Send a message", Language: "javascript", Main: true, Text: `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "@BASE@",
  apiKey: process.env.@ENV@,
});

const reply = await client.chat.completions.create({
  model: "@MODEL@",
  messages: [{ role: "user", content: "Say hello." }],
});
console.log(reply.choices[0].message.content);
`},
	}, Note: "Run it on a server or in Node. A key is refused from a web page, by design. Use chat.completions: LocalBase has no Responses API."},

	{ID: "opencode", Name: "opencode", Group: GroupAgents, Docs: "https://opencode.ai/docs/providers/", Snippets: []Snippet{
		setKey(),
		{Label: "Add this provider", Path: "~/.config/opencode/opencode.json", Language: "json", Main: true, Text: `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "localbase": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "LocalBase",
      "options": {
        "baseURL": "@BASE@",
        "apiKey": "{env:@ENV@}"
      },
      "models": {
        "@MODEL@": {
          "name": "@MODEL@",
          "limit": { "context": @CONTEXT@, "output": @OUTPUT@ }
        }
      }
    }
  }
}
`},
	}, Note: "Merge this into your file if you already have one. Keep @ai-sdk/openai-compatible: opencode's other OpenAI adapter uses the Responses API."},
	{ID: "continue", Name: "Continue", Group: GroupAgents, Docs: "https://docs.continue.dev/customize/model-providers/top-level/openai", Snippets: []Snippet{
		{Label: "Add this model", Path: "~/.continue/config.yaml", Language: "yaml", Main: true, Text: `name: LocalBase
version: 0.0.1
schema: v1
models:
  - name: "@MODEL@"
    provider: openai
    model: "@MODEL@"
    apiBase: "@BASE@"
    apiKey: "@KEY@"
    defaultCompletionOptions:
      contextLength: @CONTEXT@
    roles: [chat, edit, apply]
`},
	}, Note: "This file holds your key, so keep it private. Merge the model into your file if you already have one."},
	{ID: "cline", Name: "Cline", Group: GroupAgents, Docs: "https://docs.cline.bot/provider-config/openai-compatible", Snippets: []Snippet{
		{Label: "In Cline's settings", Language: "text", Main: true, Fields: []Field{
			{"API Provider", "OpenAI Compatible"}, {"Base URL", "@BASE@"}, {"API Key", "@KEY@"}, {"Model ID", "@MODEL@"},
		}},
	}},
	{ID: "aider", Name: "Aider", Group: GroupAgents, Docs: "https://aider.chat/docs/llms/openai-compat.html", Snippets: []Snippet{
		setKey(),
		{Label: "Start it", Language: "sh", Main: true, Text: `export OPENAI_API_BASE='@BASE@'
export OPENAI_API_KEY="$@ENV@"
aider --model 'openai/@MODEL@'
`},
	}},
	{ID: "zed", Name: "Zed", Group: GroupAgents, Docs: "https://zed.dev/docs/ai/use-api-access#openai-compatible", Snippets: []Snippet{
		{Label: "Set your key, then start Zed from this shell", Language: "sh", Text: exportKey},
		{Label: "Add this provider", Path: "~/.config/zed/settings.json", Language: "json", Main: true, Text: `{
  "language_models": {
    "openai_compatible": {
      "localbase": {
        "api_url": "@BASE@",
        "available_models": [
          { "name": "@MODEL@", "display_name": "@MODEL@", "max_tokens": @CONTEXT@ }
        ]
      }
    }
  }
}
`},
	}, Note: "Zed reads the key from the variable named after this provider, or you can enter it in its provider settings. Do not put it in settings.json."},
	{ID: "goose", Name: "goose", Group: GroupAgents, Docs: "https://goose-docs.ai/docs/getting-started/providers", Snippets: []Snippet{
		setKey(),
		{Label: "Add this provider", Path: "~/.config/goose/custom_providers/localbase.json", Language: "json", Main: true, Text: `{
  "name": "localbase",
  "engine": "openai",
  "display_name": "LocalBase",
  "api_key_env": "@ENV@",
  "base_url": "@BASE@/chat/completions",
  "models": [{ "name": "@MODEL@", "context_limit": @CONTEXT@ }],
  "supports_streaming": true,
  "requires_auth": true
}
`},
	}},

	{ID: "open-webui", Name: "Open WebUI", Group: GroupApps, Docs: "https://docs.openwebui.com/getting-started/quick-start/connect-a-provider/starting-with-openai-compatible", Snippets: []Snippet{
		{Label: "In Admin Settings, under Connections, add an OpenAI connection", Language: "text", Main: true, Fields: []Field{
			{"URL", "@BASE@"}, {"API Key", "@KEY@"},
		}},
	}, Note: "It lists your models itself, from /v1/models. If it runs in Docker, 127.0.0.1 is the container itself: use an address the container can reach. Its browser-side direct connections cannot use a key."},
}

// Unusable says which well-known clients cannot work, and why, so nobody spends an
// evening finding out. Checked with the clients above.
var Unusable = []Limit{
	{Name: "Codex CLI", Why: "Its current versions only speak the Responses API, which LocalBase does not have.",
		Docs: "https://learn.chatgpt.com/docs/models"},
	{Name: "Cursor", Why: "It sends requests through its own servers, so they cannot reach an address on this computer. Its documentation forum also reports that Agent mode needs the Responses API.",
		Docs: "https://forum.cursor.com/t/cursor-cannot-connect-to-a-custom-url-compatible-with-the-openai-protocol/153906"},
}

// Limit is a client that cannot work.
type Limit struct {
	Name string `json:"name"`
	Why  string `json:"why"`
	Docs string `json:"docs"`
}
