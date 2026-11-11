package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// forgetBindingIssuers makes the policy's bindings look like ones from before they named a provider.
func forgetBindingIssuers(t *testing.T, dir string) {
	t.Helper()
	r, err := root.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB.Exec(`DELETE FROM go_binding_issuers`); err != nil {
		t.Fatal(err)
	}
}

// What each command under `keys` and `access` is, for the audit log. A new command fails
// TestEveryAccessCommandIsClassified until it is here.
var auditClasses = map[string]string{
	"keys list": "read", "keys create": "audited", "keys revoke": "audited", "keys rotate": "audited", "keys scopes": "audited",
	"access show": "read", "access cloudflare": "audited",
	"access oidc add": "audited", "access oidc list": "read", "access oidc remove": "audited",
	"access github list": "read",
	// GitHub registrations can't be added any more; this is removeRegistration, which `access oidc remove` runs here.
	"access github remove":   "audited-same-code",
	"access magic-link add":  "audited",
	"access magic-link list": "read", "access magic-link remove": "audited",
	"access disable": "audited",
	// Deliberately not audited: it changes nothing, as on the route.
	"access email test": "not-a-change", "access policy test": "not-a-change",
	"access email show": "read", "access email configure": "audited", "access email disable": "audited",
	"access policy show": "read", "access policy apply": "audited", "access policy starter": "audited", "access policy clear": "audited",
	"access bindings adopt": "audited",
	"access users list":     "read", "access users invite": "audited", "access users roles": "audited",
	"access users enable": "audited", "access users disable": "audited", "access users remove": "audited",

	"config validate": "read", "config plan": "read", "config show": "read", "config apply": "audited",
	"configure": "audited", "init": "not-a-change", "reset": "audited",
	"doctor": "read", "status": "read", "logs": "read", "diagnostics": "read", "version": "read",
	"models catalog": "read", "models recommend": "read", "models list": "read", "models show": "read",
	"models search": "read", "models inspect": "read", "models export": "read", "models feed refresh": "read", "models feed status": "read",
	"models feed use": "audited", "models backend": "audited", "models placement": "audited", "models delete": "audited",
	"models import": "audited", "models add-definition": "audited", "models forget-peaks": "audited",
	// These need a download from a real host, which this test has no stand-in for.
	"models install": "audited-not-driven", "models add": "audited-not-driven", "models remove-entry": "audited-not-driven",
	"models cancel": "audited-elsewhere", // the gateway writes it
	"backends list": "read", "backends show": "read", "backends add": "audited", "backends remove": "audited",
	"auth-profiles list": "read", "auth-profiles add": "audited", "auth-profiles remove": "audited",
	"voices list": "read", "voices add": "audited", "voices remove": "audited",
	"speech marking": "audited",
	"usage":          "read", "usage clear": "audited", "usage retention": "audited", "usage recording": "audited",
	"connect":               "audited-elsewhere", // keys create's line; TestConnectLogsTheKeyItCreatesAsKeysCreateDoes
	"backup":                "audited-elsewhere", // backup.created
	"restore":               "audited-elsewhere", // backup.restored
	"smoke":                 "audited-temporary-key",
	"upgrade":               "audited-temporary-key",
	"upgrade check-updates": "not-a-change",
	// Deliberately not audited: it runs the gateway, and doesn't change it.
	"serve": "not-a-change",
	"start": "audited-elsewhere", "stop": "audited-elsewhere", "restart": "audited-elsewhere", // audit.service-*
	"uninstall":        "not-a-change",
	"upgrade rollback": "audited-elsewhere", // audit.upgrade-rolled-back, in the contract test
	// These write their own lines, under other event names, already.
	"access login-link": "audited-elsewhere", "access sessions list": "read", "access sessions revoke": "audited-elsewhere",
}

