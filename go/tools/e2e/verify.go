package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// sha256File is a file's checksum in hex.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifyArchive(path, want string) error {
	got, err := sha256File(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s has checksum %s, and the pin is %s", path, got, want)
	}
	return nil
}

// verifyChrome checks a fetched Chrome before any of it runs, and stops at the first thing wrong.
// The archive must match its pin, and what is unpacked beside it must be what the archive holds,
// so a changed or half-written file is caught whenever it happened. It says how many files it
// compared. Nothing is written, so runs sharing a cache do not disturb each other here.
func verifyChrome(dir, archive, want string) (int, error) {
	if err := verifyArchive(archive, want); err != nil {
		return 0, err
	}
	z, err := zip.OpenReader(archive)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", archive, err)
	}
	defer z.Close()
	expected := map[string]bool{filepath.Clean(archive): true}
	files := 0
	for _, entry := range z.File {
		if !filepath.IsLocal(filepath.FromSlash(entry.Name)) {
			return files, fmt.Errorf("%s: an entry outside its directory: %s", archive, entry.Name)
		}
		path := filepath.Join(dir, filepath.FromSlash(entry.Name))
		expected[filepath.Clean(path)] = true
		if entry.FileInfo().IsDir() {
			continue
		}
		if err := sameAsEntry(path, entry); err != nil {
			return files, err
		}
		files++
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !expected[filepath.Clean(path)] {
			return fmt.Errorf("%s is not in the archive", path)
		}
		return nil
	})
	return files, err
}

// sameAsEntry compares one unpacked file, or symlink, with its entry in the archive.
func sameAsEntry(path string, entry *zip.File) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	r, err := entry.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	if entry.Mode()&fs.ModeSymlink != 0 {
		want, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		got, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("%s should be a symlink: %w", path, err)
		}
		if got != string(want) {
			return fmt.Errorf("%s links to %s, and the archive says %s", path, got, want)
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if uint64(info.Size()) != entry.UncompressedSize64 {
		return fmt.Errorf("%s is %d bytes, and the archive says %d", path, info.Size(), entry.UncompressedSize64)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return sameBytes(path, f, r)
}

func sameBytes(path string, got, want io.Reader) error {
	a, b := make([]byte, 256<<10), make([]byte, 256<<10)
	for {
		n, errA := io.ReadFull(got, a)
		m, errB := io.ReadFull(want, b)
		if !bytes.Equal(a[:n], b[:m]) {
			return fmt.Errorf("%s differs from the archive", path)
		}
		endA := errors.Is(errA, io.EOF) || errors.Is(errA, io.ErrUnexpectedEOF)
		endB := errors.Is(errB, io.EOF) || errors.Is(errB, io.ErrUnexpectedEOF)
		switch {
		case endA && endB:
			return nil
		case errA != nil && !endA:
			return errA
		case errB != nil && !endB:
			return errB
		case endA != endB:
			return fmt.Errorf("%s differs from the archive", path)
		}
	}
}

// verifyAxe is the same for axe-core: the tarball against its pin, the script against the tarball.
func verifyAxe(script, archive, want string) error {
	if err := verifyArchive(archive, want); err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return fmt.Errorf("%s: no package/axe.min.js in it: %w", archive, err)
		}
		if h.Name != "package/axe.min.js" {
			continue
		}
		got, err := os.Open(script)
		if err != nil {
			return err
		}
		defer got.Close()
		return sameBytes(script, got, tr)
	}
}
