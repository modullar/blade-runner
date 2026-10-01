package supervisor_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/supervisor"
)

func TestRestartRemovesOnlyRunnersNamedByThisSupervisorsExactPattern(t *testing.T) {
	// This supervisor is "mini". A supervisor called "mini-2" names its runners
	// br-jit-mini-2-<12 hex>, which START WITH br-jit-mini-: a prefix match would remove the
	// runner another supervisor is using right now.
	r := newRig(t)
	own := []string{"br-jit-mini-0123456789ab", "br-jit-mini-ffffffffffff"}
	foreign := []string{
		"br-jit-mini-2-0123456789ab",     // supervisor "mini-2"
		"br-jit-mini-2",                  // not even a generated name
		"br-jit-mini-0123456789ab-extra", // a longer name that merely starts like ours
		"br-jit-mini-0123456789AB",       // ids are lower-case hex
		"br-jit-mini-0123456789a",        // 11 digits
		"br-jit-mini-0123456789abc",      // 13 digits
		"br-jit-mini-",                   // no id at all
		"mini",                           // the owner's permanent runner
		"xbr-jit-mini-0123456789ab",      // does not start with the prefix
	}
	for _, n := range append(append([]string(nil), own...), foreign...) {
		r.srv.AddRunner(repo, n, []string{"self-hosted"}, false)
	}
	if err := r.sup.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, rn := range r.srv.Runners(repo) {
		left = append(left, rn.Name)
	}
	sort.Strings(left)
	want := append([]string(nil), foreign...)
	sort.Strings(want)
	if strings.Join(left, ",") != strings.Join(want, ",") {
		t.Errorf("runners left = %v\nwant exactly the foreign ones = %v", left, want)
	}
	if st := r.entriesOfKind(supervisor.KindStartup); len(st) != 1 || !strings.Contains(st[0].Message, "2 leftover runner") {
		t.Errorf("startup entry = %+v", st)
	}
}

func TestTwoSupervisorsWhoseNamesShareAPrefixLeaveEachOthersRunnersAlone(t *testing.T) {
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.RunnerName = "mini-2" }))
	r.srv.AddRunner(repo, "br-jit-mini-0123456789ab", []string{"self-hosted"}, true) // supervisor "mini"'s, busy with a job
	r.srv.AddRunner(repo, "br-jit-mini-2-0123456789ab", []string{"self-hosted"}, false)
	if err := r.sup.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	rs := r.srv.Runners(repo)
	if len(rs) != 1 || rs[0].Name != "br-jit-mini-0123456789ab" {
		t.Errorf("runners after reconcile = %+v: only mini-2's own may go", rs)
	}
}
