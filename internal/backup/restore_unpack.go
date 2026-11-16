package backup

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var (
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	newerRe    = regexp.MustCompile(`^localbase\.backup/v[0-9]+$`)
	formerIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func readManifest(tr *tar.Reader) (*Manifest, error) {
	h, err := tr.Next()
	if err != nil {
		return nil, refuse("This is not a LocalBase backup: %v", err)
	}
	if h.Name != manifestEntry || h.Typeflag != tar.TypeReg {
		return nil, refuse("This is not a LocalBase backup: it does not start with a manifest.")
	}
	if h.Size > maxManifestBytes {
		return nil, refuse("The manifest is larger than a backup's can be.")
	}
	raw, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
	if err != nil {
		return nil, refuse("The backup is damaged: %v", err)
	}
	if len(raw) > maxManifestBytes {
		return nil, refuse("The manifest is larger than a backup's can be.")
	}
	var probe struct {
		Format string `json:"format"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return nil, refuse("This is not a LocalBase backup: its manifest can't be read.")
	}
	if probe.Format != Format {
		if newerRe.MatchString(probe.Format) {
			return nil, refuse("This backup is in the format %s, and this version reads %s. Upgrade LocalBase to restore it.", probe.Format, Format)
		}
		return nil, refuse("This is not a LocalBase backup: unknown format %q.", probe.Format)
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, refuse("The manifest can't be read: %v", err)
	}
	return &m, nil
}

func checkManifestFiles(m *Manifest) (map[string]File, int64, error) {
	if len(m.Files) > maxEntries {
		return nil, 0, refuse("The backup lists more files than a backup can hold.")
	}
	byPath := map[string]File{}
	var total, settings int64
	for _, f := range m.Files {
		if !holdable(f.Path) {
			return nil, 0, refuse("The backup lists a file a backup can't hold: %q.", f.Path)
		}
		if _, dup := byPath[f.Path]; dup {
			return nil, 0, refuse("The backup lists %q twice.", f.Path)
		}
		if f.Size < 0 || f.Size > maxBytesOf(f.Path) {
			return nil, 0, refuse("The backup lists %q with a size a backup can't hold.", f.Path)
		}
		if isSettingsFile(f.Path) {
			if settings += f.Size; settings > maxSettingsTotalBytes {
				return nil, 0, refuse("The backup lists more settings than a backup can hold.")
			}
		}
		if !sha256Hex.MatchString(f.SHA256) {
			return nil, 0, refuse("The backup lists %q with no usable checksum.", f.Path)
		}
		if total += f.Size; total > maxTotalBytes {
			return nil, 0, refuse("The backup is larger than a backup can be.")
		}
		byPath[f.Path] = f
	}
	if _, ok := byPath["local-base.db"]; !ok {
		return nil, 0, refuse("The backup holds no database.")
	}
	return byPath, total, nil
}

func checkRoom(dir string, need int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	if free := int64(st.Bavail) * int64(st.Bsize); free < need {
		return refuse("This needs %d bytes of disk, and %d are free.", need, free)
	}
	return nil
}

// extract reads a whole backup into dir. Every entry must be one the manifest lists, of the
// size and hash it says, and within limits that are constants.
func extract(ctx context.Context, in io.Reader, dir string) (*Manifest, error) {
	br := bufio.NewReader(in)
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, refuse("This is not a LocalBase backup: %v", err)
	}
	defer func() { _ = gz.Close() }() // Only read from: a failed read has already refused the file.
	// One stream only: a second would be data after the end.
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	m, err := readManifest(tr)
	if err != nil {
		return nil, err
	}
	byPath, total, err := checkManifestFiles(m)
	if err != nil {
		return nil, err
	}
	if err := checkRoom(dir, total); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, refuse("The backup is damaged: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, refuse("The backup holds %q, which is not a plain file.", h.Name)
		}
		for key := range h.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse.") {
				return nil, refuse("The backup holds %q as a sparse file.", h.Name)
			}
		}
		if !strings.HasPrefix(h.Name, filesPrefix) {
			return nil, refuse("The backup holds %q, which it should not.", h.Name)
		}
		rel := strings.TrimPrefix(h.Name, filesPrefix)
		f, listed := byPath[rel]
		if !listed {
			return nil, refuse("The backup holds %q, which its manifest does not list.", h.Name)
		}
		if seen[rel] {
			return nil, refuse("The backup holds %q twice.", h.Name)
		}
		seen[rel] = true
		if h.Size != f.Size {
			return nil, refuse("%q is not the size the manifest says.", rel)
		}
		if err := writeEntry(ctxReader{ctx, tr}, filepath.Join(dir, filepath.FromSlash(rel)), f); err != nil {
			return nil, err
		}
	}
	for rel := range byPath {
		if !seen[rel] {
			return nil, refuse("The backup is missing %q, which its manifest lists.", rel)
		}
	}
	// A tar ends with zeros. Anything else, or another gzip stream, was added to the file.
	rest := bufio.NewReader(io.LimitReader(gz, 1<<20+1))
	var n int64
	for {
		b, err := rest.ReadByte()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, refuse("The backup is damaged: %v", err)
		}
		if n++; b != 0 || n > 1<<20 {
			return nil, refuse("The backup has data after its end.")
		}
	}
	if _, err := br.Peek(1); !errors.Is(err, io.EOF) {
		return nil, refuse("The backup has data after its end.")
	}
	return m, nil
}

func writeEntry(r io.Reader, to string, f File) error {
	if err := mkdirPrivate(filepath.Dir(to)); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Chmod(0o600); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, f.Size+1))
	if errors.Is(err, context.Canceled) {
		return err
	}
	if err != nil {
		return refuse("The backup is damaged: %v", err)
	}
	if n != f.Size {
		return refuse("%q is not the size the manifest says.", f.Path)
	}
	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return refuse("%q does not match its checksum.", f.Path)
	}
	return out.Sync()
}

// mkdirPrivate makes directories that are 0700 from their first moment, whatever the umask.
func mkdirPrivate(dir string) error {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return nil
	}
	if err := mkdirPrivate(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.Chmod(dir, 0o700) //nolint:gosec // G302: a directory needs its execute bit; 0700 is owner only.
}
