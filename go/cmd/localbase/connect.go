package main

import (
	"context"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/connect"
	"github.com/timsexperiments/LocalBase/go/internal/instance"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
)

// connectScopes are what --create-key grants by default: enough for an editor or an
// agent, and no more. The same one tap as the page (DESIGN §30.5).
var connectScopes = []string{"inference:chat", "models:read"}

func connectCommand() *cli.Command {
	return &cli.Command{Name: "connect", Summary: "Show how to point an app at this gateway", Usage: "[client]", MaxArgs: 1,
		Options: []cli.Option{
			{Name: "model", Value: "<id>", Help: "Model the snippets use; defaults to the default chat model"},
			{Name: "base-url", Value: "<url>", Help: "Address the snippets use, for a proxy LocalBase cannot know about"},
			{Name: "list", Bool: true, Help: "List the clients and exit"},
			{Name: "create-key", Bool: true, Help: "Create a key for this app, and fill it in"},
			{Name: "name", Value: "<label>", Help: "The new key's label; defaults to connect-<client>"},
			{Name: "scopes", Value: "<permission,...>", Help: "The new key's complete permissions; defaults to chat and listing models"},
			{Name: "expires-days", Value: "<days>", Help: "The new key's expiry"},
		},
		Examples: []string{
			"local-base connect",
			"local-base connect python --create-key",
			"local-base connect opencode > opencode.json",
		},
		Run: withEnv(runConnect)}
}

func runConnect(c *cli.Ctx, e *env) (any, error) {
	client := ""
	if len(c.Args) == 1 {
		client = c.Args[0]
		if !slices.Contains(connect.IDs(), client) {
			return nil, cli.Inputf("Unknown client %q. Expected one of: %s", client, strings.Join(connect.IDs(), ", "))
		}
	}
	if !c.Has("create-key") {
		for _, name := range []string{"name", "scopes", "expires-days"} {
			if c.Has(name) {
				return nil, cli.Inputf("--%s only applies with --create-key", name)
			}
		}
	}
	addr, running := gatewayListen(e)
	bases := connectBases(e, addr)
	baseURL := bases[0].URL
	if c.Has("base-url") {
		u, err := connect.NormalizeBaseURL(c.String("base-url"))
		if err != nil {
			return nil, cli.Inputf("base-url: %s", err.Error())
		}
		baseURL = u
	}
	models := connectModels(e)
	model := connectDefaultModel(e, models)
	if c.Has("model") {
		model = c.String("model")
		if !slices.Contains(models, model) {
			if len(models) == 0 {
				return nil, cli.Inputf("model: no chat model is installed yet. Run: local-base models install <id>")
			}
			return nil, cli.Inputf("model: %q is not an installed chat model. Expected one of: %s", model, strings.Join(models, ", "))
		}
	}
	contextTokens, planned := connectContext(e, model)
	target := connect.Target{BaseURL: baseURL, Model: model, Context: contextTokens}

	var key map[string]any
	if c.Has("create-key") {
		name := c.String("name")
		if !c.Has("name") {
			name = "connect"
			if client != "" {
				name += "-" + client
			}
		}
		scopes := connectScopes
		if c.Has("scopes") {
			var err error
			if scopes, err = parseScopes(c.String("scopes")); err != nil {
				return nil, err
			}
		}
		days, given, err := c.Int("expires-days")
		if err != nil || (given && days <= 0) {
			return nil, cli.Inputf("expiresDays: must be a positive integer")
		}
		k, raw, err := e.Store.CreateAPIKeyExpiring(context.Background(), name, scopes, days)
		if err != nil {
			return nil, err
		}
		e.audit.KeyChange("create", k) // the line keys create writes: with actor_id and the key's scopes
		target.Key = raw
		key = auth.PublicKey(k)
		// The secret goes out once: in the snippets, or in the envelope.
		c.Error("Created key %s (%s). The secret below is not shown again.", k.ID, k.Name)
	}

	clients := connect.Clients(target)
	if client != "" {
		clients = []connect.Client{*connect.Find(client, target)}
	}
	data := map[string]any{"baseUrls": bases, "baseUrl": baseURL, "model": model, "models": models,
		"gateway": map[string]any{"running": running}, "clients": clients, "unusable": connect.Unusable}
	if contextTokens > 0 {
		source := "gateway"
		if planned {
			source = "planned"
		}
		data["context"] = map[string]any{"tokens": contextTokens, "source": source}
	}
	if key != nil {
		data["key"], data["secret"] = key, target.Key
	}
	if c.Global.JSON {
		return data, nil
	}
	if client != "" {
		printClient(c, clients[0], target.Key != "")
		if contextTokens > 0 && planned && clients[0].UsesContext {
			c.Error("\nContext in these snippets: %d tokens, as planned without a running gateway.", contextTokens)
		}
		return data, nil
	}
	printOverview(c, bases, baseURL, model, running, target.Key, contextTokens, planned)
	return data, nil
}

