package system

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// A real OpenTelemetry Collector (LOCALBASE_CONTRACT_OTELCOL: a pinned
// otelcol binary, core distribution) receives the gateway's export on a
// test port and writes what it parsed with its file exporter. The export
// is checked as the collector understood it, independent of this
// repository's decoder. Never the live collector on 127.0.0.1:4318.

const otelcolVersion = "0.163.1" // otelcol_0.163.1_darwin_arm64.tar.gz, sha256 664047e6…d665d8d3

func startOtelcol(t *testing.T, bin string) (endpoint, out string, stop func()) {
	t.Helper()
	if v, err := exec.Command(bin, "--version").Output(); err != nil || !strings.Contains(string(v), otelcolVersion) {
		t.Fatalf("otelcol %s required: %s %v", otelcolVersion, v, err)
	}
	dir := fsguard.ScratchDir(t)
	port := testutil.FreePort()
	out = filepath.Join(dir, "export.json")
	cfg := fmt.Sprintf(`receivers:
  otlp:
    protocols:
      http:
        endpoint: 127.0.0.1:%d
exporters:
  file:
    path: %s
service:
  telemetry:
    metrics:
      level: none
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [file]
    logs:
      receivers: [otlp]
      exporters: [file]
`, port, out)
	cfgPath := filepath.Join(dir, "config.yaml")
	fsguard.WriteFile(cfgPath, []byte(cfg), 0o600)
	cmd := exec.Command(bin, "--config", cfgPath)
	logf, _ := fsguard.Create(filepath.Join(dir, "otelcol.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop = func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		cmd.Wait()
		logf.Close()
		// The collector accepted everything: no errors, no rejected items.
		b, _ := os.ReadFile(logf.Name())
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "\terror\t") || strings.Contains(strings.ToLower(line), "rejected") {
				t.Errorf("otelcol: %s", line)
			}
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			stop()
			b, _ := os.ReadFile(logf.Name())
			t.Fatalf("otelcol did not start:\n%s", b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "http://127.0.0.1:" + strconv.Itoa(port), out, stop
}

// collectorJSON converts OTLP/JSON values (the collector's file output) to
// the decoded shape the other OTLP tests use.
func collectorJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 1 {
			for k, inner := range x {
				switch k {
				case "stringValue", "boolValue":
					return inner
				case "intValue":
					n, _ := strconv.ParseFloat(fmt.Sprint(inner), 64)
					return otlp.Number{Value: n, Int: true}
				case "doubleValue":
					n, _ := strconv.ParseFloat(fmt.Sprint(inner), 64)
					return otlp.Number{Value: n}
				}
			}
		}
		out := map[string]any{}
		for k, inner := range x {
			switch {
			case k == "attributes":
				attrs := map[string]any{}
				for _, e := range inner.([]any) {
					kv := e.(map[string]any)
					attrs[kv["key"].(string)] = collectorJSON(kv["value"])
				}
				out[k] = attrs
			case strings.HasSuffix(k, "UnixNano"):
				n, _ := strconv.ParseFloat(fmt.Sprint(inner), 64)
				out[k] = n
			default:
				out[k] = collectorJSON(inner)
			}
		}
		if _, ok := out["status"]; ok {
			st := out["status"].(map[string]any)
			if _, has := st["code"]; !has {
				st["code"] = 0.0
			}
		}
		return out
	case []any:
		for i := range x {
			x[i] = collectorJSON(x[i])
		}
		return x
	}
	return v
}

func readCollectorFile(t *testing.T, path string) *collector {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("collector output: %v", err)
	}
	defer f.Close()
	col := &collector{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var v map[string]any
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			continue
		}
		m := collectorJSON(v).(map[string]any)
		if _, ok := m["resourceSpans"]; ok {
			col.traces = append(col.traces, m)
		}
		if _, ok := m["resourceLogs"]; ok {
			col.logs = append(col.logs, m)
		}
	}
	return col
}

// TestRealCollector: the export reaches a real collector, which parses a server span for each
// request and the request records, and no export failure is reported. The trees themselves are
// pinned by TestDifferentialOTLP (until step 7 they were compared with TS's here too).
func TestRealCollector(t *testing.T) {
	bin := os.Getenv("LOCALBASE_CONTRACT_OTELCOL")
	if bin == "" {
		t.Skip("set LOCALBASE_CONTRACT_OTELCOL to a pinned otelcol binary")
	}
	endpoint, out, stop := startOtelcol(t, bin)
	run := runOTLPAt(t, "go", endpoint)
	time.Sleep(1500 * time.Millisecond) // the file exporter's flush
	stop()
	run.col = readCollectorFile(t, out)
	if len(run.col.spans()) == 0 || len(run.col.records()) == 0 {
		t.Fatalf("the collector wrote no spans or records")
	}
	for _, e := range localEvents(t, run.root) {
		if strings.HasPrefix(fmt.Sprint(e["eventName"]), "observability.export") {
			t.Errorf("export failure against a real collector: %v", e)
		}
	}
	for _, label := range []string{"models", "unmatched", "missing", "invalid"} {
		if run.tree(run.requests[label]) == nil {
			t.Errorf("%s: no server span as the collector parsed it", label)
		}
	}
	run.checkRecords(t, "go")
}
