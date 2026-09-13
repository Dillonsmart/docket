// Package cer is the Commit Evidence Record: the on-disk, versioned schema a
// docket is written in.
//
// It is deliberately separate from everything that produces it. The record is
// the durable artifact — content-addressed, stored in the repository, readable
// with nothing but git and a JSON parser years after the tool that wrote it is
// gone. The specification lives in spec/CER.md and this package is its
// reference implementation.
package cer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Version is the schema version of records this build writes.
const Version = "0.1"

// SpecID names the specification, so a reader of a bare JSON blob can find it.
const SpecID = "CER/0.1"

// Trust tiers. The distinction is in the schema from day one because
// retrofitting it later would mean re-reading every historic record.
const (
	// TrustLocalClaimed means the record was built on a developer machine and
	// signed with a key that machine holds. It says what the machine claims.
	TrustLocalClaimed = "local_claimed"
	// TrustCIAttested means the record was built by CI and signed with a key the
	// runner holds and the developer does not.
	TrustCIAttested = "ci_attested"
)

// Actor values for an origin.
const (
	ActorAgent   = "agent"
	ActorHuman   = "human"
	ActorUnknown = "unknown"
)

// Human contact values.
const (
	ContactNone     = "none"
	ContactEdited   = "edited"
	ContactApproved = "approved"
	ContactViewed   = "viewed"
)

// Record is one commit's evidence record.
type Record struct {
	DocketVersion string `json:"docket_version"`
	Spec          string `json:"spec"`
	Trust         string `json:"trust"`

	// Base is the revision the diff was taken against.
	Base      string    `json:"base"`
	Generator Generator `json:"generator"`
	Redaction Redaction `json:"redaction"`
	Sessions  []Session `json:"sessions"`
	Hunks     []Hunk    `json:"hunks"`
	Totals    Totals    `json:"totals"`

	// Envelope fields. These are excluded from the payload digest, because the
	// digest has to be computable before the commit it names exists: the trailer
	// carrying it is written by prepare-commit-msg, one step before git creates
	// the commit object. See Digest.
	Commit      string     `json:"commit"`
	GeneratedAt string     `json:"generated_at,omitempty"`
	PayloadHash string     `json:"payload_digest"`
	Signature   *Signature `json:"signature,omitempty"`
}

// Generator identifies what wrote the record.
type Generator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Redaction states what was stripped, so a reader knows the guarantees.
type Redaction struct {
	Version string   `json:"version"`
	Mode    string   `json:"mode"` // aggressive
	Rules   []string `json:"rules_fired,omitempty"`
}

// Session describes one agent session that contributed to the commit.
type Session struct {
	ID       string `json:"id"`
	Agent    string `json:"agent,omitempty"`
	Model    string `json:"model,omitempty"`
	Harness  string `json:"harness_version,omitempty"`
	Started  string `json:"started,omitempty"`
	Ended    string `json:"ended,omitempty"`
	Edits    int    `json:"edits"`
	Observed int    `json:"observed_edits"`
	Commands int    `json:"commands"`
	// Lossy counts edits the session recorded but docket could not replay. It is
	// published rather than hidden: it is the honest measure of how much of the
	// journey survived.
	Lossy int `json:"lossy_edits"`
	// Unreadable is set when docket found a source it could not read, so a
	// reader can tell "this agent wrote nothing" from "docket could not look".
	Unreadable string `json:"unreadable,omitempty"`
}

