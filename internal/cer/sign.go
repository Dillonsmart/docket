package cer

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Signer holds a key that can sign records.
type Signer struct {
	priv ed25519.PrivateKey
	// Where names the key's home, which decides the trust tier: a key the
	// developer holds can only make a local claim, however honest they are.
	Where string
}

// SignerFromEnv returns the CI signing key when one is configured. The plan is
// explicit that CI attestation means a key the runner holds and the developer
// does not, so this is the only path to the higher tier.
func SignerFromEnv() (*Signer, bool, error) {
	raw := strings.TrimSpace(os.Getenv("DOCKET_SIGNING_KEY"))
	if raw == "" {
		return nil, false, nil
	}
	seed, err := hex.DecodeString(raw)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, false, fmt.Errorf("DOCKET_SIGNING_KEY must be %d hex-encoded bytes", ed25519.SeedSize)
	}
	return &Signer{priv: ed25519.NewKeyFromSeed(seed), Where: "ci"}, true, nil
}

// LocalSigner loads the repository's local key, creating one on first use.
//
// The key lives in the git directory and is never pushed: it identifies this
// machine's claims, nothing more.
func LocalSigner(stateDir string) (*Signer, error) {
	path := filepath.Join(stateDir, "signing.key")
	if data, err := os.ReadFile(path); err == nil {
		seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s is not a valid signing key", path)
		}
		return &Signer{priv: ed25519.NewKeyFromSeed(seed), Where: "local"}, nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return &Signer{priv: ed25519.NewKeyFromSeed(seed), Where: "local"}, nil
}

// KeyID is a short, stable name for the verifying key.
func (s *Signer) KeyID() string { return KeyIDFor(s.priv.Public().(ed25519.PublicKey)) }

// KeyIDFor derives a key id from a public key.
func KeyIDFor(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "ed25519:" + hex.EncodeToString(sum[:8])
}

// Trust reports the tier a record signed by this key may claim.
func (s *Signer) Trust() string {
	if s.Where == "ci" {
		return TrustCIAttested
	}
	return TrustLocalClaimed
}

// Sign fills in the record's digest and signature.
func (s *Signer) Sign(r *Record) error {
	digest, err := r.Digest()
	if err != nil {
		return err
	}
	r.PayloadHash = digest
	pub := s.priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(s.priv, []byte(digest))
	r.Signature = &Signature{
		Alg: "ed25519", KeyID: KeyIDFor(pub), Sig: hex.EncodeToString(sig),
		PublicKey: hex.EncodeToString(pub), Signer: s.Where,
	}
	return nil
}

// VerifyResult reports what could be established about a record.
type VerifyResult struct {
	DigestMatches    bool
	SignatureValid   bool
	SignaturePresent bool
	KeyID            string
	Trust            string
	Recomputed       string
	Stated           string
}

// ErrNoSignature is returned when a record carries no signature at all.
var ErrNoSignature = errors.New("record is not signed")

// Verify recomputes the digest and checks the signature. It does not decide
// whether the key is trustworthy: that is the reader's business, and the whole
// point of publishing the tier next to it.
func Verify(r *Record) (VerifyResult, error) {
	res := VerifyResult{Stated: r.PayloadHash, Trust: r.Trust}
	digest, err := r.Digest()
	if err != nil {
		return res, err
	}
	res.Recomputed = digest
	res.DigestMatches = digest == r.PayloadHash
	if r.Signature == nil {
		return res, ErrNoSignature
	}
	res.SignaturePresent = true
	res.KeyID = r.Signature.KeyID
	pub, err := hex.DecodeString(r.Signature.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return res, fmt.Errorf("signature has no usable public key")
	}
	sig, err := hex.DecodeString(r.Signature.Sig)
	if err != nil {
		return res, fmt.Errorf("signature is not hex")
	}
	if KeyIDFor(pub) != r.Signature.KeyID {
		return res, fmt.Errorf("key id does not match the published public key")
	}
	res.SignatureValid = ed25519.Verify(pub, []byte(r.PayloadHash), sig)
	return res, nil
}
