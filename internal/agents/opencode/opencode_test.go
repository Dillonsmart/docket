package opencode

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test builds a database with the same shape opencode uses, then reads it
// back through the real code path. Anything less would be testing a mock of the
// format rather than the format.
func buildDB(t *testing.T, dir string, parts []map[string]any) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is not installed")
	}
	db := filepath.Join(t.TempDir(), "opencode.db")

	var sql strings.Builder
	sql.WriteString(`
create table session (id text primary key, project_id text, workspace_id text, parent_id text,
  slug text, directory text not null, path text, title text, version text, time_created integer);
create table message (id text primary key, session_id text, time_created integer, time_updated integer, data text);
create table part (id text primary key, message_id text, session_id text, time_created integer, time_updated integer, data text);
`)
	sql.WriteString("insert into session values ('ses_1','p','w',null,'s','" + dir + "',null,'Test','1.18.26',1000);\n")
	msg, _ := json.Marshal(map[string]any{"role": "assistant", "modelID": "claude-opus-5", "agent": "build"})
	sql.WriteString("insert into message values ('msg_1','ses_1',1000,1000,'" + escape(string(msg)) + "');\n")
	userMsg, _ := json.Marshal(map[string]any{"role": "user"})
	sql.WriteString("insert into message values ('msg_0','ses_1',900,900,'" + escape(string(userMsg)) + "');\n")

	for i, p := range parts {
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		msgID := "msg_1"
		if p["__user"] == true {
			msgID = "msg_0"
		}
		sql.WriteString("insert into part values ('part_" + string(rune('a'+i)) + "','" + msgID + "','ses_1'," +
			itoa(1000+i) + "," + itoa(1000+i) + ",'" + escape(string(data)) + "');\n")
	}

	cmd := exec.Command("sqlite3", db)
	cmd.Stdin = strings.NewReader(sql.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fixture database: %v: %s", err, out)
	}
	t.Setenv("DOCKET_OPENCODE_DB", db)
	return db
}

func escape(s string) string { return strings.ReplaceAll(s, "'", "''") }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func tool(name string, input, metadata map[string]any) map[string]any {
	return map[string]any{
		"type": "tool", "tool": name, "callID": "call_" + name,
		"state": map[string]any{"status": "completed", "input": input, "metadata": metadata, "output": "ok"},
	}
}

func TestReadsWritesEditsAndCommands(t *testing.T) {
	dir := "/repo"
	buildDB(t, dir, []map[string]any{
		{"type": "text", "text": "Add session minting", "__user": true},
		{"type": "text", "text": "Minting a prefixed id instead."},
		tool("write", map[string]any{"filePath": dir + "/src/auth.js", "content": "export const a = 1;\n"},
			map[string]any{"exists": false}),
		tool("edit", map[string]any{"filePath": dir + "/src/auth.js",
			"oldString": "export const a = 1;", "newString": "export const a = 2;"},
			map[string]any{"diff": ""}),
		tool("bash", map[string]any{"command": "npx vitest run", "description": "tests"},
			map[string]any{"output": "Tests  3 passed", "exit": 0}),
	})

	sessions, err := Sessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	s := sessions[0]
	if s.Agent != "opencode" || s.Model != "claude-opus-5" {
		t.Errorf("session identity = %+v", s)
	}
	if len(s.Edits) != 2 {
		t.Fatalf("got %d edits, want 2", len(s.Edits))
	}

	// A write records the whole file, and "exists": false means the pre-image is
	// known to be empty rather than unknown.
	w := s.Edits[0]
	if !w.HasPost || len(w.Post) != 1 || !w.Created || !w.HasPre {
		t.Errorf("write = %+v", w)
	}
	if w.Task != "Add session minting" || !strings.HasPrefix(w.Intent, "Minting") {
		t.Errorf("write context: task %q intent %q", w.Task, w.Intent)
	}

	// An edit records only the swap, so the replay resolves it against the file
	// it actually ran against.
	e := s.Edits[1]
	if e.Replace == nil || e.Replace.Old != "export const a = 1;" {
		t.Errorf("edit = %+v", e)
	}
	if e.HasPost {
		t.Error("an edit with no recorded images must not claim a post-image")
	}

	if len(s.Commands) != 1 {
		t.Fatalf("got %d commands", len(s.Commands))
	}
	c := s.Commands[0]
	if c.ExitCode == nil || *c.ExitCode != 0 {
		t.Errorf("exit code = %v", c.ExitCode)
	}
	if c.Test == nil || c.Test.Outcome != "pass" || c.Test.Confidence != "reported_exit_code" {
		t.Errorf("test = %+v", c.Test)
	}
}

func TestEmptyFileWriteIsStillAnEdit(t *testing.T) {
	dir := "/repo"
	buildDB(t, dir, []map[string]any{
		tool("write", map[string]any{"filePath": dir + "/app/__init__.py", "content": ""},
			map[string]any{"exists": false}),
	})
	sessions, err := Sessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || len(sessions[0].Edits) != 1 {
		t.Fatalf("an empty file write was dropped: %+v", sessions)
	}
	if !sessions[0].Edits[0].HasPost {
		t.Error("writing an empty file is a known post-image, not a missing one")
	}
}

func TestSessionsElsewhereAreIgnored(t *testing.T) {
	buildDB(t, "/somewhere/else", []map[string]any{
		tool("write", map[string]any{"filePath": "/somewhere/else/x.js", "content": "1\n"}, nil),
	})
	sessions, err := Sessions("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("read a session from another directory: %+v", sessions)
	}
}
