package cer

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleRecord() *Record {
	return &Record{
		DocketVersion: Version, Spec: SpecID, Trust: TrustLocalClaimed,
		Base: "abc123", Generator: Generator{Name: "docket", Version: "test"},
		Hunks: []Hunk{{
			File: "src/auth.ts", Range: [2]int{42, 67}, AddedLines: 26, Attributed: 26,
			Origin:       Origin{Actor: ActorAgent, AgentID: "claude-code/main", Task: "Fix session fixation"},
			HumanContact: ContactNone, Density: 0.4, Confidence: "high",
		}},
		Totals: Totals{Files: 1, Hunks: 1, AddedLines: 26},
	}
}

// The digest has to be stable across runs and independent of map ordering, or
// the trailer written before a commit will not match the record stored after it.
func TestCanonicalFormIsStable(t *testing.T) {
	a, err := sampleRecord().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		b, err := sampleRecord().Canonical()
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("canonical form changed between runs")
		}
	}
	// Keys must be sorted, which is what lets another implementation agree.
	if !strings.HasPrefix(string(a), `{"base":`) {
		t.Errorf("canonical form does not start with the first sorted key: %.40s", a)
	}
}

// The envelope carve-out is what makes the prepare-commit-msg trailer possible.
func TestDigestIgnoresEnvelopeFields(t *testing.T) {
	before := sampleRecord()
	digestBefore, err := before.Digest()
	if err != nil {
		t.Fatal(err)
	}
	after := sampleRecord()
	after.Commit = "8f3a2b1c"
	after.GeneratedAt = "2026-09-13T10:00:00Z"
	after.Signature = &Signature{Alg: "ed25519", Sig: "deadbeef"}
	after.PayloadHash = "sha256:whatever"
	digestAfter, err := after.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digestBefore != digestAfter {
		t.Errorf("digest changed when only envelope fields changed:\n  %s\n  %s", digestBefore, digestAfter)
	}
}

func TestDigestChangesWithEvidence(t *testing.T) {
	a, _ := sampleRecord().Digest()
	changed := sampleRecord()
	changed.Hunks[0].Density = 0.9
	b, _ := changed.Digest()
	if a == b {
		t.Error("digest did not change when the evidence changed")
	}
}

func TestSignAndVerify(t *testing.T) {
	dir := t.TempDir()
	signer, err := LocalSigner(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The key must persist, or every commit would be signed by a different
	// identity.
	again, err := LocalSigner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if signer.KeyID() != again.KeyID() {
		t.Errorf("key id changed on reload: %s vs %s", signer.KeyID(), again.KeyID())
	}
	if signer.Trust() != TrustLocalClaimed {
		t.Errorf("a local key must not claim CI attestation, got %q", signer.Trust())
	}

	rec := sampleRecord()
	if err := signer.Sign(rec); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DigestMatches || !res.SignatureValid {
		t.Errorf("freshly signed record failed to verify: %+v", res)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	signer, err := LocalSigner(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleRecord()
	if err := signer.Sign(rec); err != nil {
		t.Fatal(err)
	}
	// Someone edits the stored record to claim more evidence than there was.
	rec.Hunks[0].Density = 1.0
	res, err := Verify(rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.DigestMatches {
		t.Error("an altered record still matched its digest")
	}
}

func TestUnsignedRecordIsReportedNotAccepted(t *testing.T) {
	rec := sampleRecord()
	digest, _ := rec.Digest()
	rec.PayloadHash = digest
	res, err := Verify(rec)
	if err != ErrNoSignature {
		t.Errorf("err = %v, want ErrNoSignature", err)
	}
	if !res.DigestMatches {
		t.Error("digest should still check out")
	}
}

func TestRecordRoundTripsThroughJSON(t *testing.T) {
	rec := sampleRecord()
	data, err := rec.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Hunks) != 1 || back.Hunks[0].Range != [2]int{42, 67} {
		t.Errorf("round trip lost data: %+v", back)
	}
}

func TestStoragePaths(t *testing.T) {
	if got := Path("sha256:abcdef1234"); got != "records/ab/cdef1234.json" {
		t.Errorf("Path = %q", got)
	}
	if got := CommitPath("8f3a2b"); got != "by-commit/8f/3a2b" {
		t.Errorf("CommitPath = %q", got)
	}
}
