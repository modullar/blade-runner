package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
)

const storeSchema = 1

// Signer is one trusted public key, held on this machine. The private half never comes here:
// contributors keep theirs, and give the owner only the public key.
type Signer struct {
	Name        string     `json:"name"`
	Key         string     `json:"key"` // "ssh-ed25519 AAAA..." without a comment
	Fingerprint string     `json:"fingerprint"`
	AddedAt     time.Time  `json:"added_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether the signer's commits are accepted at time now.
func (s Signer) Active(now time.Time) bool {
	if s.RevokedAt != nil {
		return false
	}
	return s.ExpiresAt == nil || now.Before(*s.ExpiresAt)
}

// Status is a human word for the signer's state.
func (s Signer) Status(now time.Time) string {
	switch {
	case s.RevokedAt != nil:
		return "revoked"
	case s.ExpiresAt != nil && !now.Before(*s.ExpiresAt):
		return "expired"
	}
	return "active"
}

type storeFile struct {
	SchemaVersion int      `json:"schema_version"`
	Signers       []Signer `json:"signers"`
}

// Store is the file of trusted keys. Permission to run code here is exactly membership in it:
// the owner adds a contributor's public key, and revokes it to take the permission back.
type Store struct {
	Path string
	Now  func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func storeErr(err error, what, fix string) error {
	return diag.Wrap(err, diag.CodeTrustStore, what, "the trust store file is unreadable, malformed or unwritable", fix)
}

// Load returns the signers. A missing file is an empty store, which admits nothing.
func (s *Store) Load() ([]Signer, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, storeErr(err, "cannot read the trust store "+s.Path, "check its permissions")
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, storeErr(err, "the trust store "+s.Path+" is not valid JSON",
			"do not guess: restore it from a backup, or move it aside and re-add your keys with `bladerunner trust add`")
	}
	if f.SchemaVersion > storeSchema {
		return nil, storeErr(nil, "the trust store is from a newer bladerunner", "upgrade bladerunner")
	}
	return f.Signers, nil
}

func (s *Store) save(signers []Signer) error {
	data, err := json.MarshalIndent(storeFile{SchemaVersion: storeSchema, Signers: signers}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return storeErr(err, "cannot create "+dir, "check its permissions")
	}
	tmp, err := os.CreateTemp(dir, ".trust-*.tmp")
	if err != nil {
		return storeErr(err, "cannot write the trust store", "check "+dir)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return storeErr(err, "cannot protect the trust store", "check "+dir)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return storeErr(err, "cannot write the trust store", "check the disk space in "+dir)
	}
	if err := tmp.Close(); err != nil {
		return storeErr(err, "cannot write the trust store", "check the disk space in "+dir)
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Add trusts a public key under a name. expires is optional (the zero time means never).
func (s *Store) Add(name string, key PublicKey, expires time.Time) (Signer, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t\n") {
		return Signer{}, storeErr(nil, fmt.Sprintf("%q is not a usable name", name), "use a short name without spaces, such as the person's GitHub login")
	}
	signers, err := s.Load()
	if err != nil {
		return Signer{}, err
	}
	fp := key.Fingerprint()
	for _, e := range signers {
		switch {
		case strings.EqualFold(e.Name, name):
			return Signer{}, storeErr(nil, fmt.Sprintf("a signer named %q already exists", name), "choose another name, or revoke the old one first")
		case e.Fingerprint == fp && e.RevokedAt != nil:
			return Signer{}, storeErr(nil, fmt.Sprintf("this key (%s) was revoked as %q", fp, e.Name),
				"a revoked key is never trusted again: ask for a new key. (Edit the store by hand only if you are certain.)")
		case e.Fingerprint == fp:
			return Signer{}, storeErr(nil, fmt.Sprintf("this key (%s) is already trusted as %q", fp, e.Name), "nothing to do")
		}
	}
	entry := Signer{
		Name:        name,
		Key:         PublicKey{Blob: key.Blob}.AuthorizedKey(),
		Fingerprint: fp,
		AddedAt:     s.now().UTC(),
	}
	if !expires.IsZero() {
		if !expires.After(s.now()) {
			return Signer{}, storeErr(nil, "the expiry is in the past", "give a future date, or none for a key that does not expire")
		}
		e := expires.UTC()
		entry.ExpiresAt = &e
	}
	return entry, s.save(append(signers, entry))
}

// Revoke withdraws the permission of the signer with this name or fingerprint. The entry stays
// in the file, marked revoked, so the record of who once had permission is kept.
func (s *Store) Revoke(nameOrFingerprint string) (Signer, error) {
	signers, err := s.Load()
	if err != nil {
		return Signer{}, err
	}
	for i, e := range signers {
		if !strings.EqualFold(e.Name, nameOrFingerprint) && e.Fingerprint != nameOrFingerprint {
			continue
		}
		if e.RevokedAt == nil {
			t := s.now().UTC()
			signers[i].RevokedAt = &t
			if err := s.save(signers); err != nil {
				return Signer{}, err
			}
		}
		return signers[i], nil
	}
	return Signer{}, storeErr(nil, fmt.Sprintf("no signer named or fingerprinted %q", nameOrFingerprint), "see `bladerunner trust list`")
}

// Find returns the signer holding this key, whatever its state.
func (s *Store) Find(key PublicKey) (Signer, bool, error) {
	signers, err := s.Load()
	if err != nil {
		return Signer{}, false, err
	}
	fp := key.Fingerprint()
	for _, e := range signers {
		if e.Fingerprint == fp {
			return e, true, nil
		}
	}
	return Signer{}, false, nil
}

// Now exposes the store's clock to callers that judge expiry.
func (s *Store) Clock() time.Time { return s.now() }
