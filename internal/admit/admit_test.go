package admit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/testrig"
	"github.com/modullar/blade-runner/internal/trust"
)

var ctx = context.Background()

// Everything here runs the whole chain: real signed commits (made by real git and ssh-keygen),
// served over real HTTP by the fake GitHub, through the real client, verified by this machine.

func rig(t *testing.T) (*testrig.Rig, *admit.Admitter, map[string]testrig.RealCommit, map[string]string) {
	t.Helper()
	r := testrig.New(t, testrig.BaseConfig)
	commits := r.ServeCommits(testrig.Target)
	_, pub := testrig.RealFixtures(t)
	return r, &admit.Admitter{Provider: r.Env.Provider, Verifier: &trust.Verifier{Store: r.Env.Trust}}, commits, pub
}

func trustKey(t *testing.T, r *testrig.Rig, name, line string, expires time.Time) {
	t.Helper()
	k, err := trust.ParsePublicKey(line)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Env.Trust.Add(name, k, expires); err != nil {
		t.Fatal(err)
	}
}

func subject(c testrig.RealCommit) admit.Subject {
	return admit.Subject{Repository: testrig.Target, SHA: c.SHA}
}

func TestOnlyCommitsSignedByATrustedKeyAreAdmitted(t *testing.T) {
	r, a, commits, pub := rig(t)
	trustKey(t, r, "owner", pub["owner"], time.Time{})

	if v, err := a.Admit(ctx, subject(commits["owner"])); err != nil || v.Signer.Name != "owner" {
		t.Fatalf("the owner's commit: %v %+v", err, v)
	}
	for _, who := range []string{"mallory", "unsigned"} {
		_, err := a.Admit(ctx, subject(commits[who]))
		if diag.CodeOf(err) != diag.CodeNotAdmitted {
			t.Errorf("%s: err = %v, want BR-E067", who, err)
		}
	}
}

func TestGrantingAndWithdrawingPermissionChangesTheOutcome(t *testing.T) {
	r, a, commits, pub := rig(t)
	mallory := subject(commits["mallory"])

	if _, err := a.Admit(ctx, mallory); err == nil {
		t.Fatal("a contributor nobody has trusted was admitted")
	}
	trustKey(t, r, "mallory", pub["mallory"], time.Time{}) // the owner grants permission
	if v, err := a.Admit(ctx, mallory); err != nil || v.Signer.Name != "mallory" {
		t.Fatalf("after the owner adds their key: %v %+v", err, v)
	}
	if _, err := r.Env.Trust.Revoke("mallory"); err != nil { // and takes it back
		t.Fatal(err)
	}
	if _, err := a.Admit(ctx, mallory); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("after revoking: %v", err)
	}
}

func TestADishonestGitHubCannotSwapInOtherCode(t *testing.T) {
	r, a, commits, pub := rig(t)
	trustKey(t, r, "owner", pub["owner"], time.Time{})
	owner, mallory := commits["owner"], commits["mallory"]

	// GitHub is asked for mallory's commit id but answers with the OWNER's signed bytes: if the
	// machine took GitHub's word, malicious code would run under the owner's signature. It must
	// refuse, because the bytes do not hash to the id that was asked for.
	r.Srv.AddCommit(testrig.Target, mallory.SHA, owner.Payload, owner.Signature)
	if _, err := a.Admit(ctx, subject(mallory)); err == nil || !strings.Contains(err.Error(), "do not hash") {
		t.Fatalf("a swapped commit body was not caught: %v", err)
	}

	// GitHub answers with mallory's unsigned-by-anyone-trusted bytes plus a doctored payload.
	edited := strings.Replace(owner.Payload, "signed by the owner", "run my backdoor", 1)
	r.Srv.AddCommit(testrig.Target, owner.SHA, edited, owner.Signature)
	if _, err := a.Admit(ctx, subject(owner)); err == nil {
		t.Fatal("edited code under the owner's signature was admitted")
	}
}

func TestProviderFailuresAreNeverAdmissions(t *testing.T) {
	r, a, commits, pub := rig(t)
	trustKey(t, r, "owner", pub["owner"], time.Time{})

	for name, c := range map[string]admit.Subject{
		"a commit GitHub has never heard of": {Repository: testrig.Target, SHA: strings.Repeat("a", 40)},
		"a repository that does not exist":   {Repository: "ghost/none", SHA: commits["owner"].SHA},
		"a short id":                         {Repository: testrig.Target, SHA: "257d325"},
		"a ref name":                         {Repository: testrig.Target, SHA: "main"},
		"a path trick":                       {Repository: testrig.Target, SHA: "../../../../etc/passwd"},
	} {
		if _, err := a.Admit(ctx, c); err == nil {
			t.Errorf("%s was admitted", name)
		}
	}
	r.Srv.Down = true
	if _, err := a.Admit(ctx, subject(commits["owner"])); err == nil {
		t.Error("with GitHub unreachable, nothing may be admitted")
	}
}

func TestAnEmptyOrMissingTrustStoreAdmitsNothing(t *testing.T) {
	_, a, commits, _ := rig(t)
	for who, c := range commits {
		if _, err := a.Admit(ctx, subject(c)); err == nil {
			t.Errorf("%s was admitted with nobody trusted", who)
		}
	}
}

// brokenProvider is a provider whose commit lookup fails with a plain error (no diag code), as a
// transport failure inside a custom Provider would. Everything else is the real client.
type brokenProvider struct {
	provider.Provider
	err error
}

func (b brokenProvider) Commit(context.Context, string, string) (provider.Commit, error) {
	return provider.Commit{}, b.err
}

