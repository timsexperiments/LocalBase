package contract

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// On the host too: registering, removing and choosing a backend are on record, by id. The
// command a backend runs is not logged, since a path can hold a name.
func TestBackendChangesFromTheCommandLineAreAudited(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	fake := filepath.Join(tmp, "fakebackend")
	if out, err := exec.Command("go", "build", "-o", fake, "../internal/runtime/fakebackend").CombinedOutput(); err != nil {
		t.Fatalf("build fakebackend: %v %s", err, out)
	}
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "init"); r.code != 0 {
		t.Fatalf("init: %s%s", r.stdout, r.stderr)
	}
	def, _ := json.Marshal(map[string]any{"schema": "localbase.backend/v1", "id": "audit-fake", "name": "Audit fake", "engine": "fake-engine",
		"adapter": "process-openai/v1", "tasks": []string{"chat"}, "formats": []string{"gguf"}, "capabilities": []string{},
		"architectures": map[string]any{"supported": []string{"audittest"}},
		"platforms":     []any{map[string]any{"os": goruntime.GOOS, "arch": map[string]string{"arm64": "arm64", "amd64": "x64"}[goruntime.GOARCH], "accelerator": "cpu"}},
		"launch": map[string]any{"command": fake, "args": []string{"--host", "{host}", "--port", "{port}", "-m", "{model}"}, "healthPath": "/health",
			"memory": map[string]any{"weightsFactor": 1, "overheadGb": 0, "assumption": "a fake backend holds next to nothing"}}})
	defPath := filepath.Join(tmp, "backend.json")
	if err := mustWriteFile(defPath, def, 0o600); err != nil {
		t.Fatal(err)
	}
	e.json("backends", "add", "--file", defPath)
	const model = "qwen3.6-35b-a3b-iq4_nl_xl"
	e.json("models", "backend", model, "--set", "audit-fake")
	e.json("models", "backend", model, "--auto")
	e.json("models", "backend", model) // only looks
	e.json("backends", "remove", "audit-fake")
	// Choosing one while importing counts too. Automatic isn't a choice.
	file := filepath.Join(tmp, "tiny.gguf")
	if err := mustWriteFile(file, []byte("GGUF not really"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.cli(nil, "models", "import", file, "--id", "chosen", "--task", "chat", "--backend", "llama.cpp-cpu", "--yes"); r.code != 0 {
		t.Fatalf("import: %s%s", r.stdout, r.stderr)
	}
	if r := e.cli(nil, "models", "import", file, "--id", "automatic", "--task", "chat", "--backend", "auto", "--yes"); r.code != 0 {
		t.Fatalf("import: %s%s", r.stdout, r.stderr)
	}
	if r := e.cli(nil, "models", "backend", model, "--set", "no-such-backend"); r.code == 0 {
		t.Errorf("an unknown backend was accepted: %s", r.stdout)
	}
	if r := e.cli(nil, "backends", "remove", "no-such-backend"); r.code != 0 {
		t.Errorf("removing what isn't there: %s%s", r.stdout, r.stderr)
	}
	b, err := os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		name, _ := m["eventName"].(string)
		if !strings.HasPrefix(name, "audit.") {
			continue
		}
		attrs, _ := m["attributes"].(map[string]any)
		get := func(k string) string {
			if s, ok := m[k].(string); ok {
				return s
			}
			s, _ := attrs[k].(string)
			return s
		}
		if get("actor_kind") != "cli" {
			t.Errorf("no actor on %s: %s", name, line)
		}
		seen = append(seen, name+" "+get("action")+" "+get("target_id")+" "+get("backend_id"))
	}
	want := []string{"audit.backend-registered add audit-fake audit-fake",
		"audit.model-backend-changed set " + model + " audit-fake",
		"audit.model-backend-changed reset " + model + " ",
		"audit.backend-removed remove audit-fake audit-fake",
		"audit.model-backend-changed import chosen llama.cpp-cpu"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Errorf("audit events:\n got %q\nwant %q", seen, want)
	}
	if strings.Contains(string(b), fake) {
		t.Error("the command a backend runs reached the log")
	}
}