func leafCommands(prefix string, c *cli.Command) []string {
	var out []string
	if len(c.Sub) == 0 || c.Run != nil {
		out = append(out, prefix)
	}
	for _, sub := range c.Sub {
		out = append(out, leafCommands(prefix+" "+sub.Name, sub)...)
	}
	return out
}

func TestEveryCommandIsClassified(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range app().Root.Sub {
		for _, path := range leafCommands(c.Name, c) {
			seen[path] = true
			if auditClasses[path] == "" {
				t.Errorf("%q is not in auditClasses: say whether it changes anything, and audit it if it does", path)
			}
		}
	}
	for path := range auditClasses {
		if !seen[path] {
			t.Errorf("auditClasses names %q, which is not a command", path)
		}
	}
}

type auditLine struct {
	Event, Action, ActorKind, TargetKind, TargetID string
	Scopes, Purpose, Setting, Value                string
}

// auditLines reads the root's audit log the way a person would.
func auditLines(t *testing.T, root string) []auditLine {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "logs", "audit.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []auditLine
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ev struct {
			EventName  string         `json:"eventName"`
			Attributes map[string]any `json:"attributes"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("%q: %v", sc.Text(), err)
		}
		// Ids and permission names only: anything else needs a reason to be here. The sink also
		// redacts what looks like a key, but that is a second line of defence, and this the first.
		// setting and value: an import opt-in that configure changed, and to what (on or off).
		for k := range ev.Attributes {
			if !slices.Contains([]string{"action", "actor_kind", "actor_id", "target_kind", "target_id", "backend_id", "scopes", "count", "purpose", "setting", "value"}, k) {
				t.Errorf("an audit line has %q: %s", k, sc.Text())
			}
		}
		get := func(k string) string { s, _ := ev.Attributes[k].(string); return s }
		out = append(out, auditLine{ev.EventName, get("action"), get("actor_kind"), get("target_kind"), get("target_id"), get("scopes"), get("purpose"), get("setting"), get("value")})
	}
	return out
}

type auditEnv struct {
	t       *testing.T
	root    string
	secrets map[string]string // what must never be logged, by what it is
}

// run runs one command, and returns its data.
func (e *auditEnv) run(args ...string) map[string]any {
	e.t.Helper()
	code, out := runApp(e.t, append([]string{"--root", e.root, "--json"}, args...)...)
	if code != 0 {
		e.t.Fatalf("%v: %d %s", args, code, out)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	// The envelope is the output's last line: anything before it went to stderr.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		e.t.Fatalf("%v: %v", args, err)
	}
	return env.Data
}

// Each command that changes who can do what writes one line saying so, in the words the
// route for the same change uses, and nothing that logs a secret.
func TestAccessCommandsAreAudited(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	const (
		clientSecret = "PLANTED_oidc_client_secret_5c1f0a77e2"
		smtpPassword = "PLANTED_smtp_password_91ae44d07b"
		email        = "planted-person@example.com"
		invited      = "planted-invited@example.com"
		origin       = "https://lb.example"
		issuer       = "https://identity.example"
	)
	t.Setenv("AUDIT_TEST_CLIENT_SECRET", clientSecret)
	t.Setenv("AUDIT_TEST_SMTP_PASSWORD", smtpPassword)
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	e := &auditEnv{t: t, root: root, secrets: map[string]string{"OIDC client secret": clientSecret, "SMTP password": smtpPassword}}

	var keyID string
	var userID string
	steps := []struct {
		command    string // the name in auditClasses
		args       func() []string
		action     string
		targetKind string
		target     func() string // "" when the id isn't known beforehand: any non-empty id
		after      func(data map[string]any)
	}{
		{"keys create", func() []string { return []string{"keys", "create", "--name", "k", "--scopes", "models:read"} }, "create", "api-key", nil, func(d map[string]any) {
			keyID = d["key"].(map[string]any)["id"].(string)
			e.secrets["a new key"] = d["secret"].(string)
		}},
		{"keys scopes", func() []string { return []string{"keys", "scopes", keyID, "--scopes", "models:read,inference:chat"} }, "set-scopes", "api-key", func() string { return keyID }, nil},
		{"keys rotate", func() []string { return []string{"keys", "rotate", keyID} }, "rotate", "api-key", func() string { return keyID }, func(d map[string]any) {
			e.secrets["a rotated key's new secret"] = d["secret"].(string)
		}},
		{"keys revoke", func() []string { return []string{"keys", "revoke", keyID} }, "revoke", "api-key", func() string { return keyID }, nil},
		{"access cloudflare", func() []string {
			return []string{"access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "planted-audience", "--origin", origin}
		}, "configure-cloudflare", "provider", func() string { return "browser-access" }, nil},
		{"access disable", func() []string { return []string{"access", "disable"} }, "disable", "provider", func() string { return "browser-access" }, nil},
		{"access oidc add", func() []string {
			return []string{"access", "oidc", "add", "--id", "idp", "--name", "Provider", "--issuer", issuer, "--client-id", "client", "--client-secret-env", "AUDIT_TEST_CLIENT_SECRET", "--origin", origin}
		}, "upsert-oidc", "registration", func() string { return "idp" }, nil},
		{"access email configure", func() []string {
			return []string{"access", "email", "configure", "--host", "smtp.example", "--port", "587", "--security", "starttls", "--username", "user", "--password-env", "AUDIT_TEST_SMTP_PASSWORD", "--from", "sender@example.com"}
		}, "configure-email-delivery", "email-delivery", func() string { return "smtp" }, nil},
		{"access magic-link add", func() []string {
			return []string{"access", "magic-link", "add", "--id", "email", "--name", "Email", "--origin", origin}
		}, "upsert-magic-link", "registration", func() string { return "email" }, nil},
		{"access magic-link remove", func() []string { return []string{"access", "magic-link", "remove", "email"} }, "remove-registration", "registration", func() string { return "email" }, nil},
		{"access policy starter", func() []string { return []string{"access", "policy", "starter", "--email", email, "--issuer", issuer} }, "apply-policy", "policy", nil, nil},
		{"access users invite", func() []string {
			return []string{"access", "users", "invite", "--email", invited, "--roles", "admin", "--issuer", issuer}
		}, "invite-user", "user", nil, func(d map[string]any) {
			userID = d["user"].(map[string]any)["id"].(string)
		}},
		{"access users roles", func() []string { return []string{"access", "users", "roles", "--email", invited, "--roles", ""} }, "replace-user-roles", "user", func() string { return userID }, nil},
		{"access users disable", func() []string { return []string{"access", "users", "disable", "--email", invited} }, "disable-user", "user", func() string { return userID }, nil},
		{"access users enable", func() []string { return []string{"access", "users", "enable", "--email", invited} }, "enable-user", "user", func() string { return userID }, nil},
		{"access users remove", func() []string { return []string{"access", "users", "remove", "--email", invited} }, "remove-user", "user", func() string { return userID }, nil},
		{"access policy clear", func() []string { return []string{"access", "policy", "clear"} }, "clear-policy", "policy", func() string { return "" }, nil},
		{"access policy apply", func() []string {
			policy, err := json.Marshal(auth.StarterPolicy(email, issuer))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(policyFile, policy, 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"access", "policy", "apply", "--file", policyFile}
		}, "apply-policy", "policy", nil, nil},
		{"access bindings adopt", func() []string {
			forgetBindingIssuers(t, root)
			return []string{"access", "bindings", "adopt", "--issuer", issuer}
		}, "adopt-binding-issuer", "issuer", func() string { return issuer }, nil},
		{"access email disable", func() []string { return []string{"access", "email", "disable"} }, "disable-email-delivery", "email-delivery", func() string { return "smtp" }, nil},
		{"access oidc remove", func() []string { return []string{"access", "oidc", "remove", "idp"} }, "remove-registration", "registration", func() string { return "idp" }, nil},
	}
	ran := map[string]bool{}
	for _, s := range steps {
		before := len(auditLines(t, root))
		data := e.run(s.args()...)
		if s.after != nil {
			s.after(data)
		}
		ran[s.command] = true
		if _, ok := data["auditWritten"]; ok {
			t.Errorf("%s: auditWritten is only for a failure: %v", s.command, data)
		}
		lines := auditLines(t, root)
		if len(lines) != before+1 {
			t.Errorf("%s: %d new audit lines, want 1", s.command, len(lines)-before)
			continue
		}
		got := lines[len(lines)-1]
		area := "access"
		if strings.HasPrefix(s.command, "keys") {
			area = "api-key"
		}
		if got.Event != "audit."+area+"-changed" || got.Action != s.action || got.ActorKind != "cli" || got.TargetKind != s.targetKind {
			t.Errorf("%s: %+v", s.command, got)
		}
		if s.target != nil && got.TargetID != s.target() || s.target == nil && got.TargetID == "" {
			t.Errorf("%s: target id %q", s.command, got.TargetID)
		}
		if area == "api-key" && got.Scopes == "" {
			t.Errorf("%s: no scopes in %+v", s.command, got)
		}
	}
	for command := range auditClasses {
		if auditClasses[command] == "audited" && !ran[command] && (strings.HasPrefix(command, "keys") || strings.HasPrefix(command, "access")) {
			t.Errorf("%s is classified as audited, and nothing here shows it", command)
		}
	}

	// Nothing a person would mind being in a log is in any of them.
	e.secrets["an email address"] = email
	e.secrets["an invited address"] = invited
	entries, err := os.ReadDir(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no logs to search")
	}
	for _, entry := range entries {
		b, err := os.ReadFile(filepath.Join(root, "logs", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for what, value := range e.secrets {
			if strings.Contains(string(b), value) {
				t.Errorf("%s holds %s", entry.Name(), what)
			}
		}
	}
}

// A record that can't be written doesn't stop the command, and doesn't go unmentioned: on
// stderr, and in the result for a program reading --json.
func TestAnAuditRecordThatCannotBeWrittenIsMentioned(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	for _, tc := range []struct {
		name, class string
		breakIt     func(t *testing.T, root string)
	}{
		{"the logs folder is a file", "not-a-file", func(t *testing.T, root string) {
			logs := filepath.Join(root, "logs")
			if err := os.RemoveAll(logs); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(logs, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"only the audit file can't be opened", "write-failed", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "logs", "audit.jsonl")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "logs", "audit.jsonl"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(tmp, "root")
			if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
				t.Fatalf("configure: %s", out)
			}
			changed := changedConfig(t, root, tmp)
			tc.breakIt(t, root)
			// A keys command and an access command: the result is added in one place, for all of them.
			// models placement and config apply return a struct, not a map, so each result carries the
			// outcome itself (audit.Log.Outcome, embedded as audit.Outcome).
			for _, args := range [][]string{{"keys", "create", "--name", "k"}, {"access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "a", "--origin", "https://lb.example"},
				{"models", "placement", "qwen3.5-4b-q4_k_m", "--reset"}, {"config", "apply", "--file", changed, "--restart", "never"}} {
				var stdout, stderr bytes.Buffer
				a := app()
				a.Stdout, a.Stderr = &stdout, &stderr
				if code := a.Run(append([]string{"--root", root, "--json"}, args...)); code != 0 {
					t.Errorf("%v: the change was made, so the command should succeed: %d %s", args, code, stderr.String())
				}
				if want := "Warning: this change was made, but its audit record could not be written: " + tc.class + " (" + filepath.Join(root, "logs") + ")\n"; !strings.Contains(stderr.String(), want) {
					t.Errorf("%v: stderr %q, want %q", args, stderr.String(), want)
				}
				var env struct {
					OK   bool           `json:"ok"`
					Data map[string]any `json:"data"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
					t.Fatal(err)
				}
				if !env.OK || env.Data["auditWritten"] != false || env.Data["auditError"] != tc.class {
					t.Errorf("%v: auditWritten %v, auditError %v, ok %v", args, env.Data["auditWritten"], env.Data["auditError"], env.OK)
				}
			}
		})
	}
}

