// Package codex reads Codex CLI sessions.
//
// Codex writes a rollout file per session, one JSON record per line, under
// ~/.codex/sessions/<year>/<month>/<day>/. The records docket cares about are
// apply_patch tool calls, which carry the patch the agent wrote, and
// exec_command calls, which carry shell commands and — unlike Claude Code —
// the exit status of each one.
//
// Codex sends patches rather than whole files, so an edit here usually has no
// pre-image. The replay handles that by seeding from the base revision: see
// timeline.Options.Seed.
package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/transcript"
)

// Home returns the Codex data directory.
func Home() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// Find returns rollout files for sessions that ran inside a repository.
func Find(repoRoot string) ([]string, error) {
	root := filepath.Join(Home(), "sessions")
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable directory is not a reason to stop
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if cwd, ok := peekCWD(path); ok && within(abs, cwd) {
			out = append(out, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return out, nil
	}
	sort.Strings(out)
	return out, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// peekCWD reads the session_meta record, which Codex writes first.
func peekCWD(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	for n := 0; sc.Scan() && n < 5; n++ {
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				CWD string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Payload.CWD != "" {
			return rec.Payload.CWD, true
		}
	}
	return "", false
}

const maxLine = 64 << 20

type record struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type payload struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	ID    string `json:"id"`
	CWD   string `json:"cwd"`
	Model string `json:"model"`
	// function_call
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
	// custom_tool_call
	Input  string `json:"input"`
	Status string `json:"status"`
	// outputs
	Output json.RawMessage `json:"output"`
	// event_msg
	Message string `json:"message"`
	// session_meta
	CLIVersion string `json:"cli_version"`
	// turn_context
	ApprovalPolicy string `json:"approval_policy"`
	SandboxPolicy  struct {
		Type string `json:"type"`
	} `json:"sandbox_policy"`
}

// The approval policy alone does not decide whether a patch was prompted for:
// "on-request" asks only when the sandbox would refuse the write, so a patch
// that applied under a read-only sandbox was approved and one under
// workspace-write was not.
func gate(policy, sandbox string) (string, string) {
	detail := policy
	if sandbox != "" {
		detail = policy + " approval, " + sandbox + " sandbox"
	}
	switch {
	case policy == "":
		return "", detail
	case policy == "untrusted":
		return transcript.GatePrompted, detail
	case policy == "never":
		return transcript.GateAuto, detail
	case sandbox == "read-only":
		return transcript.GatePrompted, detail
	default:
		return transcript.GateAuto, detail
	}
}

// Parse reads one rollout file.
func Parse(path string) (*transcript.Session, transcript.ParseStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, transcript.ParseStats{}, err
	}
	defer f.Close()

	s := &transcript.Session{Path: path, Agent: transcript.AgentCodex}
	var stats transcript.ParseStats

	pend := map[string]pendingCall{}

	lastAgentText, lastPrompt, model, policy, sandbox := "", "", "", "", ""
	seq := 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	for sc.Scan() {
		stats.Lines++
		var rec record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			stats.Unparsable++
			continue
		}
		var p payload
		if len(rec.Payload) > 0 {
			_ = json.Unmarshal(rec.Payload, &p)
		}
		at := parseTime(rec.Timestamp)
		if !at.IsZero() {
			if s.Started.IsZero() || at.Before(s.Started) {
				s.Started = at
			}
			if at.After(s.Ended) {
				s.Ended = at
			}
		}

		switch rec.Type {
		case "session_meta":
			if p.ID != "" {
				s.ID = p.ID
			}
			if p.CWD != "" {
				s.CWD = p.CWD
			}
			if p.CLIVersion != "" {
				s.Version = p.CLIVersion
			}
		case "turn_context":
			if p.Model != "" {
				model = p.Model
				s.Model = p.Model
			}
			if p.ApprovalPolicy != "" {
				policy = p.ApprovalPolicy
			}
			if p.SandboxPolicy.Type != "" {
				sandbox = p.SandboxPolicy.Type
			}
			if p.CWD != "" && s.CWD == "" {
				s.CWD = p.CWD
			}
		case "event_msg":
			switch p.Type {
			case "user_message":
				if t := strings.TrimSpace(p.Message); t != "" {
					lastPrompt = t
					lastAgentText = "" // said before this prompt, about something else
					s.Prompts = append(s.Prompts, transcript.Prompt{Text: t, At: at})
				}
			case "agent_message":
				if t := strings.TrimSpace(p.Message); t != "" {
					lastAgentText = t
				}
			}
		case "response_item":
			switch p.Type {
			case "custom_tool_call", "function_call", "local_shell_call":
				pend[p.CallID] = pendingCall{
					name: p.Name, input: p.Input, args: p.Arguments, at: at,
					intent: lastAgentText, task: lastPrompt, model: model, policy: policy, sandbox: sandbox,
				}
			case "custom_tool_call_output", "function_call_output":
				call, ok := pend[p.CallID]
				if !ok {
					stats.ResultsOrphaned++
					continue
				}
				delete(pend, p.CallID)
				output := decodeOutput(p.Output)
				switch call.name {
				case "apply_patch":
					if !patchApplied(output) {
						// A rejected patch changed nothing, so it is not an edit.
						continue
					}
					for _, e := range editsFromPatch(call.input, s.ID, p.CallID, &seq, call, at) {
						s.Edits = append(s.Edits, e)
						s.Events = append(s.Events, e)
					}
				case "exec_command", "shell", "local_shell":
					seq++
					c := commandFrom(p.CallID, seq, call.args, output, at, s.ID, call.model)
					if c == nil {
						continue
					}
					s.Commands = append(s.Commands, c)
					s.Events = append(s.Events, c)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		stats.OversizeLines++
	}
	if s.ID == "" {
		s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return s, stats, nil
}

// decodeOutput unwraps the two shapes an output takes: a bare string, or a JSON
// object with the text under "output".
func decodeOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		var wrapped struct {
			Output string `json:"output"`
		}
		if strings.HasPrefix(strings.TrimSpace(text), "{") && json.Unmarshal([]byte(text), &wrapped) == nil && wrapped.Output != "" {
			return wrapped.Output
		}
		return text
	}
	var obj struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Output
	}
	return string(raw)
}

