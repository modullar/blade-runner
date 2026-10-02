package testrig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/trust"
)

// GitWorld is a throwaway git repository in which tests make REAL commits with the real `git`
// and the real `ssh-keygen`: signed by a contributor's own freshly generated ed25519 key, or not
// signed at all. It never reads the machine's git configuration or keys: the global and system
// config are switched off, HOME is a temp directory, and every signature names an explicit
// private key file and `gpg.ssh.program=ssh-keygen`, so the sandbox's own signing key can never
// be used by accident.
type GitWorld struct {
	t    testing.TB
	dir  string // holds keys and the repository
	repo string
	keys map[string]string // contributor -> private key file
}

// NewGitWorld makes an empty repository. The test is skipped when git or ssh-keygen is missing.
func NewGitWorld(t testing.TB) *GitWorld {
	t.Helper()
	for _, tool := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	w := &GitWorld{t: t, dir: dir, repo: filepath.Join(dir, "repo"), keys: map[string]string{}}
	if err := os.Mkdir(w.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	w.git("init", "-q", "-b", "main")
	return w
}

func (w *GitWorld) env() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME="+w.dir, "GIT_TERMINAL_PROMPT=0")
}

func (w *GitWorld) git(args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", w.repo}, args...)...)
	cmd.Env = w.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Key returns who's public key line, generating the key pair the first time.
func (w *GitWorld) Key(who string) string {
	w.t.Helper()
	priv, ok := w.keys[who]
	if !ok {
		priv = filepath.Join(w.dir, "key-"+who)
		out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", who+"@example", "-f", priv).CombinedOutput()
		if err != nil {
			w.t.Fatalf("ssh-keygen: %v\n%s", err, out)
		}
		w.keys[who] = priv
	}
	pub, err := os.ReadFile(priv + ".pub")
	if err != nil {
		w.t.Fatal(err)
	}
	return strings.TrimSpace(string(pub))
}

// Checkout switches to a branch, creating it at the current commit when create is set.
func (w *GitWorld) Checkout(branch string, create bool) {
	w.t.Helper()
	if create {
		w.git("checkout", "-q", "-b", branch)
		return
	}
	w.git("checkout", "-q", branch)
}

// Detach checks out a commit by id, so a merge can be made onto it.
func (w *GitWorld) Detach(sha string) { w.t.Helper(); w.git("checkout", "-q", "--detach", sha) }

// Commit makes a commit changing one file. who == "" leaves it unsigned; otherwise it is signed
// by who's key through the real `git commit -S`.
func (w *GitWorld) Commit(who, file, content, msg string) RealCommit {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.repo, file), []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
	w.git("add", file)
	args := []string{"-c", "user.name=" + orUnknown(who), "-c", "user.email=" + orUnknown(who) + "@example", "-c", "commit.gpgsign=false"}
	if who == "" {
		args = append(args, "commit", "-q", "--no-gpg-sign", "-m", msg)
	} else {
		w.Key(who)
		args = append(args, "-c", "gpg.format=ssh", "-c", "gpg.ssh.program=ssh-keygen", "-c", "user.signingkey="+w.keys[who], "commit", "-q", "-S", "-m", msg)
	}
	w.git(args...)
	return w.Head()
}

func orUnknown(who string) string {
	if who == "" {
		return "unsigned"
	}
	return who
}

// MergeCommit makes GitHub's kind of merge: an UNSIGNED merge commit of head onto the current
// commit, with the current commit as first parent and head as second.
func (w *GitWorld) MergeCommit(head string) RealCommit {
	w.t.Helper()
	w.git("-c", "user.name=github", "-c", "user.email=noreply@example", "-c", "commit.gpgsign=false", "merge", "-q", "--no-ff", "--no-gpg-sign", "-m", "merge", head)
	return w.Head()
}

// Head returns the current commit as GitHub's API would serve it.
func (w *GitWorld) Head() RealCommit {
	w.t.Helper()
	sha := w.git("rev-parse", "HEAD")
	cmd := exec.Command("git", "-C", w.repo, "cat-file", "commit", sha)
	cmd.Env = w.env()
	raw, err := cmd.Output()
	if err != nil {
		w.t.Fatal(err)
	}
	payload, sig, err := trust.SplitSignedCommit(raw)
	if err != nil {
		w.t.Fatal(err)
	}
	c := RealCommit{SHA: sha, Payload: string(payload), Signature: sig}
	for _, l := range strings.Split(string(payload), "\n") {
		if l == "" {
			break
		}
		if p, ok := strings.CutPrefix(l, "parent "); ok {
			c.Parents = append(c.Parents, p)
		}
	}
	return c
}
