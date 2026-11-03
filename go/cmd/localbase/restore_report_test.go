package main

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/timsexperiments/LocalBase/go/internal/backup"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

const plantedTail = "\x1b[2K\nsecond-line\u202e\u009b\u0430"

// keepLetters are the only fields that keep letters from another alphabet: names a person gave, and
// the path they typed. Everything else in the report is judged by eye, so it shows them as escapes.
// Adding a field to the report keeps this safe by default. Adding one here is a decision.
var keepLetters = []string{"Report.Target", "Report.ReturningKeys", "Report.KeysLost", "Report.Cautions", "Report.Grants.Role", "Report.RolesGone.Role"}

// keepsLetters says whether a path from plantInto is one of those, whatever its list index.
func keepsLetters(path string) bool {
	return slices.Contains(keepLetters, regexp.MustCompile(`\[[0-9]+\]`).ReplaceAllString(path, ""))
}

// plantInto fills a value by its type: every string gets its own marker and the characters a report
// mustn't print, and every list gets two entries. It knows the kinds printable knows, so a field of
// any other kind fails here until printable covers it. It returns each string's path.
func plantInto(t *testing.T, v reflect.Value, path string, leaves *[]string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		*leaves = append(*leaves, path)
		v.SetString(fmt.Sprintf("field%d%s", len(*leaves), plantedTail))
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				t.Fatalf("%s.%s isn't exported, so printable can't copy it", path, f.Name)
			}
			plantInto(t, v.Field(i), path+"."+f.Name, leaves)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			t.Fatalf("%s holds bytes, which printable can't escape", path)
		}
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range v.Len() {
			plantInto(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i), leaves)
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		plantInto(t, v.Elem(), path, leaves)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	default:
		t.Fatalf("%s is a %s, which printable doesn't cover", path, v.Kind())
	}
}

// eachString calls f with every string in a value, by the same walk.
func eachString(v reflect.Value, f func(string)) {
	switch v.Kind() {
	case reflect.String:
		f(v.String())
	case reflect.Struct:
		for i := range v.NumField() {
			eachString(v.Field(i), f)
		}
	case reflect.Slice:
		for i := range v.Len() {
			eachString(v.Index(i), f)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			eachString(v.Elem(), f)
		}
	}
}

func plantedReport(t *testing.T) (*backup.Report, []string) {
	t.Helper()
	rep := &backup.Report{}
	var leaves []string
	plantInto(t, reflect.ValueOf(rep).Elem(), "Report", &leaves)
	if len(leaves) < 20 {
		t.Fatalf("only %d strings in a report: %v", len(leaves), leaves)
	}
	return rep, leaves
}

// Built from the report's own fields, so a string added to it later is covered or this fails.
func TestEveryStringInTheReportIsMadePrintable(t *testing.T) {
	rep, leaves := plantedReport(t)
	var got []string
	eachString(reflect.ValueOf(printable(rep)).Elem(), func(s string) { got = append(got, s) })
	if len(got) != len(leaves) {
		t.Fatalf("%d strings came out of %d", len(got), len(leaves))
	}
	for i, s := range got {
		want := fmt.Sprintf(`field%d\x1b[2K\x0asecond-line\u202e\x9b`, i+1)
		if keepsLetters(leaves[i]) {
			want += "\u0430"
		} else {
			want += `\u0430`
		}
		if s != want {
			t.Errorf("%s: %q, want %q", leaves[i], s, want)
		}
	}
}

// It's a copy: what --json sends isn't changed by what the terminal gets.
func TestMakingTheReportPrintableLeavesItAlone(t *testing.T) {
	rep, _ := plantedReport(t)
	printable(rep)
	eachString(reflect.ValueOf(rep).Elem(), func(s string) {
		if !strings.HasSuffix(s, plantedTail) {
			t.Errorf("the original changed: %q", s)
		}
	})
}

func rendered(t *testing.T, rep *backup.Report) string {
	t.Helper()
	var out, errb bytes.Buffer
	c := &cli.Ctx{Stdout: &out, Stderr: &errb}
	printRestoreReport(c, rep)
	printRestoreNext(c, rep, &backup.Done{BeforeDir: "before" + plantedTail})
	return out.String() + errb.String()
}

