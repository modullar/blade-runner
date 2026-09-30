// Package diag is Blade Runner's error UX: every failure says what failed, the likely
// cause, the exact fix, and a docs URL keyed by a stable error code (spec section 5).
package diag

import (
	"errors"
	"fmt"
	"strings"
)

// Stable error codes. docs/errors/README.md carries one section per code; a test fails if
// a code is declared here without one.
const (
	CodeConfigInvalid       = "BR-E001"
	CodeConfigMissing       = "BR-E002"
	CodeConfigExists        = "BR-E003"
	CodeBusy                = "BR-E004"
	CodePrereqMissing       = "BR-E010"
	CodeUnsupportedPlatform = "BR-E011"
	CodeRunningAsRoot       = "BR-E012"
	CodeTokenMissing        = "BR-E020"
	CodeTokenRejected       = "BR-E021"
	CodeGitHubUnavailable   = "BR-E022"
	CodeTokenStoreFailed    = "BR-E023"
	CodeReleaseUnresolved   = "BR-E030"
	CodeChecksumMismatch    = "BR-E031"
	CodeDownloadFailed      = "BR-E032"
	CodeRegistrationFailed  = "BR-E040"
	CodeRunnerNotRegistered = "BR-E041"
	CodeServiceInstall      = "BR-E050"
	CodeServiceNotRunning   = "BR-E051"
	CodeRunnerOffline       = "BR-E052"
	CodePublicRepoRefused   = "BR-E060"
	CodeVisibilityUnknown   = "BR-E061"
	CodeWorkflowPolicy      = "BR-E062"
	CodeRepoMismatch        = "BR-E063"
	CodeForkApproval        = "BR-E064"
	CodeJobHook             = "BR-E065"
	CodeTrustStore          = "BR-E066"
	CodeNotAdmitted         = "BR-E067"
	CodeIsolation           = "BR-E068"
	CodeDiskLow             = "BR-E070"
	CodeCLIOutdated         = "BR-E071"
	CodeStateCorrupt        = "BR-E080"
	CodeConfirmRequired     = "BR-E090"
	CodeLocalPlacement      = "BR-E100"
)

// All lists every declared code, for the docs-coverage test.
func All() []string {
	return []string{
		CodeConfigInvalid, CodeConfigMissing, CodeConfigExists, CodeBusy, CodePrereqMissing, CodeUnsupportedPlatform,
		CodeRunningAsRoot, CodeTokenMissing, CodeTokenRejected, CodeGitHubUnavailable,
		CodeTokenStoreFailed, CodeReleaseUnresolved, CodeChecksumMismatch, CodeDownloadFailed,
		CodeRegistrationFailed, CodeRunnerNotRegistered, CodeServiceInstall,
		CodeServiceNotRunning, CodeRunnerOffline, CodePublicRepoRefused,
		CodeVisibilityUnknown, CodeWorkflowPolicy, CodeRepoMismatch, CodeForkApproval, CodeJobHook, CodeTrustStore, CodeNotAdmitted, CodeIsolation, CodeDiskLow, CodeCLIOutdated, CodeStateCorrupt,
		CodeConfirmRequired, CodeLocalPlacement,
	}
}

// DocsBase is where each code's page lives. Placeholder host: rename with the product.
const DocsBase = "https://github.com/modullar/blade-runner/blob/main/docs/errors/README.md#"

// DocsURL returns the docs link for a code. Anchors are the lower-cased code.
func DocsURL(code string) string { return DocsBase + strings.ToLower(code) }

// Error is a user-facing failure. Err, when set, is the underlying cause kept for
// errors.Is / errors.As but printed only as detail.
type Error struct {
	Code  string
	What  string // what failed
	Cause string // the likely cause
	Fix   string // the exact command or step that fixes it
	Err   error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Code, e.What)
	if e.Cause != "" {
		fmt.Fprintf(&b, "\n  likely cause: %s", e.Cause)
	}
	if e.Fix != "" {
		fmt.Fprintf(&b, "\n  fix: %s", e.Fix)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, "\n  detail: %s", e.Err)
	}
	fmt.Fprintf(&b, "\n  docs: %s", DocsURL(e.Code))
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an Error.
func New(code, what, cause, fix string) *Error {
	return &Error{Code: code, What: what, Cause: cause, Fix: fix}
}

// Wrap builds an Error around an underlying cause.
func Wrap(err error, code, what, cause, fix string) *Error {
	return &Error{Code: code, What: what, Cause: cause, Fix: fix, Err: err}
}

// CodeOf returns the code of the first *Error in err's chain, or "".
func CodeOf(err error) string {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}
