package conf

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/toml"
)

// Overrides are the configure settings a TOML file may carry.
type Overrides struct {
	Root, Host, SttHost, CtxMode, OtelEndpoint, OtelHeaders, HFToken, Parallel *string
	Port, SttPort, CtxSize, OtelSampleRatio                                    *int
	Selected                                                                   map[string][]string
	Active                                                                     map[string]*string
	SystemReserve, AcceleratorReserve                                          *store.MemoryReserve
}

// ParseOverrides reads a configure TOML document: known keys only, checked by type and range. A
// document that isn't valid is a *DocumentError, whose message names the problem.
func ParseOverrides(text string) (*Overrides, error) {
	doc, err := toml.Parse(text)
	if err != nil {
		return nil, &DocumentError{Message: fmt.Sprintf("Invalid TOML configuration: %v", err)}
	}
	o := &Overrides{Selected: map[string][]string{}, Active: map[string]*string{}}
	var issues []string
	bad := func(key, msg string) { issues = append(issues, key+": "+msg) }
	str := func(key string, v any) *string {
		s, ok := v.(string)
		if !ok {
			bad(key, "Invalid input: expected string")
			return nil
		}
		return &s
	}
	num := func(key string, v any, lo, hi int64) *int {
		n, ok := v.(int64)
		if !ok || n < lo || n > hi {
			bad(key, fmt.Sprintf("must be an integer from %d to %d", lo, hi))
			return nil
		}
		i := int(n)
		return &i
	}
	for _, k := range toml.SortedKeys(doc) {
		v := doc[k]
		switch k {
		case "root":
			o.Root = str(k, v)
		case "host":
			o.Host = str(k, v)
		case "sttHost":
			o.SttHost = str(k, v)
		case "hfToken":
			o.HFToken = str(k, v)
		case "otelEndpoint":
			o.OtelEndpoint = str(k, v)
			if o.OtelEndpoint != nil && *o.OtelEndpoint != "" {
				if msg := obs.ValidateEndpoint(*o.OtelEndpoint); msg != "" {
					bad(k, msg)
				}
			}
		case "otelHeaders":
			if o.OtelHeaders = str(k, v); o.OtelHeaders != nil {
				if _, err := obs.ParseHeaders(*o.OtelHeaders); err != nil {
					bad(k, err.Error())
				}
			}
		case "ctxMode":
			if o.CtxMode = str(k, v); o.CtxMode != nil && *o.CtxMode != "auto" && *o.CtxMode != "explicit" {
				bad(k, `Invalid option: expected one of "auto"|"explicit"`)
			}
		case "parallel":
			switch p := v.(type) {
			case string:
				if p != "auto" {
					bad(k, "must be \"auto\" or 1-4")
				}
				o.Parallel = &p
			case int64:
				if p < 1 || p > 4 {
					bad(k, "must be \"auto\" or 1-4")
				}
				s := strconv.FormatInt(p, 10)
				o.Parallel = &s
			default:
				bad(k, "must be \"auto\" or 1-4")
			}
		case "port":
			o.Port = num(k, v, 1, 65535)
		case "sttPort":
			o.SttPort = num(k, v, 1, 65535)
		case "ctxSize":
			o.CtxSize = num(k, v, 1, 2_147_483_647)
		case "otelSampleRatio":
			o.OtelSampleRatio = num(k, v, 0, 100)
		case "memory":
			m, ok := v.(map[string]any)
			if !ok {
				bad(k, "Invalid input: expected object")
				continue
			}
			for _, rk := range toml.SortedKeys(m) {
				r, ok := m[rk].(map[string]any)
				if !ok || (rk != "systemReserve" && rk != "acceleratorReserve") {
					bad("memory."+rk, "Unrecognized key")
					continue
				}
				res := store.MemoryReserve{Percent: 15, MinimumGB: 8}
				if rk == "acceleratorReserve" {
					res = store.MemoryReserve{Percent: 10, MinimumGB: 2}
				}
				for fk, fv := range r {
					f, isF := fv.(float64)
					if n, isN := fv.(int64); isN {
						f, isF = float64(n), true
					}
					switch {
					case !isF:
						bad("memory."+rk+"."+fk, "Invalid input: expected number")
					case fk == "percent" && f >= 0 && f <= 100:
						res.Percent = f
					case fk == "minimumGb" && f >= 0:
						res.MinimumGB = f
					default:
						bad("memory."+rk+"."+fk, "Invalid value")
					}
				}
				if rk == "systemReserve" {
					o.SystemReserve = &res
				} else {
					o.AcceleratorReserve = &res
				}
			}
		default:
			kind := ""
			for _, kk := range store.Kinds {
				if k == "selected"+KindField(kk)+"Models" || k == "active"+KindField(kk)+"Model" {
					kind = kk
				}
			}
			switch {
			case kind != "" && strings.HasPrefix(k, "selected"):
				list, ok := v.([]any)
				if !ok {
					bad(k, "Invalid input: expected array")
					continue
				}
				ids := []string{}
				for _, e := range list {
					s, ok := e.(string)
					if !ok {
						bad(k, "Invalid input: expected string")
					}
					ids = append(ids, s)
				}
				o.Selected[kind] = ids
			case kind != "":
				o.Active[kind] = str(k, v)
			default:
				bad(k, "Unrecognized key")
			}
		}
	}
	if len(issues) > 0 {
		return nil, &DocumentError{Message: strings.Join(issues, "; ")}
	}
	return o, nil
}
