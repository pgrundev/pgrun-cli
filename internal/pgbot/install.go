package pgbot

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ReleaseBase is the GitHub release download root; $PGRUN_PGBOT_RELEASE_BASE
// overrides it (a mirror, an air-gapped proxy, tests).
const ReleaseBase = "https://github.com/pgrundev/pgbot/releases/download"

// maxArchiveBytes bounds what install will download or extract.
const maxArchiveBytes = 200 << 20

type InstallResult struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Archive  string `json:"archive"`
	SHA256   string `json:"sha256"`
	Replaced bool   `json:"replaced"`
}

// ArchiveName is the release asset for this platform, matching pgbot's
// goreleaser name_template (pgbot_<ver>_<os>_<arch>.tar.gz; .zip on Windows).
func ArchiveName(version, goos, goarch string) (string, error) {
	switch goos {
	case "linux", "darwin", "windows":
	default:
		return "", fmt.Errorf("no pgbot release for %s (build it: go install github.com/pgrundev/pgbot/cmd/pgbot@latest)", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("no pgbot release for %s/%s", goos, goarch)
	}
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("pgbot_%s_%s_%s.%s", strings.TrimPrefix(version, "v"), goos, goarch, ext), nil
}

// Install downloads pgbot <version> for this platform, verifies the archive's
// SHA-256 against the release's checksums.txt, and atomically writes the
// binary to dest (default ManagedPath). Nothing is made executable before the
// checksum matches. A partially written file never replaces a good one.
func Install(ctx context.Context, version, dest string, hc *http.Client) (InstallResult, error) {
	if version == "" {
		version = PinnedVersion
	}
	version = strings.TrimPrefix(version, "v")
	if dest == "" {
		p, err := ManagedPath()
		if err != nil {
			return InstallResult{}, err
		}
		dest = p
	}
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	base := os.Getenv("PGRUN_PGBOT_RELEASE_BASE")
	if base == "" {
		base = ReleaseBase
	}
	base = strings.TrimRight(base, "/") + "/v" + version
	archive, err := ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return InstallResult{}, err
	}
	sums, err := fetch(ctx, hc, base+"/checksums.txt", 1<<20)
	if err != nil {
		return InstallResult{}, fmt.Errorf("download checksums.txt: %w", err)
	}
	want, err := checksumFor(sums, archive)
	if err != nil {
		return InstallResult{}, err
	}
	blob, err := fetch(ctx, hc, base+"/"+archive, maxArchiveBytes)
	if err != nil {
		return InstallResult{}, fmt.Errorf("download %s: %w", archive, err)
	}
	sum := sha256.Sum256(blob)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return InstallResult{}, fmt.Errorf("checksum mismatch for %s: got %s, release says %s — not installing", archive, got, want)
	}
	bin, err := extract(blob, archive)
	if err != nil {
		return InstallResult{}, err
	}
	_, statErr := os.Stat(dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return InstallResult{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".pgbot-install-*")
	if err != nil {
		return InstallResult{}, err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return InstallResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return InstallResult{}, err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return InstallResult{}, err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Path: dest, Version: version, Archive: archive, SHA256: got, Replaced: statErr == nil}, nil
}

func fetch(ctx context.Context, hc *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return b, nil
}

func checksumFor(sums []byte, archive string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == archive {
			if len(f[0]) != 64 {
				break
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s — not installing", archive)
}

func extract(blob []byte, archive string) ([]byte, error) {
	name := exeName()
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == name && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return readLimited(rc)
			}
		}
		return nil, fmt.Errorf("%s has no %s", archive, name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s has no %s", archive, name)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == name {
			return readLimited(tr)
		}
	}
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxArchiveBytes {
		return nil, fmt.Errorf("pgbot binary larger than %d bytes", maxArchiveBytes)
	}
	return b, nil
}
