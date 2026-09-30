// Package download fetches a file and unpacks it with the checks the spec demands: the
// SHA-256 is verified before anything is used (fail closed), and archives cannot write
// outside their destination.
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
)

// MaxBytes caps a download (the runner archive is on the order of 100 MB).
const MaxBytes = 1 << 30

// Fetcher downloads files.
type Fetcher struct {
	Client *http.Client
	// AllowHTTP permits plain http URLs. Production leaves it false; tests use a local
	// server.
	AllowHTTP bool
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 15 * time.Minute}
}

// httpClient is the client with one rule added: a redirect may not leave https. GitHub
// serves release assets through a redirect, so checking only the first URL is not enough.
func (f *Fetcher) httpClient() *http.Client {
	c := *f.client() // a copy: the caller's client is not modified
	inner := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !f.AllowHTTP && req.URL.Scheme != "https" {
			return fmt.Errorf("refusing a redirect to %s: downloads must stay on https", req.URL.Redacted())
		}
		if inner != nil {
			return inner(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}

// Verified downloads url to dest and checks its SHA-256 against want (hex). The bytes are
// written to dest+".part" and renamed only after the checksum matches, so dest never holds
// unverified content and a crash leaves no half-written file where a reader would look.
func (f *Fetcher) Verified(ctx context.Context, url, want, dest string) error {
	if !f.AllowHTTP && !strings.HasPrefix(url, "https://") {
		return diag.New(diag.CodeDownloadFailed, "refusing to download over an insecure URL: "+url,
			"the release metadata points at a non-https address", "file an issue with this message; nothing was downloaded")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return diag.Wrap(err, diag.CodeDownloadFailed, "cannot create the download directory", "the disk is full or read-only", "check "+filepath.Dir(dest))
	}
	part := dest + ".part"
	defer os.Remove(part)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return diag.Wrap(err, diag.CodeDownloadFailed, "cannot build the download request", "the URL is malformed", "file an issue with this message")
	}
	req.Header.Set("User-Agent", "bladerunner")
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return diag.Wrap(err, diag.CodeDownloadFailed, "cannot download "+url, "the network is down or a proxy blocks the host", "check the connection, then re-run `bladerunner apply`")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return diag.New(diag.CodeDownloadFailed, fmt.Sprintf("download of %s answered HTTP %d", url, resp.StatusCode),
			"the release asset moved or GitHub is having problems", "re-run `bladerunner apply` in a few minutes")
	}

	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return diag.Wrap(err, diag.CodeDownloadFailed, "cannot write the download", "the disk is full or read-only", "check "+filepath.Dir(dest))
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, MaxBytes+1))
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return diag.Wrap(copyErr, diag.CodeDownloadFailed, "the download was interrupted", "the connection dropped, or the disk filled up", "re-run `bladerunner apply`")
	}
	if n > MaxBytes {
		return diag.New(diag.CodeDownloadFailed, "the download is larger than the allowed maximum", "the server sent far more than a runner archive", "file an issue with this message")
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return diag.New(diag.CodeChecksumMismatch,
			fmt.Sprintf("SHA-256 mismatch for %s: expected %s, got %s", filepath.Base(dest), want, got),
			"the file was corrupted in transit, or is not the file GitHub published; the download was discarded and nothing was installed",
			"re-run `bladerunner apply`; if it fails again, do not bypass this check: report it")
	}
	return os.Rename(part, dest)
}

// HasChecksum reports whether the file at path exists and has the given SHA-256 (hex), so
// a cached download can be reused without fetching it again.
func HasChecksum(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want)
}
