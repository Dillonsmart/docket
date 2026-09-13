package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shapes here mirror what Claude Code actually writes: the tool call on an
// assistant record, and the result on a user record carrying toolUseResult.
func writeTranscript(t *testing.T, records []map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	var b strings.Builder
	for _, r := range records {
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
	return path
}

func toolUse(uuid, id, name string, input map[string]any, text string) map[string]any {
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	content = append(content, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
	return map[string]any{
		"type": "assistant", "uuid": uuid, "sessionId": "s1", "timestamp": "2026-09-13T10:00:00.000Z",
		"cwd": "/repo", "message": map[string]any{"role": "assistant", "model": "claude-opus-5", "content": content},
	}
}

func toolResult(uuid, id string, result any) map[string]any {
	return map[string]any{
		"type": "user", "uuid": uuid, "sessionId": "s1", "timestamp": "2026-09-13T10:00:01.000Z",
		"message":       map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id}}},
		"toolUseResult": result,
	}
}

func TestParseWriteAndEdit(t *testing.T) {
	original := "package main\n\nfunc main() {}\n"
	updated := "package main\n\nfunc main() { run() }\n"

	path := writeTranscript(t, []map[string]any{
		{"type": "user", "uuid": "u0", "sessionId": "s1", "timestamp": "2026-09-13T09:59:00.000Z",
			"message": map[string]any{"role": "user", "content": "Make main call run"}},
		toolUse("a1", "t1", "Write", map[string]any{"file_path": "/repo/main.go", "content": original}, "Creating the entry point."),
		toolResult("u1", "t1", map[string]any{
			"type": "create", "filePath": "/repo/main.go", "content": original,
			"originalFile": nil, "structuredPatch": []any{}, "userModified": false}),
		toolUse("a2", "t2", "Edit", map[string]any{"file_path": "/repo/main.go"}, "Calling run."),
		toolResult("u2", "t2", map[string]any{
			"filePath": "/repo/main.go", "oldString": "func main() {}", "newString": "func main() { run() }",
			"originalFile": original, "structuredPatch": []any{}, "userModified": false, "replaceAll": false}),
	})

	s, stats, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Edits) != 2 {
		t.Fatalf("got %d edits, want 2", len(s.Edits))
	}
	if stats.Unparsable != 0 {
		t.Errorf("unparsable lines: %d", stats.Unparsable)
	}

	create := s.Edits[0]
	if !create.Created || !create.HasPre || len(create.Pre) != 0 {
		t.Errorf("a Write create should have a known-empty pre-image: %+v", create)
	}
	if !create.HasPost || len(create.Post) != 3 {
		t.Errorf("post-image = %v", create.Post)
	}
	if create.Task != "Make main call run" {
		t.Errorf("task = %q", create.Task)
	}
	if create.Intent != "Creating the entry point." {
		t.Errorf("intent = %q", create.Intent)
	}
	if create.Model != "claude-opus-5" || create.Source != SourceTranscript {
		t.Errorf("identity = %+v", create)
	}

	change := s.Edits[1]
	if !change.HasPost || strings.Join(change.Post, "\n")+"\n" != updated {
		t.Errorf("edit post-image = %q", strings.Join(change.Post, "\n"))
	}
}

// An Edit whose old_string is a whole file ends with a newline. Reconstructing
// the pre-image from split lines drops it, the replacement then fails to match,
// and the edit becomes unattributable — which is how this was found.
func TestEditReplacingWholeFileKeepsTrailingNewline(t *testing.T) {
	original := "one\ntwo\n"
	replacement := "three\nfour\n"
	path := writeTranscript(t, []map[string]any{
		toolUse("a1", "t1", "Edit", map[string]any{"file_path": "/repo/a.txt"}, ""),
		toolResult("u1", "t1", map[string]any{
			"filePath": "/repo/a.txt", "oldString": original, "newString": replacement,
			"originalFile": original, "structuredPatch": []any{}}),
	})
	s, _, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Edits[0]
	if !e.HasPost {
		t.Fatal("edit could not be reconstructed")
	}
	if got := strings.Join(e.Post, "\n"); got != "three\nfour" {
		t.Errorf("post = %q", got)
	}
}

func TestTestRunsAndMutationsAreClassified(t *testing.T) {
	path := writeTranscript(t, []map[string]any{
		toolUse("a1", "t1", "Bash", map[string]any{"command": "npx vitest run", "description": "tests"}, ""),
		toolResult("u1", "t1", map[string]any{"stdout": " Tests  3 passed", "stderr": "", "interrupted": false}),
		toolUse("a2", "t2", "Bash", map[string]any{"command": "npx vitest run", "description": "tests"}, ""),
		toolResult("u2", "t2", map[string]any{"stdout": " FAIL  a.spec.ts\n Tests  1 failed | 2 passed", "stderr": "", "interrupted": false}),
		toolUse("a3", "t3", "Bash", map[string]any{"command": "sed -i '' s/a/b/ src/x.ts", "description": "rename"}, ""),
		toolResult("u3", "t3", map[string]any{"stdout": "", "stderr": "", "interrupted": false}),
	})
	s, _, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Commands) != 3 {
		t.Fatalf("got %d commands", len(s.Commands))
	}
	if s.Commands[0].Test == nil || s.Commands[0].Test.Outcome != "pass" {
		t.Errorf("first run = %+v", s.Commands[0].Test)
	}
	if s.Commands[1].Test == nil || s.Commands[1].Test.Outcome != "fail" {
		t.Errorf("second run = %+v", s.Commands[1].Test)
	}
	if !s.Commands[2].MayMutateFiles {
		t.Error("sed -i must be recognised as able to change files unseen")
	}
}