// What actually goes to the terminal, with every field set, so every line that prints runs.
func TestTheReportPrintsNothingThatActsOnATerminal(t *testing.T) {
	rep, _ := plantedReport(t)
	got := rendered(t, rep)
	for _, r := range got {
		if r != '\n' && cli.Printable(string(r)) != string(r) {
			t.Errorf("%U reached the terminal", r)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "second-line") {
			t.Errorf("a value started a line of its own: %q", line)
		}
	}
	if !strings.Contains(got, `field1\x1b[2K\x0asecond-line`) {
		t.Errorf("it doesn't show what was escaped: %.300q", got)
	}
}

// With the fields allowed to keep their letters left out, nothing from another alphabet reaches the
// terminal at all, from any line.
func TestOnlyThoseFieldsKeepTheirLetters(t *testing.T) {
	rep, _ := plantedReport(t)
	rep.Target, rep.ReturningKeys, rep.KeysLost, rep.Cautions = "", nil, nil, nil
	for i := range rep.Grants {
		rep.Grants[i].Role = "role"
	}
	for i := range rep.RolesGone {
		rep.RolesGone[i].Role = "role"
	}
	var out bytes.Buffer
	c := &cli.Ctx{Stdout: &out, Stderr: &out}
	printRestoreReport(c, rep)
	printRestoreNext(c, rep, &backup.Done{})
	for _, r := range out.String() {
		if r > unicode.MaxASCII {
			t.Errorf("%U reached the terminal", r)
		}
	}
}

// And those fields do keep them, or a person couldn't read a name of their own.
func TestAFieldThatKeepsItsLettersDoes(t *testing.T) {
	rep, _ := plantedReport(t)
	safe := printable(rep)
	for _, got := range []string{safe.Target, safe.ReturningKeys[0], safe.KeysLost[0], safe.Cautions[0], safe.Grants[0].Role, safe.RolesGone[0].Role} {
		if !strings.HasSuffix(got, "\u0430") {
			t.Errorf("%q", got)
		}
	}
}

// Every list and setting the report holds reaches the terminal, so this test covers the lines that print them.
func TestTheReportPrintsWhatItHolds(t *testing.T) {
	rep, _ := plantedReport(t)
	got := rendered(t, rep)
	for name, value := range map[string]string{
		"a program's backend":   rep.Programs[1].Backend,
		"a program's argument":  rep.Programs[1].Command[1],
		"a model's source":      rep.Sources[1].Where,
		"a role":                rep.Grants[1].Role,
		"what a role gives":     rep.Grants[1].Gives.Value,
		"who has a role":        rep.Grants[1].Who[1].Value,
		"a role going":          rep.RolesGone[1].Role,
		"what it gave":          rep.RolesGone[1].Gives.Value,
		"who had it":            rep.RolesGone[1].Who[1].Value,
		"a way of signing in":   rep.SignIn.Who[1].Value,
		"what it's named":       rep.SignIn.Who[1].Named,
		"a way that goes":       rep.SignIn.Goes[1].Value,
		"what everyone gets":    rep.SignIn.Everyone.Value,
		"what's held back":      rep.SignIn.IfPolicyRemoved,
		"the address":           rep.SignIn.Origin.Value,
		"the mail server":       rep.SignIn.Mail.Value,
		"an issuer":             rep.Issuers[1].Issuer,
		"a setting's value":     rep.Settings[1].Value,
		"a key coming back":     rep.ReturningKeys[1],
		"a key lost":            rep.KeysLost[1],
		"a caution":             rep.Cautions[1],
		"a rule":                rep.Rules[1],
		"a secret's name":       rep.Manifest.SecretNames[1],
		"a removed secret":      rep.Manifest.SecretsRemoved[1].Restore,
		"a model to install":    rep.Manifest.Models[1],
		"the target":            rep.Target,
		"a binding coming back": rep.ReturningBindings[1].Issuer,
	} {
		if !strings.Contains(got, cli.PrintableASCII(value)) && !strings.Contains(got, cli.Printable(value)) {
			t.Errorf("%s isn't there", name)
		}
	}
}
