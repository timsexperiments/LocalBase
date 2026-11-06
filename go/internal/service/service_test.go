package service

import (
	"strings"
	"testing"
)

func TestParseLaunchctl(t *testing.T) {
	out := "gui/501/com.localbase.gateway.x = {\n\tactive count = 1\n\tstate = running\n\tpid = 4242\n\tlast exit code = (never exited)\n\tendpoints = {\n\t\tstate = nested\n\t}\n}\n"
	state, pid, last, err := parseLaunchctl(out)
	if err != nil || state != "running" || pid != 4242 || last != nil {
		t.Fatalf("%q %d %v %v", state, pid, last, err)
	}
	if _, _, _, err := parseLaunchctl("x = {\n\tstate = running\n\tstate = waiting\n}\n"); err == nil {
		t.Error("duplicate state accepted")
	}
	if _, _, _, err := parseLaunchctl("x = {\n\tstate = running\n"); err == nil {
		t.Error("unbalanced braces accepted")
	}
	_, _, last, _ = parseLaunchctl("x = {\n\tstate = waiting\n\tlast exit code = 78\n}\n")
	if last == nil || *last != 78 {
		t.Errorf("last exit code %v", last)
	}
}

func TestParseLaunchdEnabled(t *testing.T) {
	out := "disabled services = {\n\t\"com.localbase.gateway.a\" => enabled\n\t\"com.localbase.gateway.b\" => disabled\n}\n"
	if e := parseLaunchdEnabled(out, "com.localbase.gateway.a"); e == nil || !*e {
		t.Error("a")
	}
	if e := parseLaunchdEnabled(out, "com.localbase.gateway.b"); e == nil || *e {
		t.Error("b")
	}
	if parseLaunchdEnabled(out, "other") != nil {
		t.Error("other")
	}
}

func TestRenderSystemd(t *testing.T) {
	m := Metadata{ServiceID: "com.localbase.gateway.h", UnitName: "local-base-gateway-h.service"}
	got := RenderSystemd(m, Invocation{Program: "/opt/local base/lb", Arguments: []string{"--root", "/r 1", "serve"}}, "/r 1", "tok",
		map[string]string{"OTEL_TRACES_SAMPLER": "always_on", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://x:4318"})
	for _, want := range []string{
		`ExecStart="/opt/local base/lb" "--root" "/r 1" "serve"`,
		`WorkingDirectory=/r\x201`,
		`Environment="LOCALBASE_SERVICE_TOKEN=tok"`,
		"Environment=\"OTEL_EXPORTER_OTLP_ENDPOINT=http://x:4318\"\nEnvironment=\"OTEL_TRACES_SAMPLER=always_on\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if q := quoteSystemd(`a$b%c"d\e`, true); q != `"a$$b%%c\"d\\e"` {
		t.Errorf("quote %s", q)
	}
}
