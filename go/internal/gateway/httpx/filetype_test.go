package httpx

import "testing"

func TestFileTypeOf(t *testing.T) {
	for name, want := range map[string]string{"a.png": "image/png", "x.PNG": "image/png", "x.jpe": "image/jpeg", "a.b.JPG": "image/jpeg",
		"image": "", ".png": "", "x.": "", "": ""} {
		if got := fileTypeOf(name); got != want {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
	if fileTypeOf("x.gif") == "" || fileTypeOf("in.bin") == "" {
		t.Error("known extensions should have a type")
	}
}
