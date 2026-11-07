package store

import "net/url"

// DSN is the "file:" name SQLite opens for path, with query after it. SQLite reads that name as a
// URI, so the path is escaped: unescaped, a "?" or "#" in a root's path ends the file name and
// "%41" is read as "A", and SQLite then opens another file than the one the store guards and holds.
func DSN(path, query string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
}
