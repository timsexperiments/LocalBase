package gatewaytest

import (
	"fmt"
	"testing"
)

// MultipartSeeds are the forms FuzzMultipart and FuzzRequestModel start from.
func MultipartSeeds(f *testing.F) {
	const b = "BOUNDARY"
	part := func(name, filename, body string) string {
		if filename != "" {
			return fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=%q; filename=%q\r\n\r\n%s\r\n", b, name, filename, body)
		}
		return fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=%q\r\n\r\n%s\r\n", b, name, body)
	}
	f.Add(b, []byte{})
	f.Add(b, []byte(part("model", "", "m")+part("file", "a.wav", "x")+"--"+b+"--\r\n"))
	f.Add(b, []byte(part("image[]", "a.png", "x")+part("image[]", "", "y")))
	f.Add("", []byte("--\r\n"))
	f.Add(`a"b`, []byte("--a\"b\r\n\r\n"))
}
