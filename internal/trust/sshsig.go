// Package trust is the cryptographic admission layer: it decides whether a commit was signed
// by a key the machine's owner has chosen to trust, using only keys held on this machine.
//
// Why signatures and not logins: a GitHub login is GitHub's statement about who pushed. A
// signature is a fact this machine checks itself, with public keys it holds, about the exact
// contents of a commit (a commit's signature covers its tree, so it covers every file). GitHub
// cannot forge one without the private key, and the commit id is recomputed from the signed
// bytes, so GitHub cannot substitute other code for signed code either.
//
// What a signature does and does not prove: it proves a key holder vouched for exactly this
// tree. It does not prove the code is safe, which is why jobs also run in isolation.
//
// Scope: SSH signatures (the format `git commit -S` produces with gpg.format=ssh) with
// ed25519 keys, implemented with the standard library. Other key types and GPG are refused,
// not guessed at.
package trust

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	keyTypeEd25519 = "ssh-ed25519"
	sigMagic       = "SSHSIG"
	sigArmorBegin  = "-----BEGIN SSH SIGNATURE-----"
	sigArmorEnd    = "-----END SSH SIGNATURE-----"
)

// PublicKey is an ed25519 SSH public key.
type PublicKey struct {
	Blob    []byte // the SSH wire encoding
	Comment string
}

// Fingerprint is the OpenSSH-style SHA256 fingerprint, as `ssh-keygen -l` prints it.
func (k PublicKey) Fingerprint() string {
	sum := sha256.Sum256(k.Blob)
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}

// AuthorizedKey renders the key as an authorized_keys / .pub line.
func (k PublicKey) AuthorizedKey() string {
	s := keyTypeEd25519 + " " + base64.StdEncoding.EncodeToString(k.Blob)
	if k.Comment != "" {
		s += " " + k.Comment
	}
	return s
}

func (k PublicKey) raw() ed25519.PublicKey {
	_, rest, _ := readString(k.Blob)
	key, _, _ := readString(rest)
	return ed25519.PublicKey(key)
}

// ParsePublicKey reads a `.pub` / authorized_keys line: "ssh-ed25519 AAAA... comment".
func ParsePublicKey(line string) (PublicKey, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return PublicKey{}, errors.New("not an SSH public key: expected `ssh-ed25519 <base64> [comment]`")
	}
	if fields[0] != keyTypeEd25519 {
		return PublicKey{}, fmt.Errorf("unsupported key type %q: only ssh-ed25519 keys are supported", fields[0])
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return PublicKey{}, fmt.Errorf("the key is not valid base64: %w", err)
	}
	return parseKeyBlob(blob, strings.Join(fields[2:], " "))
}

func parseKeyBlob(blob []byte, comment string) (PublicKey, error) {
	typ, rest, err := readString(blob)
	if err != nil {
		return PublicKey{}, errors.New("malformed key")
	}
	if string(typ) != keyTypeEd25519 {
		return PublicKey{}, fmt.Errorf("unsupported key type %q: only ssh-ed25519 keys are supported", typ)
	}
	key, rest, err := readString(rest)
	if err != nil || len(key) != ed25519.PublicKeySize || len(rest) != 0 {
		return PublicKey{}, errors.New("malformed ed25519 key")
	}
	return PublicKey{Blob: append([]byte(nil), blob...), Comment: comment}, nil
}

// readString reads an SSH wire string (uint32 length, bytes).
func readString(b []byte) (s, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, errors.New("short buffer")
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, errors.New("string longer than the buffer")
	}
	return b[4 : 4+n], b[4+n:], nil
}

func appendString(dst, s []byte) []byte {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(s)))
	return append(append(dst, l[:]...), s...)
}

// VerifySSHSig checks an armored SSH signature (PROTOCOL.sshsig) over message in the given
// namespace, and returns the key it was made with. It proves the signature is valid for that
// key; whether the key is trusted is for the caller (Store) to decide.
func VerifySSHSig(armored string, message []byte, namespace string) (PublicKey, error) {
	blob, err := dearmor(armored)
	if err != nil {
		return PublicKey{}, err
	}
	if len(blob) < len(sigMagic)+4 || string(blob[:len(sigMagic)]) != sigMagic {
		return PublicKey{}, errors.New("not an SSH signature")
	}
	rest := blob[len(sigMagic):]
	if v := binary.BigEndian.Uint32(rest); v != 1 {
		return PublicKey{}, fmt.Errorf("unsupported SSH signature version %d", v)
	}
	rest = rest[4:]

	var keyBlob, ns, reserved, hashAlg, sigBlob []byte
	for _, dst := range []*[]byte{&keyBlob, &ns, &reserved, &hashAlg, &sigBlob} {
		if *dst, rest, err = readString(rest); err != nil {
			return PublicKey{}, errors.New("truncated SSH signature")
		}
	}
	if len(rest) != 0 {
		return PublicKey{}, errors.New("trailing data after the SSH signature")
	}
	if string(ns) != namespace {
		return PublicKey{}, fmt.Errorf("the signature is for namespace %q, not %q: it was not made for a git commit", ns, namespace)
	}
	key, err := parseKeyBlob(keyBlob, "")
	if err != nil {
		return PublicKey{}, err
	}

	var digest []byte
	switch string(hashAlg) {
	case "sha512":
		h := sha512.Sum512(message)
		digest = h[:]
	case "sha256":
		h := sha256.Sum256(message)
		digest = h[:]
	default:
		return PublicKey{}, fmt.Errorf("unsupported signature hash %q", hashAlg)
	}
	signed := []byte(sigMagic)
	signed = appendString(signed, ns)
	signed = appendString(signed, reserved)
	signed = appendString(signed, hashAlg)
	signed = appendString(signed, digest)

	alg, sigRest, err := readString(sigBlob)
	if err != nil || string(alg) != keyTypeEd25519 {
		return PublicKey{}, errors.New("the signature is not an ed25519 signature")
	}
	sig, sigRest, err := readString(sigRest)
	if err != nil || len(sig) != ed25519.SignatureSize || len(sigRest) != 0 {
		return PublicKey{}, errors.New("malformed ed25519 signature")
	}
	if !ed25519.Verify(key.raw(), signed, sig) {
		return PublicKey{}, errors.New("the signature does not match the commit: it was altered, or made by a different key")
	}
	return key, nil
}

func dearmor(armored string) ([]byte, error) {
	s := strings.TrimSpace(armored)
	if !strings.HasPrefix(s, sigArmorBegin) || !strings.HasSuffix(s, sigArmorEnd) {
		return nil, errors.New("not an armored SSH signature (is this a GPG signature? only SSH signatures are supported)")
	}
	body := strings.TrimSuffix(strings.TrimPrefix(s, sigArmorBegin), sigArmorEnd)
	body = strings.Join(strings.Fields(body), "")
	blob, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("the signature is not valid base64: %w", err)
	}
	return blob, nil
}