// Hunk is the evidence record for one diff hunk.
type Hunk struct {
	File  string `json:"file"`
	Range [2]int `json:"range"`
	// Commit is set only in an aggregate record covering several commits, where
	// a hunk has to say which one it came from. It is absent in a single-commit
	// record, where the envelope already says.
	Commit string `json:"commit,omitempty"`

	Origin       Origin        `json:"origin"`
	Contributors []Contributor `json:"contributors,omitempty"`
	Attempts     []Attempt     `json:"attempts,omitempty"`
	Evidence     []Evidence    `json:"evidence,omitempty"`
	HumanContact string        `json:"human_contact"`

	AddedLines int     `json:"added_lines"`
	Attributed int     `json:"attributed_lines"`
	Verified   int     `json:"verified_lines"`
	Confidence string  `json:"attribution_confidence"`
	Density    float64 `json:"evidence_density"`

	// Unknown explains, when origin.actor is unknown, why nothing could be
	// established. An unexplained unknown would be indistinguishable from a bug.
	Unknown *Unknown `json:"unknown,omitempty"`
}

// Origin is who wrote a hunk.
type Origin struct {
	Actor   string `json:"actor"`
	AgentID string `json:"agent_id,omitempty"`
	Model   string `json:"model,omitempty"`
	Session string `json:"session,omitempty"`
	Task    string `json:"task,omitempty"`
	Tool    string `json:"tool,omitempty"`
	// Source is "observed" when docket read the file before and after the change
	// itself, "transcript" when the harness reported it.
	Source string `json:"source,omitempty"`
	At     string `json:"at,omitempty"`
	// Intent is what the agent said it was doing immediately before the edit.
	Intent string `json:"intent,omitempty"`
	// Command is the shell command responsible, for observed edits.
	Command string `json:"command,omitempty"`
}

