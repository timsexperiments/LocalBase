package httpx

import (
	"mime"
	"strings"
)

// fileTypeOf is Bun's File.type for an uploaded file: the MIME type of the
// filename's extension ("" for none or an unknown one).
func fileTypeOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	switch ext := strings.ToLower(name[i+1:]); ext {
	case "png":
		return "image/png"
	case "jpg", "jpeg", "jpe":
		return "image/jpeg"
	default:
		return mime.TypeByExtension("." + ext)
	}
}
