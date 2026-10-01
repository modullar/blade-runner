package supervisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	KindAlarm     = "alarm"     // a runner was handed a job that was not admitted, or that could not be ruled out
	KindError     = "error"     // something failed; the supervisor failed closed
	KindGaveUp    = "gave_up"   // an admitted job's runner failed repeatedly; left alone
	KindRecovered = "recovered" // the log was reopened after a torn last line; see AuditLog
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

	// Fragment is set only on a KindRecovered entry: the sha256 of the torn line the entry
	// accounts for, so verification accepts that one unparseable line and no other.
	Fragment string `json:"fragment_sha256,omitempty"`
}

// ErrTornTail is wrapped by VerifyAuditLog's error when the log ends in a line that is not an
// entry: an interrupted write. Everything before it verified. Reopening the log with
// OpenAuditLog is the explicit recovery: it records a KindRecovered entry that names the torn
// line by hash, and the chain then verifies again.
var ErrTornTail = errors.New("the audit log ends in a torn line")

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
//
// What the chain can and cannot see:
//
//   - The LAST entry has no successor, so nothing in the chain commits to it: it can be edited,
//     or the tail cut off, and the rest still verifies. The head anchor (<path>.head, the number
//     and hash of the newest entry, rewritten after every entry) closes that for as long as the
//     anchor file survives: VerifyAuditLog and OpenAuditLog refuse a log that is shorter than
//     its anchor or whose anchored entry differs.
//   - An EMPTY log verifies clean (zero entries), and so does one whose anchor was removed or
//     rewritten along with it: both are just files, and nothing remembers what was there.
//     Holding the head hash somewhere the machine's user cannot write (another host, an
//     append-only service) is the only real answer and is not built.
//   - A write that fails after part of the line reached the disk, or a flush that fails after the
//     line was written, leaves the writer unable to say what the file holds. It stops writing
//     (every later Record fails with BR-E079) rather than guess; reopening the log is the
//     recovery. A torn last line is then accounted for by a KindRecovered entry that names it by
//     hash, so the chain verifies again and the damage stays on record.
//   - One writer at a time: the file is flocked while open, so two supervisors (or one with a
//     different configuration) cannot fork the chain.
type AuditLog struct {
	Path string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// SyncFn flushes the file to disk; nil means (*os.File).Sync. It exists so a test can make
	// the flush fail the way a failing disk would.
	SyncFn func() error

	mu       sync.Mutex
	f        *os.File
	seq      int64
	prev     string // hex sha256 of the last line
	fixNL    bool   // the file ended without a newline: start on a fresh line
	closed   bool
	poisoned error // set once what is on disk is unknown: every later write fails
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

// parsedLog is a log read into entries (with the exact line of each) and, possibly, a torn tail.
type parsedLog struct {
	entries []Entry
	lines   [][]byte // the trimmed line of each entry, what the next entry's prev_hash covers
	tail    []byte   // a last line that is not an entry (an interrupted write), or nil
	tailNo  int      // its line number
}

// parseLog splits data into entries. A line that is not an entry is accepted only when it is the
// last line (a torn tail, reported in tail) or when the very next entry is a KindRecovered
// record naming it by hash. Anything else is damage and an error.
func parseLog(data []byte) (parsedLog, error) {
	var p parsedLog
	raw := bytes.Split(data, []byte("\n"))
	last := -1
	for i, l := range raw {
		if len(bytes.TrimSpace(l)) > 0 {
			last = i
		}
	}
	pending, pendingNo := "", 0
	for i, l := range raw {
		line := bytes.TrimSpace(l)
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			if pending != "" {
				return p, fmt.Errorf("line %d is not an entry and no recovery record accounts for it", pendingNo)
			}
			if i == last {
				p.tail, p.tailNo = line, i+1
				break
			}
			pending, pendingNo = lineHash(line), i+1
			continue
		}
		if pending != "" {
			if e.Kind != KindRecovered || e.Fragment != pending {
				return p, fmt.Errorf("line %d is not an entry and no recovery record accounts for it", pendingNo)
			}
			pending = ""
		}
		p.entries = append(p.entries, e)
		p.lines = append(p.lines, append([]byte(nil), line...))
	}
	if pending != "" {
		return p, fmt.Errorf("line %d is not an entry and no recovery record accounts for it", pendingNo)
	}
	return p, nil
}

// checkChain verifies sequence numbers and hashes and returns the hash of the last line.
func (p parsedLog) checkChain() (prev string, n int, err error) {
	prev = genesis
	for i, e := range p.entries {
		n = i + 1
		if e.Seq != int64(n) {
			return prev, n, fmt.Errorf("entry %d has sequence %d, want %d: an entry was removed or inserted", n, e.Seq, n)
		}
		if e.PrevHash != prev {
			return prev, n, fmt.Errorf("entry %d does not follow the entry before it: an entry was edited or removed", n)
		}
		prev = lineHash(p.lines[i])
	}
	return prev, n, nil
}

// anchor is the number and hash of the newest entry, kept beside the log.
type anchor struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

func anchorPath(path string) string { return path + ".head" }

func readAnchor(path string) (*anchor, error) {
	raw, err := os.ReadFile(anchorPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a anchor
	if err := json.Unmarshal(raw, &a); err != nil || a.Seq < 1 || a.Hash == "" {
		return nil, fmt.Errorf("the head anchor %s is not valid", anchorPath(path))
	}
	return &a, nil
}

func writeAnchor(path string, seq int64, hash string) error {
	raw, _ := json.Marshal(anchor{Seq: seq, Hash: hash})
	tmp := anchorPath(path) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, anchorPath(path))
}

