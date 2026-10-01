package trust

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
)

// The fixtures in testdata/real were made by the real `git commit -S` with real ssh-keygen
// ed25519 keys, then read back with `git cat-file commit`: nothing here was built by this
// package, so a pass means the verifier agrees with the real tools, not just with itself.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "real", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func shas(t *testing.T) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(fixture(t, "shas.txt"))), "\n") {
		f := strings.Fields(l)
		m[f[0]] = f[1]
	}
	return m
}

func key(t *testing.T, file string) PublicKey {
	t.Helper()
	k, err := ParsePublicKey(string(fixture(t, file)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func commit(t *testing.T, who string) Commit {
	t.Helper()
	payload, sig, err := SplitSignedCommit(fixture(t, who+"-commit.raw"))
	if err != nil {
		t.Fatal(err)
	}
	return Commit{SHA: shas(t)[who], Payload: string(payload), Signature: sig}
}

var clock = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	return &Store{Path: filepath.Join(t.TempDir(), "trust", "trust.json"), Now: func() time.Time { return clock }}
}

func trustOwner(t *testing.T) (*Store, *Verifier) {
	s := newStore(t)
	if _, err := s.Add("owner", key(t, "owner.pub"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	return s, &Verifier{Store: s}
}

// ---- keys ----------------------------------------------------------------------------

func TestFingerprintMatchesSSHKeygen(t *testing.T) {
	for _, who := range []string{"owner", "mallory"} {
		want := strings.TrimSpace(string(fixture(t, who+".fingerprint")))
		if got := key(t, who+".pub").Fingerprint(); got != want {
			t.Errorf("%s: fingerprint %s, ssh-keygen says %s", who, got, want)
		}
	}
}

func TestParsePublicKeyRefusesWhatItCannotVerify(t *testing.T) {
	for name, line := range map[string]string{
		"empty":       "",
		"one field":   "ssh-ed25519",
		"rsa":         "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7 alice",
		"ecdsa":       "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTY= alice",
		"bad base64":  "ssh-ed25519 !!!notbase64!!!",
		"truncated":   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA",
		"a GPG block": "-----BEGIN PGP PUBLIC KEY BLOCK-----",
	} {
		if _, err := ParsePublicKey(line); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	k := key(t, "owner.pub")
	if k.Comment != "owner@example" || !strings.HasPrefix(k.AuthorizedKey(), "ssh-ed25519 ") {
		t.Errorf("key = %+v", k)
	}
}

// ---- commits -------------------------------------------------------------------------

func TestRealCommitIdIsRecomputedFromPayloadAndSignature(t *testing.T) {
	for _, who := range []string{"owner", "mallory"} {
		c := commit(t, who)
		id, err := ObjectID([]byte(c.Payload), c.Signature)
		if err != nil || id != c.SHA {
			t.Errorf("%s: recomputed %s (%v), git says %s: the split/join must reproduce git's own id", who, id, err, c.SHA)
		}
	}
	unsigned := fixture(t, "unsigned-commit.raw")
	payload, sig, _ := SplitSignedCommit(unsigned)
	if sig != "" || string(payload) != string(unsigned) {
		t.Error("an unsigned commit is its own payload")
	}
}

func TestTheOwnersSignedCommitIsAdmitted(t *testing.T) {
	_, v := trustOwner(t)
	got, err := v.Verify(commit(t, "owner"))
	if err != nil {
		t.Fatalf("a real commit signed by a trusted key was refused: %v", err)
	}
	if got.Signer.Name != "owner" || got.Signer.Fingerprint != key(t, "owner.pub").Fingerprint() {
		t.Errorf("verdict = %+v", got)
	}
}

func TestEveryOtherCommitIsRefused(t *testing.T) {
	_, v := trustOwner(t)
	owner := commit(t, "owner")
	tests := []struct {
		name   string
		commit func() Commit
		want   string
	}{
		{"signed by a key nobody trusts", func() Commit { return commit(t, "mallory") }, "does not trust"},
		{"unsigned", func() Commit {
			p, _, _ := SplitSignedCommit(fixture(t, "unsigned-commit.raw"))
			return Commit{SHA: shas(t)["unsigned"], Payload: string(p)}
		}, "not signed"},
		{"the message was changed after signing", func() Commit {
			c := owner
			c.Payload = strings.Replace(c.Payload, "signed by the owner", "signed by the owner (edited)", 1)
			return c
		}, "do not hash"},
		{"a different tree under the owner's signature", func() Commit {
			c := owner
			c.Payload = strings.Replace(c.Payload, "tree ", "tree 0000", 1)
			return c
		}, "do not hash"},
		{"the owner's signature on mallory's commit", func() Commit {
			m := commit(t, "mallory")
			m.Signature = owner.Signature
			return m
		}, ""},
		{"someone else's commit id for the owner's bytes", func() Commit {
			c := owner
			c.SHA = shas(t)["mallory"]
			return c
		}, "do not hash"},
		{"a character changed inside the signature", func() Commit {
			c := owner
			lines := strings.Split(c.Signature, "\n")
			i := len(lines) - 2 // the last base64 line, before the END marker
			flipped := "A"
			if lines[i][0] == 'A' {
				flipped = "B"
			}
			lines[i] = flipped + lines[i][1:]
			c.Signature = strings.Join(lines, "\n")
			if c.Signature == owner.Signature {
				t.Fatal("the test did not change the signature: it would prove nothing")
			}
			return c
		}, ""},
		{"a GPG signature", func() Commit {
			c := owner
			c.Signature = "-----BEGIN PGP SIGNATURE-----\n\niQEzBAABCAAdFiEE\n-----END PGP SIGNATURE-----"
			return c
		}, ""},
		{"no id at all", func() Commit { c := owner; c.SHA = ""; return c }, "do not hash"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v.Verify(tc.commit())
			if err == nil {
				t.Fatalf("was ADMITTED (signer %q)", got.Signer.Name)
			}
			if diag.CodeOf(err) != diag.CodeNotAdmitted {
				t.Errorf("code = %q, want BR-E067: %v", diag.CodeOf(err), err)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q:\n%v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "fix:") {
				t.Errorf("a refusal must say what to do:\n%v", err)
			}
		})
	}
}

func TestPermissionFollowsTheStore(t *testing.T) {
	s := newStore(t)
	v := &Verifier{Store: s}
	mallory := commit(t, "mallory")

	if _, err := v.Verify(mallory); err == nil {
		t.Fatal("an empty store must admit nothing")
	}
	if _, err := s.Add("mallory", key(t, "mallory.pub"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, err := v.Verify(mallory); err != nil || got.Signer.Name != "mallory" {
		t.Fatalf("after granting permission: %v %+v", err, got)
	}
	if _, err := s.Revoke("mallory"); err != nil {
		t.Fatal(err)
	}
	_, err := v.Verify(mallory)
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("after revoking: %v, want a refusal saying the signer is revoked", err)
	}
}

func TestExpiredPermissionIsRefused(t *testing.T) {
	now := clock
	s := &Store{Path: filepath.Join(t.TempDir(), "trust.json"), Now: func() time.Time { return now }}
	if _, err := s.Add("owner", key(t, "owner.pub"), clock.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Store: s}
	if _, err := v.Verify(commit(t, "owner")); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	now = clock.Add(25 * time.Hour)
	if _, err := v.Verify(commit(t, "owner")); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("after expiry: %v", err)
	}
}

// ---- the real tools agree ------------------------------------------------------------

func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
}

func run(t *testing.T, dir string, stdin string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestFreshCommitsFromTheRealToolsAreAdmitted(t *testing.T) {
	needTools(t)
	dir := t.TempDir()
	run(t, dir, "", "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", "k", "-C", "live@example")
	run(t, dir, "", "git", "init", "-q", "r")
	repo := filepath.Join(dir, "r")
	pub, _ := os.ReadFile(filepath.Join(dir, "k.pub"))
	pk, err := ParsePublicKey(string(pub))
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if _, err := s.Add("live", pk, time.Time{}); err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Store: s}

	// Commits with different shapes: a merge-free history, a multi-line message, non-ASCII, a
	// second parent-less commit, an empty message body. Each must round-trip through real git.
	for i, msg := range []string{"first", "multi\n\nline body\nwith lines", "unicode: héllo ✓ 日本語", "x"} {
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte(msg+"\n"+string(rune('a'+i))), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, repo, "", "git", "add", "f.txt")
		run(t, repo, "", "git", "-c", "user.name=L", "-c", "user.email=l@e", "-c", "gpg.format=ssh", "-c", "gpg.ssh.program=ssh-keygen", "-c", "commit.gpgsign=false",
			"-c", "user.signingkey="+filepath.Join(dir, "k"), "commit", "-q", "-S", "-m", msg)
		sha := run(t, repo, "", "git", "rev-parse", "HEAD")
		raw := run(t, repo, "", "git", "cat-file", "commit", sha)
		payload, sig, err := SplitSignedCommit([]byte(raw + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		// cat-file output via run() was trimmed; restore git's exact trailing newline handling
		rawExact, _ := exec.Command("git", "-C", repo, "cat-file", "commit", sha).Output()
		payload, sig, _ = SplitSignedCommit(rawExact)
		if _, err := v.Verify(Commit{SHA: sha, Payload: string(payload), Signature: sig}); err != nil {
			t.Errorf("commit %d (%q) made by real git was refused: %v", i, msg, err)
		}

		// And the real ssh-keygen agrees that this payload and signature belong together.
		sigFile := filepath.Join(dir, "sig")
		_ = os.WriteFile(sigFile, []byte(sig+"\n"), 0o600)
		cmd := exec.Command("ssh-keygen", "-Y", "check-novalidate", "-n", "git", "-s", sigFile)
		cmd.Stdin = strings.NewReader(string(payload))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("ssh-keygen itself rejects what we split out of commit %d: %v\n%s", i, err, out)
		}
	}
}

func TestSignaturesFromAnotherNamespaceAreRefused(t *testing.T) {
	needTools(t)
	dir := t.TempDir()
	run(t, dir, "", "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", "k")
	pub, _ := os.ReadFile(filepath.Join(dir, "k.pub"))
	pk, _ := ParsePublicKey(string(pub))
	msg := "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nnot a commit signature\n"
	// A valid signature made for a FILE, not for git: it must not be accepted as a commit signature.
	if err := os.WriteFile(filepath.Join(dir, "msg"), []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "", "ssh-keygen", "-Y", "sign", "-n", "file", "-f", filepath.Join(dir, "k"), filepath.Join(dir, "msg"))
	sig, _ := os.ReadFile(filepath.Join(dir, "msg.sig"))

	s := newStore(t)
	_, _ = s.Add("k", pk, time.Time{})
	id, _ := ObjectID([]byte(msg), strings.TrimSpace(string(sig)))
	_, err := (&Verifier{Store: s}).Verify(Commit{SHA: id, Payload: msg, Signature: strings.TrimSpace(string(sig))})
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("a signature made for another purpose must be refused: %v", err)
	}
}

func TestRSASignedCommitsAreRefusedNotMisread(t *testing.T) {
	needTools(t)
	dir := t.TempDir()
	run(t, dir, "", "ssh-keygen", "-q", "-t", "rsa", "-b", "2048", "-N", "", "-f", "k")
	run(t, dir, "", "git", "init", "-q", "r")
	repo := filepath.Join(dir, "r")
	_ = os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644)
	run(t, repo, "", "git", "add", "f")
	run(t, repo, "", "git", "-c", "user.name=L", "-c", "user.email=l@e", "-c", "gpg.format=ssh", "-c", "gpg.ssh.program=ssh-keygen", "-c", "commit.gpgsign=false", "-c", "user.signingkey="+filepath.Join(dir, "k"), "commit", "-q", "-S", "-m", "rsa")
	sha := run(t, repo, "", "git", "rev-parse", "HEAD")
	rawExact, _ := exec.Command("git", "-C", repo, "cat-file", "commit", sha).Output()
	payload, sig, _ := SplitSignedCommit(rawExact)
	_, err := (&Verifier{Store: newStore(t)}).Verify(Commit{SHA: sha, Payload: string(payload), Signature: sig})
	if err == nil || !strings.Contains(err.Error(), "ssh-ed25519") {
		t.Errorf("an RSA signature must be refused with a message naming the supported type: %v", err)
	}
}

// ---- store ---------------------------------------------------------------------------

func TestStoreAddListRevokePersist(t *testing.T) {
	s := newStore(t)
	owner, err := s.Add("owner", key(t, "owner.pub"), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if owner.Fingerprint != key(t, "owner.pub").Fingerprint() || strings.Contains(owner.Key, "owner@example") {
		t.Errorf("entry = %+v (the comment must not be stored as if it were identity)", owner)
	}
	if _, err := s.Add("mallory", key(t, "mallory.pub"), clock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	reloaded := &Store{Path: s.Path, Now: s.Now}
	all, err := reloaded.Load()
	if err != nil || len(all) != 2 || all[1].ExpiresAt == nil {
		t.Fatalf("reload: %+v %v", all, err)
	}
	if info, _ := os.Stat(s.Path); info.Mode().Perm() != 0o600 {
		t.Errorf("trust store mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := reloaded.Revoke(all[1].Fingerprint); err != nil { // by fingerprint
		t.Fatal(err)
	}
	after, _ := reloaded.Load()
	if after[1].RevokedAt == nil || after[0].RevokedAt != nil {
		t.Errorf("only mallory should be revoked: %+v", after)
	}
	if _, err := reloaded.Revoke("mallory"); err != nil { // idempotent
		t.Errorf("revoking twice: %v", err)
	}
	if _, err := reloaded.Revoke("nobody"); diag.CodeOf(err) != diag.CodeTrustStore {
		t.Errorf("revoking an unknown signer: %v", err)
	}
}

func TestStoreRefusesRiskyAdds(t *testing.T) {
	s := newStore(t)
	if _, err := s.Add("owner", key(t, "owner.pub"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	for name, add := range map[string]func() error{
		"same name":          func() error { _, e := s.Add("Owner", key(t, "mallory.pub"), time.Time{}); return e },
		"same key, new name": func() error { _, e := s.Add("second", key(t, "owner.pub"), time.Time{}); return e },
		"empty name":         func() error { _, e := s.Add("", key(t, "mallory.pub"), time.Time{}); return e },
		"name with a space":  func() error { _, e := s.Add("a b", key(t, "mallory.pub"), time.Time{}); return e },
		"expiry in the past": func() error { _, e := s.Add("m", key(t, "mallory.pub"), clock.Add(-time.Hour)); return e },
	} {
		if err := add(); diag.CodeOf(err) != diag.CodeTrustStore {
			t.Errorf("%s: %v, want BR-E066", name, err)
		}
	}
	if _, err := s.Revoke("owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("owner2", key(t, "owner.pub"), time.Time{}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("a revoked key must never be quietly re-trusted: %v", err)
	}
}

func TestCorruptStoreIsAnErrorNeverAnEmptyStore(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"{ nope", `{"schema_version": 9, "signers": []}`} {
		_ = os.WriteFile(s.Path, []byte(content), 0o600)
		if _, err := s.Load(); diag.CodeOf(err) != diag.CodeTrustStore {
			t.Errorf("%q: err = %v, want BR-E066", content, err)
		}
		if _, err := (&Verifier{Store: s}).Verify(commit(t, "owner")); err == nil {
			t.Errorf("%q: a corrupt store must not admit anything", content)
		}
		if _, err := s.Add("x", key(t, "owner.pub"), time.Time{}); err == nil {
			t.Errorf("%q: Add must not overwrite a store it cannot read", content)
		}
	}
	if data, _ := os.ReadFile(s.Path); string(data) != `{"schema_version": 9, "signers": []}` {
		t.Error("an unreadable store must be left untouched for the user to inspect")
	}
}

// An attacker who controls the bytes can make the commit id self-consistent with a forged
// signature (the id is just a hash of what they supply). Only the signature check itself can
// stop that, so these cases recompute the id to get past the binding check.
func TestForgedSignaturesWithAConsistentIdAreRefusedByTheSignatureCheck(t *testing.T) {
	_, v := trustOwner(t)
	owner, mallory := commit(t, "owner"), commit(t, "mallory")
	consistent := func(c Commit) Commit {
		id, err := ObjectID([]byte(c.Payload), c.Signature)
		if err != nil {
			t.Fatal(err)
		}
		c.SHA = id
		return c
	}

	t.Run("a valid owner signature attached to other code", func(t *testing.T) {
		forged := consistent(Commit{Payload: mallory.Payload, Signature: owner.Signature})
		if forged.SHA == owner.SHA || forged.SHA == mallory.SHA {
			t.Fatal("the forgery must have its own id")
		}
		if _, err := v.Verify(forged); err == nil || !strings.Contains(err.Error(), "does not match the commit") {
			t.Fatalf("the owner's signature over different code must not verify: %v", err)
		}
	})
	t.Run("the owner's signature with one character changed", func(t *testing.T) {
		lines := strings.Split(owner.Signature, "\n")
		i := len(lines) - 2
		first := "A"
		if lines[i][0] == 'A' {
			first = "B"
		}
		lines[i] = first + lines[i][1:]
		forged := consistent(Commit{Payload: owner.Payload, Signature: strings.Join(lines, "\n")})
		if _, err := v.Verify(forged); err == nil {
			t.Fatal("a corrupted signature was admitted")
		}
	})
	t.Run("a payload edited and the old signature kept", func(t *testing.T) {
		forged := consistent(Commit{Payload: strings.Replace(owner.Payload, "signed by the owner", "backdoor added", 1), Signature: owner.Signature})
		if _, err := v.Verify(forged); err == nil || !strings.Contains(err.Error(), "does not match the commit") {
			t.Fatalf("edited code under the old signature: %v", err)
		}
	})
}