func TestAnAdmissionFailureIsToldFromARefusalByItsCode(t *testing.T) {
	r, a, commits, pub := rig(t)
	trustKey(t, r, "owner", pub["owner"], time.Time{})

	// GitHub down (503): "could not ask" is BR-E022, never BR-E067 (callers read E067 as "this
	// commit may not run" and would cancel or refuse on an outage).
	r.Srv.Down = true
	if _, err := a.Admit(ctx, subject(commits["owner"])); diag.CodeOf(err) != diag.CodeGitHubUnavailable {
		t.Errorf("GitHub down: %v, want BR-E022", err)
	}
	r.Srv.Down = false

	// A repository the token cannot read: a 404 that says nothing about the commit.
	if _, err := a.Admit(ctx, admit.Subject{Repository: "ghost/hidden", SHA: commits["owner"].SHA}); err == nil || diag.CodeOf(err) == diag.CodeNotAdmitted {
		t.Errorf("a repository the token cannot read: %v, want an error that is not BR-E067", err)
	}

	// A commit GitHub has no object for, in a repository it can read: a real refusal, BR-E067.
	if _, err := a.Admit(ctx, admit.Subject{Repository: testrig.Target, SHA: strings.Repeat("c", 40)}); diag.CodeOf(err) != diag.CodeNotAdmitted {
		t.Errorf("a commit that does not exist: %v, want BR-E067", err)
	}

	// A provider error with no code at all is wrapped as BR-E022, not passed on bare.
	b := &admit.Admitter{Provider: brokenProvider{Provider: r.Env.Provider, err: errors.New("connection reset")}, Verifier: a.Verifier}
	if _, err := b.Admit(ctx, subject(commits["owner"])); diag.CodeOf(err) != diag.CodeGitHubUnavailable {
		t.Errorf("an uncoded provider error: %v, want BR-E022", err)
	}
	// ...and a coded refusal from a provider is passed on untouched.
	refusal := diag.New(diag.CodeNotAdmitted, "no such commit", "x", "y")
	b = &admit.Admitter{Provider: brokenProvider{Provider: r.Env.Provider, err: refusal}, Verifier: a.Verifier}
	if _, err := b.Admit(ctx, subject(commits["owner"])); diag.CodeOf(err) != diag.CodeNotAdmitted {
		t.Errorf("a coded refusal: %v, want BR-E067", err)
	}
}

func commitRequests(r *testrig.Rig) int {
	n := 0
	for _, req := range r.Srv.Requests() {
		if strings.Contains(req, "/git/commits/") {
			n++
		}
	}
	return n
}

func TestACacheSavesTheFetchButNeverTheTrustDecision(t *testing.T) {
	r, a, commits, pub := rig(t)
	trustKey(t, r, "owner", pub["owner"], time.Time{})
	a.Cache = admit.NewCommitCache(10)
	s := subject(commits["owner"])

	for i := 0; i < 3; i++ {
		if v, err := a.Admit(ctx, s); err != nil || v.Signer.Name != "owner" {
			t.Fatalf("admit %d: %v %+v", i, err, v)
		}
	}
	if n := commitRequests(r); n != 1 {
		t.Errorf("commit requests = %d, want 1: the same commit is fetched once", n)
	}
	if _, err := r.Env.Trust.Revoke("owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Admit(ctx, s); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("a revocation must apply at once even for a cached commit: %v", err)
	}
	if n := commitRequests(r); n != 1 {
		t.Errorf("the refusal after revocation cost %d requests", n)
	}
}

func TestACacheNeverKeepsWhatItShouldNotTrust(t *testing.T) {
	r, a, commits, pub := rig(t)
	trustKey(t, r, "owner", pub["owner"], time.Time{})
	a.Cache = admit.NewCommitCache(10)
	o, m := commits["owner"], commits["mallory"]

	// A server that answers the owner's id with mallory's bytes: refused, and not remembered, so
	// the honest answer is accepted as soon as the server gives it.
	r.Srv.AddCommit(testrig.Target, o.SHA, m.Payload, m.Signature)
	if _, err := a.Admit(ctx, subject(o)); diag.CodeOf(err) != diag.CodeNotAdmitted {
		t.Fatalf("dishonest answer: %v", err)
	}
	if a.Cache.Len() != 0 {
		t.Error("bytes that are not the commit were cached")
	}
	r.Srv.AddCommit(testrig.Target, o.SHA, o.Payload, o.Signature)
	if _, err := a.Admit(ctx, subject(o)); err != nil {
		t.Fatalf("after the server corrected itself: %v", err)
	}

	// An unsigned commit is refused every time and not cached.
	before := a.Cache.Len()
	for i := 0; i < 2; i++ {
		if _, err := a.Admit(ctx, subject(commits["unsigned"])); err == nil {
			t.Fatal("unsigned commit admitted")
		}
	}
	if a.Cache.Len() != before {
		t.Error("an unsigned commit was cached")
	}
}

func TestTheCacheIsBounded(t *testing.T) {
	r, a, commits, pub := rig(t)
	trustKey(t, r, "owner", pub["owner"], time.Time{})
	trustKey(t, r, "mallory", pub["mallory"], time.Time{})
	a.Cache = admit.NewCommitCache(1)
	for _, who := range []string{"owner", "mallory", "owner"} {
		if _, err := a.Admit(ctx, subject(commits[who])); err != nil {
			t.Fatal(err)
		}
	}
	if a.Cache.Len() != 1 {
		t.Errorf("cache holds %d, want at most 1", a.Cache.Len())
	}
	if n := commitRequests(r); n != 3 {
		t.Errorf("requests = %d: a size-1 cache evicts the older commit, so each of three alternating commits is fetched", n)
	}
}
