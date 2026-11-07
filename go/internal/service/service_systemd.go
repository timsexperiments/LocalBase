package service

import (
	"strings"
)

func quoteSystemd(v string, expandDollar bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '%':
			b.WriteString("%%")
		case r == '$' && expandDollar:
			b.WriteString("$$")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func escapeSystemdPath(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '%':
			b.WriteString("%%")
		case ' ':
			b.WriteString(`\x20`)
		case '\t':
			b.WriteString(`\x09`)
		case '"':
			b.WriteString(`\x22`)
		case '\\':
			b.WriteString(`\x5c`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RenderSystemd renders the user unit exactly as TS does.
func RenderSystemd(m Metadata, inv Invocation, rootDir, token string, otel map[string]string) string {
	exec := []string{}
	for _, a := range append([]string{inv.Program}, inv.Arguments...) {
		exec = append(exec, quoteSystemd(a, true))
	}
	lines := []string{"[Unit]", "Description=LocalBase gateway", "After=network.target", "", "[Service]", "Type=exec",
		"ExecStart=" + strings.Join(exec, " "), "WorkingDirectory=" + escapeSystemdPath(rootDir), "Restart=on-failure",
		"RestartSec=2s", "KillMode=mixed", "KillSignal=SIGTERM", "TimeoutStopSec=15s", "UMask=0077",
		"Environment=" + quoteSystemd("LOCALBASE_SERVICE_ID="+m.ServiceID, false),
		"Environment=" + quoteSystemd("LOCALBASE_SERVICE_TOKEN="+token, false)}
	for _, k := range sortedKeys(otel) {
		lines = append(lines, "Environment="+quoteSystemd(k+"="+otel[k], false))
	}
	lines = append(lines, "StandardOutput=journal", "StandardError=journal", "", "[Install]", "WantedBy=default.target", "")
	return strings.Join(lines, "\n")
}