// checkAnchor compares the log with its head anchor, if there is one.
func (p parsedLog) checkAnchor(path string) error {
	a, err := readAnchor(path)
	if err != nil {
		return err
	}
	if a == nil {
		return nil
	}
	if int64(len(p.entries)) < a.Seq {
		return fmt.Errorf("the log has %d entries but its head anchor says there were at least %d: it was truncated or replaced", len(p.entries), a.Seq)
	}
	if lineHash(p.lines[a.Seq-1]) != a.Hash {
		return fmt.Errorf("entry %d does not match its head anchor: it was edited or replaced", a.Seq)
	}
	return nil
}

// OpenAuditLog opens (creating, with owner-only permissions) the log at path and continues its
// chain. It takes an exclusive lock on the file, so a second writer is refused. A log that does
// not verify (damage, or shorter than its head anchor) is refused too: continuing it would hide
// the break. A torn last line is the one damage it repairs, explicitly, by recording a
// KindRecovered entry that names the fragment by hash.
func OpenAuditLog(path string, now func() time.Time) (*AuditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, auditErr(err, "cannot create the audit directory")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, auditErr(err, "cannot open the audit log")
	}
	fail := func(err error, what string) (*AuditLog, error) {
		f.Close()
		return nil, auditErr(err, what)
	}
	busy, err := lockFile(f)
	if err != nil {
		return fail(err, "cannot lock the audit log")
	}
	if busy {
		f.Close()
		return nil, diag.New(diag.CodeAuditFailed, "another supervisor has the audit log open: "+path,
			"two writers would fork its hash chain", "stop the other supervisor, or give this one its own runner name (each runner name has its own log)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail(err, "cannot read the audit log")
	}
	p, err := parseLog(data)
	if err != nil {
		return fail(err, "the audit log is damaged; move it aside to start a new one")
	}
	prev, _, err := p.checkChain()
	if err != nil {
		return fail(err, "the audit log's chain is broken; move it aside to start a new one")
	}
	if err := p.checkAnchor(path); err != nil {
		return fail(err, "the audit log does not match its head anchor; move both aside to start a new one")
	}
	l := &AuditLog{Path: path, Now: now, f: f, seq: int64(len(p.entries)), prev: prev}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		l.fixNL = true
	}
	if p.tail != nil {
		msg := fmt.Sprintf("recovered: a torn last line of %d bytes (line %d, sha256 %s) was left by an interrupted write; it is kept in the file and the chain continues from the last complete entry",
			len(p.tail), p.tailNo, lineHash(p.tail))
		if err := l.append(Entry{Kind: KindRecovered, Message: msg, Fragment: lineHash(p.tail)}); err != nil {
			f.Close()
			return nil, err
		}
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
	return l.append(e)
}

// append writes e. The caller holds mu (or, at open, is the only user).
func (l *AuditLog) append(e Entry) error {
	if l.poisoned != nil {
		return auditErr(l.poisoned, "the audit log refuses further writes: an earlier write or flush failed, so what is on disk is unknown (restart the supervisor)")
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
			l.poisoned = err // part of a line is on disk: stop, and let a reopen account for it
		}
		return auditErr(err, "cannot write to the audit log")
	}
	// The line is on disk from here on, so it is part of the chain whatever happens next: a
	// failed flush must not let the next entry claim to follow the one before it.
	l.fixNL = false
	l.seq = e.Seq
	l.prev = lineHash(raw)
	sync := l.f.Sync
	if l.SyncFn != nil {
		sync = l.SyncFn
	}
	if err := sync(); err != nil {
		l.poisoned = err
		return auditErr(err, "cannot flush the audit log to disk")
	}
	if err := writeAnchor(l.Path, l.seq, l.prev); err != nil {
		return auditErr(err, "cannot write the audit log's head anchor")
	}
	return nil
}

// Close releases the file (and its lock).
func (l *AuditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return l.f.Close()
}

// ReadAuditLog returns the entries of the log at path in order. A line that is not an entry
// and is not accounted for (a torn last line, or damage) is an error naming it; the entries
// before it are returned with it.
func ReadAuditLog(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := parseLog(data)
	if err != nil {
		return p.entries, err
	}
	if p.tail != nil {
		return p.entries, fmt.Errorf("audit log line %d is not an entry: %w", p.tailNo, ErrTornTail)
	}
	return p.entries, nil
}

// VerifyAuditLog checks the chain: sequence numbers count up from 1 and every entry names the
// hash of the line before it; then the head anchor, when there is one, if it matches. It
// returns the number of entries verified, or the first break. A torn last line is reported as
// an error wrapping ErrTornTail, after the entries before it verified; a line the supervisor
// recovered from is accepted when its KindRecovered record names it. See AuditLog for what this
// cannot detect.
func VerifyAuditLog(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	p, err := parseLog(data)
	if err != nil {
		return len(p.entries), err
	}
	_, n, err := p.checkChain()
	if err != nil {
		return n, err
	}
	if err := p.checkAnchor(path); err != nil {
		return n, err
	}
	if p.tail != nil {
		return n, fmt.Errorf("line %d (%d bytes): %w; reopen the log to record the recovery", p.tailNo, len(p.tail), ErrTornTail)
	}
	return n, nil
}