func TestHumanRunCommandsAndPromptFiltering(t *testing.T) {
	path := writeTranscript(t, []map[string]any{
		{"type": "user", "uuid": "u1", "sessionId": "s1", "timestamp": "2026-09-13T10:00:00.000Z",
			"message": map[string]any{"role": "user", "content": "<command-name>/model</command-name>"}},
		{"type": "user", "uuid": "u2", "sessionId": "s1", "timestamp": "2026-09-13T10:00:01.000Z",
			"message": map[string]any{"role": "user", "content": "<bash-input>git checkout -- src/x.ts</bash-input>"}},
		{"type": "user", "uuid": "u3", "sessionId": "s1", "timestamp": "2026-09-13T10:00:02.000Z",
			"message": map[string]any{"role": "user", "content": "Now fix the bug"}},
	})
	s, _, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Prompts) != 1 || s.Prompts[0].Text != "Now fix the bug" {
		t.Errorf("prompts = %+v", s.Prompts)
	}
	if len(s.Commands) != 1 || s.Commands[0].Actor != ActorHuman {
		t.Fatalf("human command not recorded: %+v", s.Commands)
	}
	if !s.Commands[0].MayMutateFiles {
		t.Error("git checkout run by the human must open a mutation window")
	}
}

func TestApplyPatchRefusesAMismatch(t *testing.T) {
	pre := []string{"a", "b", "c"}
	ops := []EditOp{{OldStart: 2, OldText: []string{"WRONG"}, NewText: []string{"B"}}}
	if _, ok := applyPatch(pre, ops); ok {
		t.Error("a patch that does not match its pre-image must not be applied")
	}
	ok := []EditOp{{OldStart: 2, OldText: []string{"b"}, NewText: []string{"B"}}}
	got, applied := applyPatch(pre, ok)
	if !applied || strings.Join(got, "") != "aBc" {
		t.Errorf("patch application = %v %v", got, applied)
	}
}

// A runner named as a flag is not a test run. `laravel new --pest` scaffolds a
// project; counting it as a check would put evidence on a hunk nothing checked.
func TestRunnerMentionedAsAFlagIsNotATestRun(t *testing.T) {
	path := writeTranscript(t, []map[string]any{
		toolUse("a1", "t1", "Bash", map[string]any{"command": "laravel new villains --database=pgsql --pest --no-interaction"}, ""),
		toolResult("u1", "t1", map[string]any{"stdout": "Application ready", "stderr": "", "interrupted": false}),
		toolUse("a2", "t2", "Bash", map[string]any{"command": "./vendor/bin/pest --colors=never"}, ""),
		toolResult("u2", "t2", map[string]any{"stdout": "  Tests:    25 passed (76 assertions)", "stderr": "", "interrupted": false}),
	})
	s, _, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Commands[0].Test != nil {
		t.Errorf("scaffolding command was read as a test run: %+v", s.Commands[0].Test)
	}
	if s.Commands[1].Test == nil || s.Commands[1].Test.Outcome != "pass" {
		t.Errorf("real pest run = %+v", s.Commands[1].Test)
	}
}

// Agents often append `; echo "EXIT=$?"` because the transcript records no exit
// code. Believe it — unless a pipe means it is reporting the exit code of tail.
func TestReportedExitCodeIsUsedOnlyWhenItMeansAnything(t *testing.T) {
	path := writeTranscript(t, []map[string]any{
		toolUse("a1", "t1", "Bash", map[string]any{"command": `go test ./...; echo "EXIT=$?"`}, ""),
		toolResult("u1", "t1", map[string]any{"stdout": "some output\nEXIT=1", "stderr": "", "interrupted": false}),
		toolUse("a2", "t2", "Bash", map[string]any{"command": `go test ./... 2>&1 | tail -5; echo "EXIT=$?"`}, ""),
		toolResult("u2", "t2", map[string]any{"stdout": "--- FAIL: TestThing\nFAIL\nEXIT=0", "stderr": "", "interrupted": false}),
	})
	s, _, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Commands[0].Test; got == nil || got.Outcome != "fail" || got.Confidence != "reported_exit_code" {
		t.Errorf("unpiped run = %+v, want a failure read from the exit code", got)
	}
	// Through a pipe the marker is tail's exit code, so the output has the say.
	if got := s.Commands[1].Test; got == nil || got.Outcome != "fail" || got.Confidence != "output_pattern" {
		t.Errorf("piped run = %+v, want a failure read from the output", got)
	}
}
