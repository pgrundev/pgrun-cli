package pgbot

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func tarball(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		n string
		b []byte
	}{{"README.md", []byte("readme")}, {name, body}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.n, Mode: 0o755, Size: int64(len(f.b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.b)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func releaseServer(t *testing.T, version string, archive []byte, corrupt bool) {
	t.Helper()
	name, err := ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	sum := sha256.Sum256(archive)
	hexsum := hex.EncodeToString(sum[:])
	if corrupt {
		hexsum = strings.Repeat("0", 64)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v" + version + "/checksums.txt":
			fmt.Fprintf(w, "%s  %s\n%s  other_file.tar.gz\n", hexsum, name, strings.Repeat("1", 64))
		case "/v" + version + "/" + name:
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PGRUN_PGBOT_RELEASE_BASE", srv.URL)
}

func TestInstall_VerifiesChecksumAndWritesExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tarball path")
	}
	t.Setenv("PGRUN_HOME", t.TempDir())
	script := []byte("#!/bin/sh\necho 'pgbot version 0.9.0'\n")
	releaseServer(t, "0.9.0", tarball(t, "pgbot", script), false)
	res, err := Install(context.Background(), "0.9.0", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := ManagedPath()
	if res.Path != want || res.Replaced {
		t.Fatalf("result %+v", res)
	}
	st, err := os.Stat(res.Path)
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("not executable: %v %v", st, err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PGRUN_PGBOT", "")
	b, err := Locate(context.Background())
	if err != nil || b.Source != "managed" || b.Version != "0.9.0" || !b.Compatible || b.Capabilities.SchemaReview {
		t.Fatalf("locate after install: %+v %v", b, err)
	}
	// Installing again replaces in place.
	res2, err := Install(context.Background(), "0.9.0", "", nil)
	if err != nil || !res2.Replaced {
		t.Fatalf("reinstall %+v %v", res2, err)
	}
}

func TestInstall_ChecksumMismatchInstallsNothing(t *testing.T) {
	t.Setenv("PGRUN_HOME", t.TempDir())
	releaseServer(t, "0.9.0", tarball(t, "pgbot", []byte("evil")), true)
	if _, err := Install(context.Background(), "0.9.0", "", nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	p, _ := ManagedPath()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("a binary was written despite the mismatch")
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestInstall_MissingChecksumEntryRefuses(t *testing.T) {
	t.Setenv("PGRUN_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			fmt.Fprintln(w, strings.Repeat("a", 64)+"  unrelated.tar.gz")
			return
		}
		w.Write([]byte("blob"))
	}))
	defer srv.Close()
	t.Setenv("PGRUN_PGBOT_RELEASE_BASE", srv.URL)
	if _, err := Install(context.Background(), "0.9.0", "", nil); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("err = %v", err)
	}
}

func TestArchiveName(t *testing.T) {
	cases := map[[2]string]string{
		{"linux", "amd64"}:   "pgbot_0.9.0_linux_amd64.tar.gz",
		{"darwin", "arm64"}:  "pgbot_0.9.0_darwin_arm64.tar.gz",
		{"windows", "amd64"}: "pgbot_0.9.0_windows_amd64.zip",
	}
	for in, want := range cases {
		got, err := ArchiveName("v0.9.0", in[0], in[1])
		if err != nil || got != want {
			t.Errorf("%v: %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := ArchiveName("0.9.0", "plan9", "amd64"); err == nil {
		t.Error("plan9 should have no release")
	}
}

func TestAtLeast(t *testing.T) {
	for _, c := range []struct {
		v, min string
		want   bool
	}{{"0.9.0", "0.9.0", true}, {"0.10.0", "0.9.0", true}, {"0.8.9", "0.9.0", false}, {"1.0.0-rc1", "0.9.0", true}, {"v0.9.1", "0.9.0", true}} {
		if got := AtLeast(c.v, c.min); got != c.want {
			t.Errorf("AtLeast(%s, %s) = %v", c.v, c.min, got)
		}
	}
}

func TestLocate_OldVersionIsIncompatible(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "pgbot")
	os.WriteFile(p, []byte("#!/bin/sh\necho 'pgbot version 0.8.1'\n"), 0o755)
	t.Setenv("PGRUN_PGBOT", p)
	b, err := Locate(context.Background())
	if err != nil || b.Compatible || !strings.Contains(b.Problem, "older than") {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestChildEnvDropsAIKeysAndLibpqVars(t *testing.T) {
	env := childEnv([]string{"PATH=/bin", "OPENAI_API_KEY=x", "PGPASSWORD=y", "DATABASE_URL=prod", "HOME=/h"}, "postgres://branch", false)
	got := strings.Join(env, " ")
	if strings.Contains(got, "OPENAI") || strings.Contains(got, "PGPASSWORD") || strings.Contains(got, "=prod") {
		t.Fatalf("env %v", env)
	}
	if !strings.Contains(got, "DATABASE_URL=postgres://branch") || !strings.Contains(got, "PATH=/bin") {
		t.Fatalf("env %v", env)
	}
	if allowed := strings.Join(childEnv([]string{"OPENAI_API_KEY=x"}, "", true), " "); !strings.Contains(allowed, "OPENAI_API_KEY=x") {
		t.Fatalf("allowAI dropped the key: %s", allowed)
	}
}
