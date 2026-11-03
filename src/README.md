# Legacy TypeScript implementation

This directory is the Bun/TypeScript version of LocalBase (0.1.x). The Go code in [`go/`](../go) replaces it and is where new work belongs.

`src/` stays only as a rollback path and as the source for a few generated Go files. Until it is removed, change it only for a fix that the rollback path needs, or a catalog or runtime pin the Go sync still reads.