// Contributor is a secondary edit that also wrote lines in the hunk.
type Contributor struct {
	AgentID string `json:"agent_id,omitempty"`
	Session string `json:"session,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Lines   int    `json:"lines"`
	At      string `json:"at,omitempty"`
	Task    string `json:"task,omitempty"`
}

// Attempt is something tried in this region and then taken out again.
type Attempt struct {
	Summary string `json:"summary"`
	Outcome string `json:"outcome"` // abandoned | superseded
	Reason  string `json:"reason,omitempty"`
	Lines   int    `json:"lines"`
	At      string `json:"at,omitempty"`
	Sample  string `json:"sample,omitempty"`
}

// Evidence is a verification signal attached to a hunk.
type Evidence struct {
	Kind string `json:"kind"` // test_execution | coverage | typecheck | static_check
	Ref  string `json:"ref"`
	// Result is pass, fail or unknown.
	Result string `json:"result"`
	// Transitioned means a check that failed before this change passes after it,
	// which is much stronger than a check that was always green.
	Transitioned bool `json:"transitioned,omitempty"`
	// Observed says whether the check ran after the code was written. A test that
	// ran before the edit proves nothing about it.
	Observed bool `json:"observed_after_edit"`
	// CoveredLines and TotalLines apply to coverage evidence.
	CoveredLines int `json:"covered_lines,omitempty"`
	TotalLines   int `json:"total_lines,omitempty"`
	// Confidence records how the result was determined.
	Confidence string `json:"confidence,omitempty"`
	At         string `json:"at,omitempty"`
}

// Unknown explains an unattributed hunk.
type Unknown struct {
	Reasons map[string]int `json:"reasons"`
	// Candidates are commands that ran in the window and could account for the
	// change. They are candidates, never attributions: docket did not see them
	// write these lines, and saying otherwise would be a guess.
	Candidates []Candidate `json:"candidates,omitempty"`
}

// Candidate is a possible but unproven explanation.
type Candidate struct {
	Command string `json:"command"`
	Hint    string `json:"hint,omitempty"`
	At      string `json:"at,omitempty"`
	// PathMentioned is true when the command line names this file, which is
	// circumstantial evidence and labelled as such.
	PathMentioned bool `json:"path_mentioned,omitempty"`
}

// Totals is the commit-level roll-up: the numbers a review surface sorts on and
// an organisation aggregates.
type Totals struct {
	Files           int     `json:"files"`
	Hunks           int     `json:"hunks"`
	AddedLines      int     `json:"added_lines"`
	AttributedLines int     `json:"attributed_lines"`
	VerifiedLines   int     `json:"verified_lines"`
	UnknownHunks    int     `json:"unknown_hunks"`
	ZeroEvidence    int     `json:"zero_evidence_hunks"`
	ZeroEvidenceAdd int     `json:"zero_evidence_lines"`
	MeanDensity     float64 `json:"mean_evidence_density"`
}

// Signature is a detached signature over the payload digest.
type Signature struct {
	Alg   string `json:"alg"` // ed25519
	KeyID string `json:"key_id"`
	Sig   string `json:"sig"` // hex
	// PublicKey is the verifying key, hex encoded, so a record can be checked
	// with nothing but itself. It proves the record has not been altered since it
	// was written; whether the key is one you trust is what the tier is for.
	PublicKey string `json:"public_key,omitempty"`
	// Signer names where the key lived: "local" or "ci".
	Signer string `json:"signer,omitempty"`
}

// envelope names the fields excluded from the payload digest.
var envelope = map[string]bool{
	"commit":         true,
	"generated_at":   true,
	"payload_digest": true,
	"signature":      true,
}

// Digest computes the payload digest: sha256 over the canonical JSON of the
// record with the envelope fields removed.
//
// Excluding the commit id is what makes the git trailer possible. The trailer
// has to be in the commit message, and the message is fixed before the commit
// object — and therefore its id — exists. So the digest covers the evidence and
// not the name of the thing it is attached to; `docket verify` checks the
// binding the other way round, from the commit's trailer to the record.
func (r *Record) Digest() (string, error) {
	payload, err := r.canonicalPayload()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (r *Record) canonicalPayload() ([]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k := range envelope {
		delete(m, k)
	}
	return canonicalMap(m)
}

// Canonical renders the whole record in canonical form, which is what gets
// stored: stable key order means the same evidence always produces the same
// bytes, and a stored record can be re-hashed by anyone.
func (r *Record) Canonical() ([]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func canonicalMap(m map[string]json.RawMessage) ([]byte, error) {
	conv := make(map[string]any, len(m))
	for k, v := range m {
		var d any
		if err := json.Unmarshal(v, &d); err != nil {
			return nil, err
		}
		conv[k] = d
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, conv); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeCanonical emits JSON with object keys sorted and no HTML escaping, the
// minimum needed for two independent implementations to agree on the bytes.
func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case string:
		return writeString(buf, t)
	case float64:
		// json.Marshal renders float64 the way ECMAScript does, which is the
		// same rule JSON canonicalisation schemes use.
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case nil:
		buf.WriteString("null")
	default:
		return fmt.Errorf("canonical json: unsupported type %T", v)
	}
	return nil
}

func writeString(buf *bytes.Buffer, s string) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	// Encoder appends a newline; trim it back off.
	start := buf.Len()
	if err := enc.Encode(s); err != nil {
		return err
	}
	b := buf.Bytes()
	if b[len(b)-1] == '\n' {
		buf.Truncate(buf.Len() - 1)
	}
	_ = start
	return nil
}

// Path is where a record with the given digest is stored inside the docket ref's
// tree. Fanning out by the first two hex characters keeps the tree usable in
// repositories with tens of thousands of commits.
func Path(digest string) string {
	h := digest
	if len(h) > 7 && h[:7] == "sha256:" {
		h = h[7:]
	}
	if len(h) < 4 {
		return "records/" + h + ".json"
	}
	return "records/" + h[:2] + "/" + h[2:] + ".json"
}

// CommitPath is the by-commit index entry, so a record can be found from a
// commit id alone when the trailer has been lost (a squash merge, a rebase).
func CommitPath(commit string) string {
	if len(commit) < 4 {
		return "by-commit/" + commit
	}
	return "by-commit/" + commit[:2] + "/" + commit[2:]
}

// Ref is the orphan ref docket records live on.
const Ref = "refs/docket/records"

// Trailer is the git trailer key that binds a commit to its record.
const Trailer = "Docket"
