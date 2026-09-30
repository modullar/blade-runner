package trust

import (
	"fmt"

	"github.com/modullar/blade-runner/internal/diag"
)

// Commit is what the provider returns for a commit id: the signed bytes and the signature.
type Commit struct {
	SHA       string // the id that was asked for
	Payload   string // the commit object without its signature: what was signed
	Signature string // the armored SSH signature, or "" if unsigned
}

// Verdict says who vouched for a commit.
type Verdict struct {
	Signer Signer
}

// Verifier admits commits signed by keys in Store.
type Verifier struct {
	Store *Store
}

func refuse(sha, why, fix string) error {
	return diag.New(diag.CodeNotAdmitted, fmt.Sprintf("commit %s is not admitted: %s", short(sha), why),
		"only commits signed by a key in this machine's trust store may run here", fix)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Verify checks, in order, and refuses at the first failure:
//  1. the commit is signed (an unsigned commit proves nothing about who wrote it);
//  2. the signature belongs to THIS commit: the id recomputed from the payload and signature
//     equals the id asked for, so the bytes cannot be swapped for others;
//  3. the signature is cryptographically valid, in git's namespace, for the key it names;
//  4. that key is in the trust store and neither revoked nor expired.
func (v *Verifier) Verify(c Commit) (Verdict, error) {
	if c.Signature == "" {
		return Verdict{}, refuse(c.SHA, "it is not signed", "sign your commits (git config gpg.format ssh; git commit -S) with a key the owner has trusted")
	}
	id, err := ObjectID([]byte(c.Payload), c.Signature)
	if err != nil {
		return Verdict{}, refuse(c.SHA, "its contents cannot be read ("+err.Error()+")", "report this: the commit is malformed")
	}
	if id != c.SHA {
		return Verdict{}, refuse(c.SHA, "the signed contents do not hash to this commit id (got "+short(id)+")",
			"GitHub returned bytes that are not this commit; nothing was admitted")
	}
	key, err := VerifySSHSig(c.Signature, []byte(c.Payload), "git")
	if err != nil {
		return Verdict{}, refuse(c.SHA, err.Error(), "re-sign the commit with a supported key (ssh-ed25519)")
	}
	signer, found, err := v.Store.Find(key)
	if err != nil {
		return Verdict{}, err
	}
	if !found {
		return Verdict{}, refuse(c.SHA, "it is signed by a key this machine does not trust ("+key.Fingerprint()+")",
			"if you trust its owner: bladerunner trust add --name <who> --key <their .pub file>")
	}
	if !signer.Active(v.Store.Clock()) {
		return Verdict{}, refuse(c.SHA, fmt.Sprintf("its signer %q is %s", signer.Name, signer.Status(v.Store.Clock())),
			"ask for a new key, and add it with `bladerunner trust add`")
	}
	return Verdict{Signer: signer}, nil
}
