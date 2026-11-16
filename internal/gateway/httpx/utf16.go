package httpx

import "unicode/utf16"

// UTF16Length is how long s is in UTF-16 code units, as JavaScript counts a string's length.
func UTF16Length(s string) int { return len(utf16.Encode([]rune(s))) }
