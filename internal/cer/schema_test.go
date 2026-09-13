package cer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The specification and this package are two descriptions of one format, and
// they drift the moment a member is renamed on one side only. This is not a full
// JSON Schema validator: it checks that every member the schema requires is
// actually emitted, and that the values sit inside the enums the schema allows.
func TestRecordSatisfiesPublishedSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "spec", "cer-0.1.schema.json"))
	if err != nil {
		t.Fatalf("the spec must ship with the implementation: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}

	rec := sampleRecord()
	rec.Commit = "8f3a2b1c"
	rec.Hunks[0].Evidence = []Evidence{{Kind: "coverage", Ref: "coverage/lcov.info", Result: "pass", Observed: true}}
	rec.Hunks[0].Attempts = []Attempt{{Summary: "tried the middleware", Outcome: "abandoned"}}
	rec.Sessions = []Session{{ID: "s1", Agent: "claude-code"}}
	signer, err := LocalSigner(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.Sign(rec); err != nil {
		t.Fatal(err)
	}
	canonical, err := rec.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(canonical, &doc); err != nil {
		t.Fatal(err)
	}

	for _, name := range required(schema) {
		if _, ok := doc[name]; !ok {
			t.Errorf("record is missing the required member %q", name)
		}
	}

	defs, _ := schema["$defs"].(map[string]any)
	hunkSchema, _ := defs["hunk"].(map[string]any)
	hunks, _ := doc["hunks"].([]any)
	if len(hunks) == 0 {
		t.Fatal("no hunks in the sample record")
	}
	hunk, _ := hunks[0].(map[string]any)
	for _, name := range required(hunkSchema) {
		if _, ok := hunk[name]; !ok {
			t.Errorf("hunk is missing the required member %q", name)
		}
	}

	// Enum members the implementation is responsible for.
	enums := map[string][]string{
		"trust":                  {TrustLocalClaimed, TrustCIAttested},
		"docket_version":         {Version},
		"spec":                   {SpecID},
		"human_contact":          {ContactNone, ContactEdited, ContactApproved, ContactViewed},
		"attribution_confidence": {"high", "medium", "none"},
	}
	check := func(where map[string]any, key string, allowed []string) {
		v, ok := where[key].(string)
		if !ok {
			return
		}
		for _, a := range allowed {
			if v == a {
				return
			}
		}
		t.Errorf("%s = %q is outside the values the schema allows %v", key, v, allowed)
	}
	check(doc, "trust", enums["trust"])
	check(doc, "docket_version", enums["docket_version"])
	check(doc, "spec", enums["spec"])
	check(hunk, "human_contact", enums["human_contact"])
	check(hunk, "attribution_confidence", enums["attribution_confidence"])
}

func required(schema map[string]any) []string {
	raw, _ := schema["required"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