// allowedActorKinds is every value actor_kind has in the audit log: a principal's kind, or
// `cli` for the command line and `local` for whoever redeemed a local sign-in link, who has
// no session yet.
var allowedActorKinds = []string{"anonymous", "api-key", "browser-session", "cli", "environment", "local"}

// actorKindSpellings finds each literal in a directory's code that becomes an actor kind: the
// value after an "actor_kind" attribute, a variable named for an actor, a Principal's Kind.
func actorKindSpellings(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	note := func(e ast.Expr) {
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil && v != "" { // "" is a variable's starting value
				found[v] = fset.Position(lit.Pos()).String()
			}
		}
	}
	isActor := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && (id.Name == "actor" || id.Name == "actorKind")
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				for i := 0; i+1 < len(n.Args); i++ {
					if lit, ok := n.Args[i].(*ast.BasicLit); ok && lit.Value == `"actor_kind"` {
						note(n.Args[i+1])
					}
				}
			case *ast.CompositeLit:
				for i := 0; i+1 < len(n.Elts); i++ {
					if lit, ok := n.Elts[i].(*ast.BasicLit); ok && lit.Value == `"actor_kind"` {
						note(n.Elts[i+1])
					}
				}
				typeName := ""
				switch ty := n.Type.(type) {
				case *ast.Ident:
					typeName = ty.Name
				case *ast.SelectorExpr:
					typeName = ty.Sel.Name
				}
				if typeName == "Principal" {
					for _, elt := range n.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Kind" {
								note(kv.Value)
							}
						}
					}
				}
			case *ast.AssignStmt:
				for i, l := range n.Lhs {
					if isActor(l) && i < len(n.Rhs) {
						note(n.Rhs[i])
					}
				}
			}
			return true
		})
	}
	return found
}

