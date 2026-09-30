// Package provider is the CI-provider extension point (spec section 4.1). v1 implements
// only GitHub (provider/github); a second provider needs no core change.
package provider

import "context"

// Scope is where a runner registers: one repository or one organization.
type Scope struct {
	Kind         string // "repo" or "org"
	Repository   string // OWNER/REPO when Kind is repo
	Organization string // when Kind is org
}

// Target is the human name of the scope.
func (s Scope) Target() string {
	if s.Kind == "org" {
		return s.Organization
	}
	return s.Repository
}

// Visibility is a repository's exposure. Internal repositories count as private.
type Visibility int

const (
	VisibilityUnknown Visibility = iota
	Private
	Public
)

// Runner is a registered runner as the provider reports it.
type Runner struct {
	ID     int64
	Name   string
	Status string // "online" or "offline"
	Busy   bool
	Labels []string
}

// Online reports whether the provider sees the runner as connected.
func (r Runner) Online() bool { return r.Status == "online" }

// Release is a downloadable runner build with the checksum published for it.
type Release struct {
	Version  string // without a leading "v"
	Filename string
	URL      string
	SHA256   string // lower-case hex
}

// Provider is everything Blade Runner needs from a CI provider.
type Provider interface {
	// CheckAuth proves the token is accepted and can manage runners in scope.
	CheckAuth(ctx context.Context, scope Scope) error
	// Visibility reports whether OWNER/REPO is public.
	Visibility(ctx context.Context, repository string) (Visibility, error)
	// RegistrationToken mints a short-lived token for configuring a runner. The caller must
	// not persist it.
	RegistrationToken(ctx context.Context, scope Scope) (string, error)
	// Release resolves a runner build for an OS and architecture (Go's names). An empty
	// version means the latest; a version pins one, so a reinstall never drifts from the
	// release recorded in state.
	Release(ctx context.Context, goos, goarch, version string) (Release, error)
	// ForkApprovalPolicy returns the repository's "approval for running fork pull request
	// workflows" setting (see docs/decisions/0005). The endpoint is an unverified assumption.
	ForkApprovalPolicy(ctx context.Context, repository string) (string, error)
	// Commit returns a commit's signed bytes and signature, exactly as stored. The caller does
	// not trust them: it recomputes the commit id and checks the signature itself.
	Commit(ctx context.Context, repository, sha string) (Commit, error)
	ListRunners(ctx context.Context, scope Scope) ([]Runner, error)
	RemoveRunner(ctx context.Context, scope Scope, id int64) error
	// RegistrationURL is the URL handed to the runner's configure script.
	RegistrationURL(scope Scope) string
}

// Commit is what the provider reports for a commit id. Payload is the commit object without
// its signature (what was signed) and Signature the armored signature, or "" if unsigned.
// Nothing in it is verified by the provider's word: see internal/trust.
type Commit struct {
	SHA       string
	Payload   string
	Signature string
}

// StrictForkApproval is the policy under which every outside contributor's workflow run
// waits for a maintainer's approval.
const StrictForkApproval = "all_external_contributors"
