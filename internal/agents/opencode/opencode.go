// Package opencode reads opencode sessions.
//
// opencode keeps its history in a SQLite database rather than in files:
// ~/.local/share/opencode/opencode.db, with a row per message and a row per
// part. The parts docket cares about are edit, write and bash tool calls, each
// carrying its input, a unified diff and — for bash — the exit code.
//
// Reading it goes through the sqlite3 command line tool. Linking a SQLite
// driver into docket would mean cgo or a large pure-Go dependency in a binary
// that has neither, and this data is read once per commit, not on every
// keystroke. When sqlite3 is absent, docket says so rather than silently
// reporting that opencode wrote nothing.
package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/diffx"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// ErrNoSQLite is returned when the sqlite3 tool is not installed.
var ErrNoSQLite = errors.New("opencode sessions are stored in SQLite and the sqlite3 command is not installed")

// DatabasePath returns the opencode database location.
func DatabasePath() string {
	if p := os.Getenv("DOCKET_OPENCODE_DB"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode", "opencode.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

// Available reports whether there is anything to read and a way to read it.
func Available() (dbPath string, err error) {
	db := DatabasePath()
	if db == "" {
		return "", os.ErrNotExist
	}
	if _, err := os.Stat(db); err != nil {
		return "", err
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return db, ErrNoSQLite
	}
	return db, nil
}

// query runs one SQL statement and returns the rows as JSON.
//
// The database is opened read-only through a file: URI, so docket can never
// disturb a session that is running while a commit is made.
func query(db, sql string) ([]map[string]any, error) {
	uri := "file:" + db + "?mode=ro&immutable=0"
	cmd := exec.Command("sqlite3", "-readonly", "-json", uri, sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sqlite3: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("sqlite3 returned something unreadable: %w", err)
	}
	return rows, nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Sessions reads every opencode session that ran inside a repository.
func Sessions(repoRoot string) ([]*transcript.Session, error) {
	db, err := Available()
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	rows, err := query(db, fmt.Sprintf(
		`select id, directory, title, version, time_created from session where directory = %s or directory like %s order by time_created`,
		quote(abs), quote(abs+"/%")))
	if err != nil {
		return nil, err
	}
	var out []*transcript.Session
	for _, r := range rows {
		s, err := readSession(db, str(r["id"]), str(r["directory"]), str(r["version"]))
		if err != nil || s == nil {
			continue
		}
		if len(s.Edits) == 0 && len(s.Commands) == 0 {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func readSession(db, id, dir, version string) (*transcript.Session, error) {
	s := &transcript.Session{ID: id, Agent: transcript.AgentOpencode, CWD: dir, Version: version}

	// Messages carry the role and the model; parts carry what was done. Reading
	// them together keeps a tool call attached to the turn that made it.
	msgRows, err := query(db, fmt.Sprintf(
		`select id, time_created, json_extract(data,'$.role') as role,
		        json_extract(data,'$.modelID') as model, json_extract(data,'$.agent') as agent
		 from message where session_id = %s order by time_created, id`, quote(id)))
	if err != nil {
		return nil, err
	}
	type msgInfo struct{ role, model, agent string }
	messages := map[string]msgInfo{}
	for _, m := range msgRows {
		messages[str(m["id"])] = msgInfo{role: str(m["role"]), model: str(m["model"]), agent: str(m["agent"])}
		if mm := str(m["model"]); mm != "" {
			s.Model = mm
		}
	}

	partRows, err := query(db, fmt.Sprintf(
		`select id, message_id, time_created, data from part where session_id = %s order by time_created, id`, quote(id)))
	if err != nil {
		return nil, err
	}

	lastText, lastPrompt := "", ""
	seq := 0
	for _, row := range partRows {
		at := millis(row["time_created"])
		if !at.IsZero() {
			if s.Started.IsZero() || at.Before(s.Started) {
				s.Started = at
			}
			if at.After(s.Ended) {
				s.Ended = at
			}
		}
		var part struct {
			Type  string `json:"type"`
			Tool  string `json:"tool"`
			Text  string `json:"text"`
			State struct {
				Status   string          `json:"status"`
				Input    json.RawMessage `json:"input"`
				Output   string          `json:"output"`
				Metadata json.RawMessage `json:"metadata"`
			} `json:"state"`
		}
		if json.Unmarshal([]byte(str(row["data"])), &part) != nil {
			continue
		}
		info := messages[str(row["message_id"])]

		switch part.Type {
		case "text":
			if t := strings.TrimSpace(part.Text); t != "" {
				if info.role == "user" {
					lastPrompt = t
					lastText = "" // said before this prompt, about something else
					s.Prompts = append(s.Prompts, transcript.Prompt{Text: t, At: at})
				} else {
					lastText = t
				}
			}
		case "tool":
			if part.State.Status != "completed" {
				// A failed or aborted call changed nothing worth recording.
				continue
			}
			switch part.Tool {
			case "write", "edit", "patch":
				seq++
				if e := edit(part.Tool, str(row["id"]), seq, at, part.State.Input, part.State.Metadata, s, info.model, lastPrompt, lastText); e != nil {
					s.Edits = append(s.Edits, e)
					s.Events = append(s.Events, e)
				}
			case "bash":
				seq++
				if c := command(str(row["id"]), seq, at, part.State.Input, part.State.Metadata, s.ID, info.model); c != nil {
					s.Commands = append(s.Commands, c)
					s.Events = append(s.Events, c)
				}
			}
		}
	}
	return s, nil
}

func edit(tool, id string, seq int, at time.Time, input, metadata json.RawMessage, s *transcript.Session, model, task, intent string) *transcript.FileEdit {
	var in struct {
		FilePath string `json:"filePath"`
		// A pointer, because writing an empty file is a real edit and "" is not
		// the same as "not recorded".
		Content   *string `json:"content"`
		OldString string  `json:"oldString"`
		NewString string  `json:"newString"`
	}
	if json.Unmarshal(input, &in) != nil || in.FilePath == "" {
		return nil
	}
	var meta struct {
		Diff string `json:"diff"`
		// Exists says whether the file was there before this write, which is the
		// difference between a known-empty pre-image and an unknown one.
		Exists *bool `json:"exists"`
	}
	_ = json.Unmarshal(metadata, &meta)

	e := &transcript.FileEdit{
		ID: id, Tool: tool, Path: in.FilePath, Sequence: seq, At: at,
		Actor: transcript.ActorAgent, AgentID: "opencode/main", Model: model,
		SessionID: s.ID, Task: task, Intent: intent, Source: transcript.SourceTranscript,
	}
	switch {
	case tool == "write" && in.Content != nil:
		// A write replaces the file, so its content is the post-image.
		e.Post = diffx.SplitLines(*in.Content)
		e.HasPost = true
		if meta.Exists != nil && !*meta.Exists {
			e.Created = true
			e.HasPre = true // known empty: the file did not exist
		}
	case in.OldString != "" || in.NewString != "":
		// An edit is a substring replacement, the same shape Claude Code uses.
		e.Replace = &transcript.Replacement{Old: in.OldString, New: in.NewString}
	}
	if meta.Diff != "" {
		// The recorded diff is the fallback when the replacement cannot be
		// applied, and it carries real line numbers.
		for _, fd := range diffx.ParseUnified(meta.Diff) {
			for _, h := range fd.Hunks {
				op := transcript.EditOp{OldStart: h.OldStart, OldLines: h.OldLines, NewStart: h.NewStart, NewLines: h.NewLines}
				op.OldText = append(op.OldText, h.RemovedText...)
				for _, l := range h.AddedLines {
					op.NewText = append(op.NewText, l.Text)
				}
				e.Patch = append(e.Patch, op)
			}
		}
	}
	return e
}

func command(id string, seq int, at time.Time, input, metadata json.RawMessage, session, model string) *transcript.Command {
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	if json.Unmarshal(input, &in) != nil || in.Command == "" {
		return nil
	}
	var meta struct {
		Output string `json:"output"`
		Exit   *int   `json:"exit"`
	}
	_ = json.Unmarshal(metadata, &meta)

	c := &transcript.Command{
		ID: id, Sequence: seq, At: at, Command: in.Command, Description: in.Description,
		Stdout: meta.Output, ExitCode: meta.Exit,
		Actor: transcript.ActorAgent, SessionID: session, Model: model,
	}
	transcript.ClassifyCommand(c)
	return c
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// millis converts opencode's epoch-millisecond timestamps.
func millis(v any) time.Time {
	switch t := v.(type) {
	case float64:
		return time.UnixMilli(int64(t)).UTC()
	case string:
		var n int64
		if _, err := fmt.Sscan(t, &n); err == nil {
			return time.UnixMilli(n).UTC()
		}
	}
	return time.Time{}
}

// SortSessions orders sessions by start time, so a repository worked on in
// several sittings replays in the order it happened.
func SortSessions(in []*transcript.Session) {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Started.Before(in[j].Started) })
}
