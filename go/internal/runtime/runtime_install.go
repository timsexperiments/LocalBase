package runtime

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Managed runtime installation (TS manager/binaries.ts ensureBinary): a
// pinned release archive is downloaded, checked against its size and
// sha256, extracted into staging and published as
// <root>/bin/runtimes/<family>/<archive sha256>/, with the TS receipt
// (.managed-binaries.json) so either implementation can verify it.

// goManifestJSON pins runtimes the Go build adds (CrispASR). Same format as
// the TS manifest, plus derived support files.
//
//go:embed go-runtime-manifest.json
var goManifestJSON []byte

// SupportFile is a file a runtime needs beside its executable, built from
// pinned sources and checked against a recorded sha256.
type SupportFile struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	// Derivation names how it is built; "misaki-gold-first-union" merges
	// two pinned dictionaries, gold first (CrispASR's Kokoro G2P).
	Derivation string          `json:"derivation"`
	Sources    []SupportSource `json:"sources"`
}

// SupportSource is one pinned input of a support file.
type SupportSource struct {
	URL               string `json:"url"`
	ExpectedSizeBytes int64  `json:"expectedSizeBytes"`
	SHA256            string `json:"sha256"`
}

type goManifest struct {
	Targets []struct {
		Platform     string `json:"platform"`
		Architecture string `json:"architecture"`
		Runtimes     map[string]struct {
			manifestArtifact
			SupportFiles []SupportFile `json:"supportFiles"`
		} `json:"runtimes"`
	} `json:"targets"`
}

func goPinned(family string) (manifestArtifact, []SupportFile, bool) {
	var m goManifest
	if json.Unmarshal(goManifestJSON, &m) != nil {
		return manifestArtifact{}, nil, false
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goruntime.GOARCH]
	for _, t := range m.Targets {
		if t.Platform == goruntime.GOOS && t.Architecture == arch {
			if a, ok := t.Runtimes[family]; ok {
				return a.manifestArtifact, a.SupportFiles, true
			}
		}
	}
	return manifestArtifact{}, nil, false
}

// release returns the pinned release of an executable's family.
func release(name string) (family string, a manifestArtifact, support []SupportFile, ok bool) {
	family = executableFamily[name]
	if a, ok := pinnedRuntime(family); ok {
		return family, a, nil, true
	}
	if a, s, ok := goPinned(family); ok {
		return family, a, s, true
	}
	return family, manifestArtifact{}, nil, false
}

var installMu sync.Mutex