// printClient puts the snippet that does the job on stdout, so it can go into a file,
// and everything around it on stderr.
func printClient(c *cli.Ctx, client connect.Client, hasKey bool) {
	for _, s := range client.Snippets {
		if s.Main {
			continue
		}
		c.Error("# %s\n%s\n", s.Label, cli.Lines(strings.TrimRight(s.Text, "\n")))
	}
	for _, s := range client.Snippets {
		if !s.Main {
			continue
		}
		where := s.Label
		if s.Path != "" {
			where += " in " + s.Path
		}
		c.Error("# %s", where)
		c.Out("%s", cli.Lines(strings.TrimRight(s.Text, "\n")))
	}
	if client.Note != "" {
		c.Error("\n%s", client.Note)
	}
	if !client.Run {
		c.Error("\n%s.", connect.NotRunLabel)
	}
	if !hasKey && slices.ContainsFunc(client.Snippets, func(s connect.Snippet) bool { return strings.Contains(s.Text, connect.KeyToken) }) {
		c.Error("\nGet a key: local-base connect %s --create-key", client.ID)
	}
	c.Error("\nIts documentation: %s", termsafe.PrintableASCII(client.Docs))
}

func printOverview(c *cli.Ctx, bases []connect.Base, baseURL, model string, running bool, key string, contextTokens int, planned bool) {
	if running {
		c.Info("LocalBase is running.")
	} else {
		c.Info("LocalBase is not running. Start it with: local-base start")
	}
	c.Info("")
	if baseURL != bases[0].URL {
		c.Info("Address: %s", termsafe.PrintableASCII(baseURL))
	}
	for _, b := range bases {
		c.Info("%s: %s", b.Label, termsafe.PrintableASCII(b.URL))
		if b.Note != "" {
			c.Info("  %s", b.Note)
		}
	}
	if model != "" {
		c.Info("Model: %s", model)
		switch {
		case contextTokens > 0 && planned:
			c.Info("Context: %d tokens, as planned without a running gateway", contextTokens)
		case contextTokens > 0:
			c.Info("Context: %d tokens", contextTokens)
		}
	} else {
		c.Info("Model: none installed yet. Run: local-base models catalog")
	}
	if key != "" {
		c.Info("Key: %s", key)
		c.Info("  Store this secret now. It is not shown again.")
	} else {
		c.Info("Key: local-base connect --create-key, or local-base keys create --name my-app --scopes %s", strings.Join(connectScopes, ","))
	}
	c.Info("")
	c.Info("Snippets for your app: local-base connect <client>")
	c.Info("  %s", strings.Join(connect.IDs(), ", "))
	c.Info("")
	c.Info("LocalBase serves chat completions, not /v1/responses, /v1/completions or /v1/messages. These will not work:")
	for _, l := range connect.Unusable {
		c.Info("  %s: %s", l.Name, l.Why)
	}
}

// connectBases reads the addresses the same way the page does.
func connectBases(e *env, addr string) []connect.Base {
	origin, edge := "", false
	if a, err := auth.LoadAccess(e.Root.Dir); err == nil && a != nil {
		origin, edge = a.Origin, a.Provider.Kind == "cloudflare-access"
	}
	bases := connect.BaseURLs(addr, origin, edge)
	if len(bases) == 0 { // the configured host and port are always an address; this guards a bad row
		bases = []connect.Base{{Label: "This computer", URL: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(e.Config.GatewayPort)) + "/v1"}}
	}
	return bases
}

// connectModels are the installed chat models (library.ChatCandidates that are ready): catalog
// ones, then imports.
func connectModels(e *env) []string {
	lib, err := library.Open(e.Root, e.Store, e.Catalog, nil)
	if err != nil {
		lib = nil // the catalog ones are still listed
	}
	var out []string
	for _, c := range library.ChatCandidates(context.Background(), e.Config, e.Catalog, e.Root.ModelsDir("llm"), lib) {
		if c.Ready {
			out = append(out, c.ID)
		}
	}
	return out
}

// connectDefaultModel is the running gateway's effective default, else the active model,
// else the first installed one.
func connectDefaultModel(e *env, models []string) string {
	if d := gatewayDefaultModel(e); d != nil && slices.Contains(models, d.ModelID) {
		return d.ModelID
	}
	if a := e.Config.Active["llm"]; slices.Contains(models, a) {
		return a
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}

// connectContext is the context the model runs with here, or 0 when it cannot be worked out, and
// whether it is a plan made here rather than the running gateway's. With a gateway running it is the
// gateway's figure, which counts its --ctx-size and placements. Without one it is as planned without a
// running gateway: a plan made now through the library, the same plan the page uses, imported models
// included, which a gateway started with other flags could change.
func connectContext(e *env, model string) (tokens int, planned bool) {
	if model == "" {
		return 0, false
	}
	if gw, _, _ := instance.Dial(e.Root); gw != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if a, err := gw.Get(ctx, "/_localbase/model-context?model="+url.QueryEscape(model), nil); err == nil && a.Status == 200 {
			var v struct {
				ContextLength int `json:"contextLength"`
			}
			if a.Decode(&v) == nil && v.ContextLength > 0 {
				return v.ContextLength, false
			}
		}
	}
	lib, err := openLibrary(e)
	if err != nil {
		return 0, false
	}
	llm, _, _ := placementLLM(e, lib)
	defer llm.Close()
	plan, err := llm.Plan(model)
	if err != nil || plan == nil {
		return 0, false
	}
	return plan.Budget.EffectiveContext, true
}
