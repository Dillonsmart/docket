package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The records here mirror a real rollout file: session_meta first, then
// turn_context, then response_item pairs for each tool call.
func writeRollout(t *testing.T, cwd string, records []map[string]any) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "09", "13")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-09-13T10-00-00-abc.jsonl")
	var b strings.Builder
	meta := map[string]any{
		"timestamp": "2026-09-13T10:00:00.000Z", "type": "session_meta",
		"payload": map[string]any{"id": "sess-codex", "cwd": cwd, "cli_version": "0.116.0"},
	}
	for _, r := range append([]map[string]any{meta}, records...) {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	return path
}

func item(ts string, payload map[string]any) map[string]any {
	return map[string]any{"timestamp": ts, "type": "response_item", "payload": payload}
}

func event(ts, kind, message string) map[string]any {
	return map[string]any{"timestamp": ts, "type": "event_msg",
		"payload": map[string]any{"type": kind, "message": message}}
}

const addPatch = `*** Begin Patch
*** Add File: /repo/src/auth.py
+def new_session(user):
+    return {"id": mint_id(), "user": user}
*** End Patch
`

const updatePatch = `*** Begin Patch
*** Update File: /repo/src/auth.py
@@
 def new_session(user):
-    return {"id": mint_id(), "user": user}
+    return {"id": mint_id(), "user": user, "created": now()}
*** End Patch
`

func TestParseRollout(t *testing.T) {
	path := writeRollout(t, "/repo", []map[string]any{
		{"timestamp": "2026-09-13T10:00:01.000Z", "type": "turn_context",
			"payload": map[string]any{"model": "gpt-5.4", "cwd": "/repo"}},
		event("2026-09-13T10:00:02.000Z", "user_message", "Add session minting"),
		event("2026-09-13T10:00:03.000Z", "agent_message", "Minting a prefixed id so a fixated session cannot be reused."),
		item("2026-09-13T10:00:04.000Z", map[string]any{
			"type": "custom_tool_call", "name": "apply_patch", "call_id": "c1", "input": addPatch}),
		item("2026-09-13T10:00:05.000Z", map[string]any{
			"type": "custom_tool_call_output", "call_id": "c1",
			"output": `{"output":"Success. Updated the following files:\nA /repo/src/auth.py\n","metadata":{"exit_code":0}}`}),
		item("2026-09-13T10:00:06.000Z", map[string]any{
			"type": "function_call", "name": "exec_command", "call_id": "c2",
			"arguments": `{"cmd":"pytest -q","workdir":"/repo"}`}),
		item("2026-09-13T10:00:07.000Z", map[string]any{
			"type": "function_call_output", "call_id": "c2",
			"output": "Command: /bin/zsh -lc pytest -q\nProcess exited with code 1\nOutput:\n1 failed, 2 passed\n"}),
	})

	s, stats, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Unparsable != 0 {
		t.Errorf("unparsable lines: %d", stats.Unparsable)
	}
	if s.ID != "sess-codex" || s.Model != "gpt-5.4" || s.Agent != "codex" {
		t.Errorf("session identity = %+v", s)
	}
	if len(s.Edits) != 1 {
		t.Fatalf("got %d edits, want 1", len(s.Edits))
	}
	e := s.Edits[0]
	if e.Path != "/repo/src/auth.py" || !e.Created || !e.HasPost || len(e.Post) != 2 {
		t.Errorf("edit = %+v", e)
	}
	if e.Task != "Add session minting" {
		t.Errorf("task = %q", e.Task)
	}
	if !strings.HasPrefix(e.Intent, "Minting a prefixed id") {
		t.Errorf("intent = %q", e.Intent)
	}

	if len(s.Commands) != 1 {
		t.Fatalf("got %d commands", len(s.Commands))
	}
	c := s.Commands[0]
	if c.Command != "pytest -q" {
		t.Errorf("command = %q", c.Command)
	}
	// Codex records the exit status, which settles the outcome without reading
	// the output at all.
	if c.ExitCode == nil || *c.ExitCode != 1 {
		t.Errorf("exit code = %v", c.ExitCode)
	}
	if c.Test == nil || c.Test.Outcome != "fail" || c.Test.Confidence != "reported_exit_code" {
		t.Errorf("test = %+v", c.Test)
	}
}

func TestFindMatchesOnlySessionsInTheRepository(t *testing.T) {
	writeRollout(t, "/somewhere/else", nil)
	paths, err := Find("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Errorf("found sessions from another directory: %v", paths)
	}
}

func TestParsePatchDialect(t *testing.T) {
	files := ParsePatch(updatePatch)
	if len(files) != 1 {
		t.Fatalf("got %d files", len(files))
	}
	f := files[0]
	if f.Path != "/repo/src/auth.py" || f.Added || f.Deleted {
		t.Errorf("file = %+v", f)
	}
	if len(f.Ops) != 1 {
		t.Fatalf("got %d hunks", len(f.Ops))
	}
	op := f.Ops[0]
	// The context line belongs to both sides; it is what locates the hunk,
	// since this dialect carries no line numbers.
	if len(op.OldText) != 2 || op.OldText[0] != "def new_session(user):" {
		t.Errorf("old side = %q", op.OldText)
	}
	if len(op.NewText) != 2 || !strings.Contains(op.NewText[1], "created") {
		t.Errorf("new side = %q", op.NewText)
	}
	if op.OldStart != 0 {
		t.Errorf("this dialect has no line numbers, so OldStart should stay 0, got %d", op.OldStart)
	}
}

func TestDeletionsAreNotRecordedAsEdits(t *testing.T) {
	files := ParsePatch("*** Begin Patch\n*** Delete File: /repo/gone.py\n*** End Patch\n")
	if len(files) != 1 || !files[0].Deleted {
		t.Fatalf("parse = %+v", files)
	}
	var seq int
	edits := editsFromPatch("", "s", "c", &seq, pendingCall{}, time.Time{})
	if len(edits) != 0 {
		t.Errorf("an empty patch produced %d edits", len(edits))
	}
}