func patchApplied(output string) bool {
	if output == "" {
		return true // no output recorded; the call is marked completed elsewhere
	}
	return !strings.Contains(output, "error") || strings.Contains(output, "Success")
}

var (
	exitCodeRe = regexp.MustCompile(`(?m)^Process exited with code (\d+)`)
	outputRe   = regexp.MustCompile(`(?s)\nOutput:\n(.*)$`)
)

func commandFrom(callID string, seq int, arguments, output string, at time.Time, session, model string) *transcript.Command {
	var args struct {
		Cmd     string   `json:"cmd"`
		Command []string `json:"command"`
		Workdir string   `json:"workdir"`
	}
	_ = json.Unmarshal([]byte(arguments), &args)
	cmd := args.Cmd
	if cmd == "" && len(args.Command) > 0 {
		// The shell tool passes an argv, usually ["bash","-lc","<script>"].
		cmd = args.Command[len(args.Command)-1]
	}
	if cmd == "" {
		return nil
	}
	c := &transcript.Command{
		ID: callID, Sequence: seq, At: at, Command: cmd,
		Actor: transcript.ActorAgent, SessionID: session, Model: model,
	}
	if m := outputRe.FindStringSubmatch(output); m != nil {
		c.Stdout = m[1]
	} else {
		c.Stdout = output
	}
	if m := exitCodeRe.FindStringSubmatch(output); m != nil {
		if code, err := strconv.Atoi(m[1]); err == nil {
			c.ExitCode = &code
		}
	}
	transcript.ClassifyCommand(c)
	return c
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------- patches

// pendingCall is a tool call waiting for its output record.
type pendingCall struct {
	name    string
	input   string
	args    string
	at      time.Time
	intent  string
	task    string
	model   string
	policy  string // approval policy in force when the call was made
	sandbox string // sandbox policy in force when the call was made
}

// editsFromPatch turns one apply_patch call into an edit per file it touched.
func editsFromPatch(input, session, callID string, seq *int, call pendingCall, at time.Time) []*transcript.FileEdit {
	files := ParsePatch(input)
	out := make([]*transcript.FileEdit, 0, len(files))
	for _, f := range files {
		if f.Deleted {
			// A deletion adds no lines, and recording it as an edit with an empty
			// post-image would let it swallow the provenance of the whole file.
			continue
		}
		*seq++
		e := &transcript.FileEdit{
			ID: callID + "#" + f.Path, Tool: "apply_patch", Path: f.Path,
			Sequence: *seq, At: at, Actor: transcript.ActorAgent,
			AgentID: "codex/main", Model: call.model, SessionID: session,
			Task: call.task, Intent: call.intent, Source: transcript.SourceTranscript,
		}
		e.Gate, e.GateDetail = gate(call.policy, call.sandbox)
		if f.Added {
			e.Created = true
			e.HasPre = true
			e.Post = f.Content
			e.HasPost = true
		} else {
			e.Patch = f.Ops
		}
		out = append(out, e)
	}
	return out
}

// PatchFile is one file's worth of an apply_patch payload.
type PatchFile struct {
	Path    string
	Added   bool
	Deleted bool
	MoveTo  string
	Content []string // for an added file
	Ops     []transcript.EditOp
}

// ParsePatch reads the apply_patch dialect: a Begin/End envelope around
// per-file sections, with hunks separated by @@ and no line numbers anywhere.
// Location comes from the context lines, which is why the replay matches hunks
// by content rather than position.
func ParsePatch(input string) []PatchFile {
	var files []PatchFile
	var cur *PatchFile
	var op *transcript.EditOp

	flushOp := func() {
		if cur != nil && op != nil && (len(op.OldText) > 0 || len(op.NewText) > 0) {
			cur.Ops = append(cur.Ops, *op)
		}
		op = nil
	}
	flushFile := func() {
		flushOp()
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}

	for _, line := range strings.Split(input, "\n") {
		switch {
		case strings.HasPrefix(line, "*** Begin Patch"), strings.HasPrefix(line, "*** End Patch"):
			if strings.HasPrefix(line, "*** End Patch") {
				flushFile()
			}
		case strings.HasPrefix(line, "*** Update File: "):
			flushFile()
			cur = &PatchFile{Path: strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))}
			op = &transcript.EditOp{}
		case strings.HasPrefix(line, "*** Add File: "):
			flushFile()
			cur = &PatchFile{Path: strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: ")), Added: true}
		case strings.HasPrefix(line, "*** Delete File: "):
			flushFile()
			cur = &PatchFile{Path: strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: ")), Deleted: true}
		case strings.HasPrefix(line, "*** Move to: "):
			if cur != nil {
				cur.MoveTo = strings.TrimSpace(strings.TrimPrefix(line, "*** Move to: "))
			}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "@@"):
			flushOp()
			op = &transcript.EditOp{}
		case cur.Added:
			cur.Content = append(cur.Content, strings.TrimPrefix(line, "+"))
		case op == nil:
			continue
		case strings.HasPrefix(line, "+"):
			op.NewText = append(op.NewText, line[1:])
		case strings.HasPrefix(line, "-"):
			op.OldText = append(op.OldText, line[1:])
		case strings.HasPrefix(line, " "):
			op.OldText = append(op.OldText, line[1:])
			op.NewText = append(op.NewText, line[1:])
		case line == "":
			// A blank line inside a hunk is an empty context line.
			op.OldText = append(op.OldText, "")
			op.NewText = append(op.NewText, "")
		}
	}
	flushFile()
	return files
}

// Sessions reads every Codex session that ran inside a repository.
func Sessions(repoRoot string) ([]*transcript.Session, []transcript.ParseStats, error) {
	paths, err := Find(repoRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("codex: %w", err)
	}
	var sessions []*transcript.Session
	var stats []transcript.ParseStats
	for _, p := range paths {
		s, st, err := Parse(p)
		if err != nil {
			continue
		}
		sessions = append(sessions, s)
		stats = append(stats, st)
	}
	return sessions, stats, nil
}
