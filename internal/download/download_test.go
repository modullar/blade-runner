package download

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func serve(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestVerifiedAcceptsMatchingChecksum(t *testing.T) {
	body := []byte("the runner archive")
	s := serve(t, body, 200)
	dest := filepath.Join(t.TempDir(), "cache", "runner.tar.gz")
	f := &Fetcher{AllowHTTP: true}
	if err := f.Verified(context.Background(), s.URL, strings.ToUpper(sum(body)), dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, body) {
		t.Errorf("dest content = %q, %v", got, err)
	}
	if _, err := os.Stat(dest + ".part"); err == nil {
		t.Error(".part file left behind")
	}
}

func TestVerifiedFailsClosedOnMismatch(t *testing.T) {
	s := serve(t, []byte("tampered"), 200)
	dest := filepath.Join(t.TempDir(), "runner.tar.gz")
	err := (&Fetcher{AllowHTTP: true}).Verified(context.Background(), s.URL, sum([]byte("genuine")), dest)
	if diag.CodeOf(err) != diag.CodeChecksumMismatch {
		t.Fatalf("err = %v, want BR-E031", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 0 {
		t.Errorf("a mismatching download must leave nothing on disk, found %v", entries)
	}
}

func TestVerifiedDoesNotReplaceAGoodFileWithABadOne(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "runner.tar.gz")
	if err := os.WriteFile(dest, []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := serve(t, []byte("bad"), 200)
	if err := (&Fetcher{AllowHTTP: true}).Verified(context.Background(), s.URL, sum([]byte("other")), dest); err == nil {
		t.Fatal("expected a mismatch")
	}
	if got, _ := os.ReadFile(dest); string(got) != "good" {
		t.Errorf("existing file was clobbered: %q", got)
	}
}

func TestVerifiedRefusesHTTPUnlessAllowed(t *testing.T) {
	s := serve(t, []byte("x"), 200)
	err := (&Fetcher{}).Verified(context.Background(), s.URL, sum([]byte("x")), filepath.Join(t.TempDir(), "f"))
	if diag.CodeOf(err) != diag.CodeDownloadFailed || !strings.Contains(err.Error(), "insecure") {
		t.Errorf("err = %v", err)
	}
}

func TestVerifiedReportsHTTPAndNetworkFailures(t *testing.T) {
	s := serve(t, []byte("nope"), 404)
	dest := filepath.Join(t.TempDir(), "f")
	f := &Fetcher{AllowHTTP: true}
	if err := f.Verified(context.Background(), s.URL, sum(nil), dest); diag.CodeOf(err) != diag.CodeDownloadFailed || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
	s.Close()
	if err := f.Verified(context.Background(), s.URL, sum(nil), dest); diag.CodeOf(err) != diag.CodeDownloadFailed {
		t.Errorf("unreachable: %v", err)
	}
}

type entry struct {
	name, link string
	typ        byte
	mode       int64
	body       string
}

func makeTarGz(t *testing.T, entries []entry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: typ, Mode: e.mode, Size: int64(len(e.body))}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractPreservesLayoutAndModes(t *testing.T) {
	archive := makeTarGz(t, []entry{
		{name: "bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "bin/tool", mode: 0o755, body: "#!/bin/sh\n"},
		{name: "config.sh", mode: 0o755, body: "x"},
		{name: "data.txt", mode: 0o644, body: "y"},
		{name: "./dotted/file", mode: 0o600, body: "z"},
		{name: "bin/alias", typ: tar.TypeSymlink, link: "tool"},
	})
	dest := filepath.Join(t.TempDir(), "out")
	if err := ExtractTarGz(archive, dest); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"bin/tool": 0o755, "config.sh": 0o755, "data.txt": 0o644, "dotted/file": 0o600} {
		info, err := os.Stat(filepath.Join(dest, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", name, info.Mode().Perm(), want)
		}
	}
	if got, err := os.Readlink(filepath.Join(dest, "bin/alias")); err != nil || got != "tool" {
		t.Errorf("symlink = %q, %v", got, err)
	}
}

func TestExtractStripsSetuid(t *testing.T) {
	archive := makeTarGz(t, []entry{{name: "suid", mode: 0o4755, body: "x"}})
	dest := t.TempDir()
	if err := ExtractTarGz(archive, dest); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dest, "suid"))
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("special bits survived: %v", info.Mode())
	}
}

func TestExtractRefusesUnsafeArchives(t *testing.T) {
	tests := []struct {
		name    string
		entries []entry
	}{
		{"parent traversal", []entry{{name: "../evil", body: "x", mode: 0o644}}},
		{"nested traversal", []entry{{name: "a/../../evil", body: "x", mode: 0o644}}},
		{"absolute path", []entry{{name: "/tmp/evil", body: "x", mode: 0o644}}},
		{"absolute symlink", []entry{{name: "link", typ: tar.TypeSymlink, link: "/etc"}}},
		{"escaping symlink", []entry{{name: "link", typ: tar.TypeSymlink, link: "../../etc"}}},
		{"nested escaping symlink", []entry{{name: "a/link", typ: tar.TypeSymlink, link: "../../x"}}},
		{"symlink chain escapes", []entry{
			{name: "x", typ: tar.TypeDir, mode: 0o755},
			{name: "x/l1", typ: tar.TypeSymlink, link: ".."},    // lexically fine: points at the root
			{name: "x/l1/l2", typ: tar.TypeSymlink, link: ".."}, // really lives in the root, so ".." leaves it
		}},
		{"hard link", []entry{{name: "h", typ: tar.TypeLink, link: "x"}}},
		{"device", []entry{{name: "d", typ: tar.TypeChar}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "out")
			err := ExtractTarGz(makeTarGz(t, tc.entries), dest)
			if err == nil {
				t.Fatal("expected the archive to be refused")
			}
			if diag.CodeOf(err) != diag.CodeDownloadFailed {
				t.Errorf("code = %q", diag.CodeOf(err))
			}
			// Nothing may have been written outside dest.
			outside, _ := filepath.Glob(filepath.Join(parent, "*"))
			for _, p := range outside {
				if p != dest {
					t.Errorf("wrote outside the destination: %s", p)
				}
			}
			if _, err := os.Stat("/tmp/evil"); err == nil {
				os.Remove("/tmp/evil")
				t.Error("wrote /tmp/evil")
			}
		})
	}
}

