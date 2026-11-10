package gateway

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// Config holds exactly Server's exported fields, by name and type, and server copies each one. A
// field added to one and not the other, or left out of the copy, fails here.
func TestConfigIsEveryServerField(t *testing.T) {
	st, ct := reflect.TypeFor[Server](), reflect.TypeFor[Config]()
	exported := map[string]reflect.Type{}
	for f := range st.Fields() {
		if f.IsExported() {
			exported[f.Name] = f.Type
		}
	}
	for f := range ct.Fields() {
		want, ok := exported[f.Name]
		switch {
		case !ok:
			t.Errorf("Config.%s is not a Server field", f.Name)
		case want != f.Type:
			t.Errorf("Config.%s is %s, Server's is %s", f.Name, f.Type, want)
		}
		delete(exported, f.Name)
	}
	for name := range exported {
		t.Errorf("Server.%s is not in Config", name)
	}

	// Each field set to a value of its own, then copied.
	var cfg Config
	v := reflect.ValueOf(&cfg).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		case reflect.Func:
			f.Set(reflect.MakeFunc(f.Type(), func([]reflect.Value) []reflect.Value {
				out := make([]reflect.Value, f.Type().NumOut())
				for j := range out {
					out[j] = reflect.Zero(f.Type().Out(j))
				}
				return out
			}))
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int64:
			f.SetInt(int64(i + 1))
		case reflect.Struct:
			f.Field(0).SetInt(int64(i + 1)) // Limits: its first field is enough to tell a copy
		case reflect.Interface:
			switch ct.Field(i).Name {
			case "UI":
				f.Set(reflect.ValueOf(http.NewServeMux()))
			case "LoginPages":
				f.Set(reflect.ValueOf(gatewaytest.Pages{}))
			default:
				t.Fatalf("Config.%s: no value to set an interface of this kind to", ct.Field(i).Name)
			}
		default:
			t.Fatalf("Config.%s: no value to set a %s to", ct.Field(i).Name, f.Kind())
		}
	}
	s := reflect.ValueOf(cfg.server()).Elem()
	for i := range v.NumField() {
		name := ct.Field(i).Name
		got, want := s.FieldByName(name), v.Field(i)
		same := false
		switch want.Kind() {
		case reflect.Pointer, reflect.Func:
			same = got.Pointer() == want.Pointer()
		case reflect.Interface:
			same = got.Interface() == want.Interface()
		default:
			same = reflect.DeepEqual(got.Interface(), want.Interface())
		}
		if !same {
			t.Errorf("server doesn't copy Config.%s", name)
		}
	}
}
