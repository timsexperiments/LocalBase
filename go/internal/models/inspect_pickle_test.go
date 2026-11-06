package models

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeFileNamed(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func zipOf(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("bytes"))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A pickle runs code when something loads it, whatever it's called. Names are checked before a
// download, but weights in this form usually arrive as pytorch_model.bin, so the bytes are checked too.
func TestAPickleIsRefusedWhateverItIsCalled(t *testing.T) {
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"protocol 2", []byte("\x80\x02}q\x00.")},
		{"protocol 3", []byte("\x80\x03}q\x00.")},
		{"protocol 4", []byte("\x80\x04\x95\x00\x00\x00\x00")},
		{"protocol 5", []byte("\x80\x05\x95\x00\x00\x00\x00")},
		{"a checkpoint, as PyTorch writes one", zipOf(t, "archive/data.pkl", "archive/data/0", "archive/version")},
		{"a pickle at the top of a zip", zipOf(t, "data.pkl")},
		{"a pickle named in capitals", zipOf(t, "archive/DATA.PKL")},
	} {
		ff := InspectFile(writeFileNamed(t, "pytorch_model.bin", c.body))
		if !ff.Refused {
			t.Errorf("%s wasn't refused: %+v", c.name, ff)
		}
	}
}

// Other files with the same kind of name go on as before.
func TestFilesThatAreNotPicklesAreNotRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"whisper weights", []byte("lmgg\x00\x00\x00\x00")},
		{"raw numbers", []byte("\x00\x00\x80\x3f\x00\x00\x00\x40")},
		{"raw numbers that start like a pickle of no known version", []byte("\x80\x06\x00\x00\x00\x00")},
		{"raw numbers that start with the same byte", []byte("\x80\x00\x00\x00\x00\x00")},
		{"a zip of something else", zipOf(t, "voices/a.wav", "readme.txt")},
		{"a file that only looks like a zip", []byte("PK\x03\x04 and then nothing a zip would hold")},
		{"text", []byte("{\"a\":1}")},
	} {
		ff := InspectFile(writeFileNamed(t, "model.bin", c.body))
		if ff.Refused {
			t.Errorf("%s was refused: %s", c.name, ff.Problem)
		}
	}
}

func TestARefusedPickleSaysWhy(t *testing.T) {
	ff := InspectFile(writeFileNamed(t, "pytorch_model.bin", []byte("\x80\x02}q\x00.")))
	if ff.Problem == "" {
		t.Error("no reason given")
	}
}
