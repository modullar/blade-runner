package trust

import (
	"crypto/sha1" //nolint:gosec // git object ids are SHA-1; this recomputes one, it is not a security hash here
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// A signed commit on disk is: headers, a `gpgsig` header holding the signature (continuation
// lines start with a space), a blank line, the message. What is signed is the same object
// WITHOUT the gpgsig header. GitHub's API hands back both halves separately.

// SplitSignedCommit separates a raw commit object (as `git cat-file commit` prints it) into the
// signed payload and the signature. signature is "" for an unsigned commit.
func SplitSignedCommit(raw []byte) (payload []byte, signature string, err error) {
	headEnd := strings.Index(string(raw), "\n\n")
	if headEnd < 0 {
		return nil, "", errors.New("malformed commit object: no blank line after the headers")
	}
	head, body := string(raw[:headEnd]), string(raw[headEnd:])
	lines := strings.Split(head, "\n")
	var kept, sig []string
	inSig := false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "gpgsig "):
			inSig = true
			sig = append(sig, strings.TrimPrefix(l, "gpgsig "))
		case inSig && strings.HasPrefix(l, " "):
			sig = append(sig, l[1:])
		default:
			inSig = false
			kept = append(kept, l)
		}
	}
	payload = []byte(strings.Join(kept, "\n") + body)
	return payload, strings.Join(sig, "\n"), nil
}

// ObjectID recomputes the git id of a signed commit from its payload and signature. Matching
// the id the caller asked for is what ties the signature to the commit, and so to the tree,
// that will actually run: GitHub cannot hand back different bytes for the same id.
func ObjectID(payload []byte, signature string) (string, error) {
	raw, err := joinSignedCommit(payload, signature)
	if err != nil {
		return "", err
	}
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "commit %d\x00", len(raw))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func joinSignedCommit(payload []byte, signature string) ([]byte, error) {
	if signature == "" {
		return payload, nil
	}
	headEnd := strings.Index(string(payload), "\n\n")
	if headEnd < 0 {
		return nil, errors.New("malformed commit payload: no blank line after the headers")
	}
	sigLines := strings.Split(strings.TrimRight(signature, "\n"), "\n")
	var b strings.Builder
	b.WriteString(string(payload[:headEnd]))
	b.WriteString("\ngpgsig " + sigLines[0])
	for _, l := range sigLines[1:] {
		b.WriteString("\n " + l)
	}
	b.WriteString(string(payload[headEnd:]))
	return []byte(b.String()), nil
}