// EnsureBinary returns a managed executable, installing its pinned release
// the first time.
func EnsureBinary(ctx context.Context, runtimesDir, name string) (string, error) {
	family, rel, support, ok := release(name)
	if !ok {
		return ResolveBinary(runtimesDir, name)
	}
	if p, err := ResolveBinary(runtimesDir, name); err == nil && supportComplete(filepath.Dir(p), support) {
		return p, nil
	}
	installMu.Lock()
	defer installMu.Unlock()
	packageDir := filepath.Join(runtimesDir, family, strings.ToLower(rel.SHA256))
	dest := filepath.Join(packageDir, name)
	if fileExists(dest) && supportComplete(packageDir, support) {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(packageDir), 0o700); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(filepath.Dir(packageDir), ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	archive := filepath.Join(work, rel.AssetName)
	if err := fetchVerified(ctx, rel.URL, archive, rel.ExpectedSizeBytes, rel.SHA256); err != nil {
		return "", fmt.Errorf("installing %s %s: %w", name, rel.Tag, err)
	}
	staging := filepath.Join(work, "package")
	if err := os.Mkdir(staging, 0o700); err != nil {
		return "", err
	}
	switch rel.Format {
	case "tar.gz":
		err = extractTarGz(archive, staging, rel.StripComponents)
	case "zip":
		err = extractZip(archive, staging, rel.StripComponents)
	default:
		err = fmt.Errorf("unsupported archive format %q", rel.Format)
	}
	if err != nil {
		return "", fmt.Errorf("extracting %s: %w", rel.AssetName, err)
	}
	if !fileExists(filepath.Join(staging, name)) {
		return "", fmt.Errorf("%s was not found after extracting %s", name, rel.AssetName)
	}
	for _, f := range support {
		if err := buildSupport(ctx, work, staging, f); err != nil {
			return "", err
		}
	}
	receipt := map[string]any{"version": 1, "runtimes": map[string]any{}}
	exe := filepath.Join(staging, name)
	info, _ := os.Stat(exe)
	os.Chmod(exe, info.Mode()|0o111)
	sum, err := sha256File(exe)
	if err != nil {
		return "", err
	}
	receipt["runtimes"].(map[string]any)[name] = map[string]any{"tag": rel.Tag, "assetName": rel.AssetName, "url": rel.URL,
		"expectedSizeBytes": rel.ExpectedSizeBytes, "authoritativeSha256": rel.SHA256, "format": rel.Format,
		"stripComponents": rel.StripComponents, "binarySha256": sum, "file": fileIdentityOf(exe)}
	b, _ := json.MarshalIndent(receipt, "", "  ")
	if err := os.WriteFile(filepath.Join(staging, ".managed-binaries.json"), b, 0o600); err != nil {
		return "", err
	}
	os.RemoveAll(packageDir)
	if err := os.Rename(staging, packageDir); err != nil {
		return "", err
	}
	return dest, nil
}

func supportComplete(dir string, files []SupportFile) bool {
	for _, f := range files {
		if sum, err := sha256File(filepath.Join(dir, f.Filename)); err != nil || sum != f.SHA256 {
			return false
		}
	}
	return true
}

// SupportPath is where a pinned support file of an executable lives once
// installed ("" when the family has none).
func SupportPath(runtimesDir, name, filename string) string {
	family, rel, support, ok := release(name)
	if !ok {
		return ""
	}
	for _, f := range support {
		if f.Filename == filename {
			return filepath.Join(runtimesDir, family, strings.ToLower(rel.SHA256), filename)
		}
	}
	return ""
}

// fetchURL maps a pinned URL to where it is fetched from (tests serve the
// pinned bytes locally; the checksums still apply).
var fetchURL = func(u string) string { return u }

func fetchVerified(ctx context.Context, url, dest string, size int64, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL(url), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if size > 0 && n != size {
		return fmt.Errorf("download is %d bytes, expected %d", n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", want, got)
	}
	return nil
}

// safeTarget joins an archive entry name under dir after stripping leading
// components, refusing absolute paths and anything that escapes dir.
func safeTarget(dir, name string, strip int) (string, bool) {
	name = path.Clean(strings.TrimPrefix(filepath.ToSlash(name), "./"))
	parts := strings.Split(name, "/")
	if len(parts) <= strip {
		return "", false
	}
	rel := path.Join(parts[strip:]...)
	if rel == "." || strings.HasPrefix(rel, "../") || rel == ".." || path.IsAbs(name) {
		return "", false
	}
	return filepath.Join(dir, filepath.FromSlash(rel)), true
}

func extractTarGz(archive, dir string, strip int) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, ok := safeTarget(dir, h.Name, strip)
		if !ok {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0o700)
		case tar.TypeReg:
			if err := writeEntry(target, tr, h.FileInfo().Mode()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Only links that stay inside the package.
			if t := filepath.Join(filepath.Dir(target), h.Linkname); !strings.HasPrefix(t, dir+string(filepath.Separator)) || filepath.IsAbs(h.Linkname) {
				return fmt.Errorf("archive link %s points outside the package", h.Name)
			}
			os.MkdirAll(filepath.Dir(target), 0o700)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		}
	}
}

func extractZip(archive, dir string, strip int) error {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer z.Close()
	for _, f := range z.File {
		target, ok := safeTarget(dir, f.Name, strip)
		if !ok {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0o700)
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive entry %s is a link", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeEntry(target, rc, f.Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeEntry(target string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
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

func fileIdentityOf(p string) map[string]any {
	info, err := os.Stat(p)
	if err != nil {
		return nil
	}
	ms := float64(info.ModTime().UnixNano()) / 1e6
	id := map[string]any{"size": info.Size(), "mtimeMs": ms, "ctimeMs": ms}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		id["dev"], id["ino"] = int64(st.Dev), int64(st.Ino)
	}
	return id
}

// buildSupport downloads a support file's pinned sources and derives it.
func buildSupport(ctx context.Context, work, staging string, f SupportFile) error {
	var inputs []string
	for i, s := range f.Sources {
		p := filepath.Join(work, fmt.Sprintf("support-%d", i))
		if err := fetchVerified(ctx, s.URL, p, s.ExpectedSizeBytes, s.SHA256); err != nil {
			return fmt.Errorf("support file %s: %w", f.Filename, err)
		}
		inputs = append(inputs, p)
	}
	var out []byte
	switch f.Derivation {
	case "misaki-gold-first-union":
		if len(inputs) != 2 {
			return errors.New("misaki merge needs gold and silver")
		}
		var err error
		if out, err = MergeMisaki(inputs[0], inputs[1]); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown derivation %q", f.Derivation)
	}
	sum := sha256.Sum256(out)
	if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
		return fmt.Errorf("support file %s did not reproduce its pinned checksum (got %s)", f.Filename, got)
	}
	return os.WriteFile(filepath.Join(staging, f.Filename), out, 0o600)
}
