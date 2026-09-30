package admit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/diag"
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
