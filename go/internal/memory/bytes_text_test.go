package memory

import "testing"

// A file's size at each unit's edge, and one that rounds up into the next tenth.
func TestBytesText(t *testing.T) {
	for n, want := range map[int64]string{
		0:                "0 bytes",
		1023:             "1023 bytes",
		1024:             "1.0 KiB",
		1<<20 - 1:        "1024.0 KiB",
		1 << 20:          "1.0 MiB",
		1<<30 - 1:        "1024.0 MiB",
		1 << 30:          "1.0 GiB",
		2093796557:       "2.0 GiB", // 1.95 GiB and a byte: one decimal rounds it up
		3<<30 + 300<<20:  "3.3 GiB",
		1536 * (1 << 20): "1.5 GiB",
	} {
		if got := BytesText(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}