// One name for each kind of actor, so that a search of the log finds them all. The gateway is read
// with every package under it, at least its 9 (gatewayPackages in internal/gateway). internal/audit
// writes the CLI's records, with actor_kind cli.
func TestActorKindsAreOneVocabulary(t *testing.T) {
	seen := map[string]bool{}
	var dirs []string
	for _, root := range []struct {
		dir      string
		packages int
	}{{".", 1}, {"../../internal/gateway", 9}, {"../../internal/auth", 1}, {"../../internal/audit", 1}} {
		for _, f := range testutil.GoSources(t, root.dir, root.packages) {
			if dir := filepath.Dir(f); !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	for _, dir := range dirs {
		for kind, where := range actorKindSpellings(t, dir) {
			seen[kind] = true
			if !slices.Contains(allowedActorKinds, kind) {
				t.Errorf("%s: actor kind %q is not in allowedActorKinds", where, kind)
			}
		}
	}
	for _, kind := range allowedActorKinds {
		if !seen[kind] {
			t.Errorf("allowedActorKinds has %q, and no code uses it (or the search no longer finds it)", kind)
		}
	}

	// The search finds each way of spelling one, or this test says nothing.
	dir := t.TempDir()
	planted := `package p
func f(log func(...any)) {
	log("event", "x", "actor_kind", "planted-one")
	actorKind, id := "planted-two", ""
	_ = Principal{Kind: "planted-three"}
	_, _ = actorKind, id
}
type Principal struct{ Kind string }
`
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	got := actorKindSpellings(t, dir)
	for _, kind := range []string{"planted-one", "planted-two", "planted-three"} {
		if got[kind] == "" {
			t.Errorf("the search missed %q: %v", kind, got)
		}
	}
}
