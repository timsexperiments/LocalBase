package gatewaytest

import "bytes"

// FormType is the content type of a form FormOf makes.
const FormType = "multipart/form-data; boundary=b"

// FormOf is a form of n empty parts called x, then whatever else is given as ready-made parts.
func FormOf(n int, more ...string) []byte {
	var b bytes.Buffer
	for range n {
		b.WriteString("--b\r\nContent-Disposition: form-data; name=\"x\"\r\n\r\n\r\n")
	}
	for _, part := range more {
		b.WriteString(part)
	}
	b.WriteString("--b--\r\n")
	return b.Bytes()
}