func TestExtractAllowsWritesThroughSymlinksThatStayInside(t *testing.T) {
	archive := makeTarGz(t, []entry{
		{name: "x", typ: tar.TypeDir, mode: 0o755},
		{name: "x/up", typ: tar.TypeSymlink, link: ".."}, // points at the root itself
		{name: "x/up/file", body: "ok", mode: 0o644},     // lands in the root: still inside
	})
	dest := t.TempDir()
	if err := ExtractTarGz(archive, dest); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "file")); err != nil || string(got) != "ok" {
		t.Errorf("file = %q, %v", got, err)
	}
}

func TestExtractRejectsNonArchives(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.tar.gz")
	_ = os.WriteFile(p, []byte("not gzip"), 0o600)
	if err := ExtractTarGz(p, t.TempDir()); diag.CodeOf(err) != diag.CodeDownloadFailed {
		t.Errorf("err = %v", err)
	}
	if err := ExtractTarGz(filepath.Join(t.TempDir(), "missing"), t.TempDir()); err == nil {
		t.Error("missing archive must error")
	}
}

func TestRedirectsMayNotLeaveHTTPS(t *testing.T) {
	plain := serve(t, []byte("payload"), 200) // an http:// target
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer origin.Close()

	dest := filepath.Join(t.TempDir(), "f")
	f := &Fetcher{Client: origin.Client()} // trusts the test server's certificate; AllowHTTP is false
	err := f.Verified(context.Background(), origin.URL, sum([]byte("payload")), dest)
	if diag.CodeOf(err) != diag.CodeDownloadFailed || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want a refusal to follow the redirect to http", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("a file was written after an insecure redirect")
	}
}

func TestHTTPSRedirectsAreFollowed(t *testing.T) {
	final := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("payload")) }))
	defer final.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer origin.Close()
	// One client that trusts both test servers' certificates.
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	pool.AddCert(final.Certificate())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	dest := filepath.Join(t.TempDir(), "f")
	if err := (&Fetcher{Client: client}).Verified(context.Background(), origin.URL, sum([]byte("payload")), dest); err != nil {
		t.Fatalf("an https-to-https redirect must work (GitHub serves assets this way): %v", err)
	}
}
