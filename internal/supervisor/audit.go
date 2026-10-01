package supervisor

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
)

// Entry kinds. Every decision the supervisor takes is one of these, written before it takes
// effect.
const (
	KindStartup   = "startup"   // the supervisor started and reconciled its state with the provider
	KindRefused   = "refused"   // a queued job was refused; nothing was started for it
	KindWithheld  = "withheld"  // no runner was started (or one was stopped) because of a job that is not admitted
	KindCancel    = "cancel"    // a refused run was asked to cancel
	KindLaunching = "launching" // an admitted job's runner is about to start
	KindFinished  = "finished"  // a runner's container ended
	KindAlarm     = "alarm"     // a runner was handed a job that was not admitted
	KindError     = "error"     // something failed; the supervisor failed closed
	KindGaveUp    = "gave_up"   // an admitted job's runner failed repeatedly; left alone
)

// Entry is one line of the audit log. It never holds a secret: in particular never the
// just-in-time runner config, which starts a runner.
type Entry struct {
	Seq      int64  `json:"seq"`
	Time     string `json:"time"`
	PrevHash string `json:"prev_hash"` // sha256 of the previous line: a deleted or edited line breaks the chain

	Kind    string `json:"kind"`
	Code    string `json:"code,omitempty"` // a diag code, for refusals and failures
	Message string `json:"message"`

	Repository  string `json:"repository,omitempty"` // where the commit lives
	SHA         string `json:"sha,omitempty"`
	RunID       int64  `json:"run_id,omitempty"`
	JobID       int64  `json:"job_id,omitempty"`
	Event       string `json:"event,omitempty"`
	Actor       string `json:"actor,omitempty"`
	Signer      string `json:"signer,omitempty"` // who vouched, when admitted
	Fingerprint string `json:"fingerprint,omitempty"`

	Runner    string  `json:"runner,omitempty"` // the just-in-time runner's name (also the container's)
	ExitCode  *int    `json:"exit_code,omitempty"`
	TimedOut  bool    `json:"timed_out,omitempty"`
	OOMKilled bool    `json:"oom_killed,omitempty"`
	JobsRun   []int64 `json:"jobs_run,omitempty"` // jobs the provider says this runner took
}

// Recorder is where decisions are written. The supervisor starts nothing it could not record.
type Recorder interface {
	Record(Entry) error
}

// AuditLog is an append-only JSON-lines file with a hash chain. It is the durable answer to
// "why was this job not run" and "what did this machine run, and who vouched for it".
//
// Append-only is enforced by how the file is opened (O_APPEND) and made detectable by the
// chain (VerifyAuditLog); a local attacker who can edit the file can also rewrite the chain, so
// this is evidence against accident and casual tampering, not a defence against root.
type AuditLog struct {
	Path string
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu     sync.Mutex
	f      *os.File
	seq    int64
	prev   string // hex sha256 of the last line
	fixNL  bool   // the file ended without a newline (a crash mid-write): start on a fresh line
	closed bool
}

const genesis = "0000000000000000000000000000000000000000000000000000000000000000"

func auditErr(err error, what string) error {
	return diag.Wrap(err, diag.CodeAuditFailed, what, "the audit directory is not writable or the disk is full",
		"fix the permissions of ~/.bladerunner/audit, or free disk space")
}

func lineHash(line []byte) string {
	h := sha256.Sum256(bytes.TrimSpace(line))
	return hex.EncodeToString(h[:])
}

// OpenAuditLog opens (creating, with owner-only permissions) the log at path and continues its
// chain.
func OpenAuditLog(path string, now func() time.Time) (*AuditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, auditErr(err, "cannot create the audit directory")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, auditErr(err, "cannot open the audit log")
	}
	l := &AuditLog{Path: path, Now: now, f: f, prev: genesis}
	data, err := os.ReadFile(path)
	if err != nil {
		f.Close()
		return nil, auditErr(err, "cannot read the audit log")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		l.fixNL = true
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		l.seq++
		l.prev = lineHash(line)
	}
	if err := sc.Err(); err != nil {
		f.Close()
		return nil, auditErr(err, "cannot read the audit log")
	}
	return l, nil
}

// Record appends one entry and flushes it to disk before returning. It fails (BR-E079) rather
// than lose the entry.
func (l *AuditLog) Record(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return auditErr(os.ErrClosed, "the audit log is closed")
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	e.Seq = l.seq + 1
	e.Time = now().UTC().Format(time.RFC3339Nano)
	e.PrevHash = l.prev
	raw, err := json.Marshal(e)
	if err != nil {
		return auditErr(err, "cannot encode an audit entry")
	}
	line := append(raw, '\n')
	if l.fixNL {
		line = append([]byte{'\n'}, line...)
	}
	if n, err := l.f.Write(line); err != nil {
		if n > 0 {
			l.fixNL = true // part of a line may be on disk
		}
		return auditErr(err, "cannot write to the audit log")
	}
	if err := l.f.Sync(); err != nil {
		return auditErr(err, "cannot flush the audit log to disk")
	}
	l.fixNL = false
	l.seq = e.Seq
	l.prev = lineHash(raw)
	return nil
}

// Close releases the file.
func (l *AuditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return l.f.Close()
}

// ReadAuditLog returns the entries of the log at path in order. A line that is not an entry
// (a half-written one) is an error naming it.
func ReadAuditLog(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return out, fmt.Errorf("audit log line %d is not an entry: %v", i+1, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// VerifyAuditLog checks the chain: sequence numbers count up from 1 and every entry names the
// hash of the line before it. It returns the number of entries, or the first break.
func VerifyAuditLog(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	prev := genesis
	n := 0
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return n, fmt.Errorf("line %d is not an entry: %v", i+1, err)
		}
		n++
		if e.Seq != int64(n) {
			return n, fmt.Errorf("line %d has sequence %d, want %d: an entry was removed or inserted", i+1, e.Seq, n)
		}
		if e.PrevHash != prev {
			return n, fmt.Errorf("line %d does not follow the line before it: an entry was edited or removed", i+1)
		}
		prev = lineHash(line)
	}
	return n, nil
}
