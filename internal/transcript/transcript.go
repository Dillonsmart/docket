// Package transcript reads Claude Code session transcripts and turns them into
// an ordered stream of events docket can reason about.
//
// The transcript is the only place the implementation journey survives: the
// tool_result payload for an Edit or Write carries the file's pre-image
// (originalFile), its post-image (content or newString) and a structured patch.
// That is what makes content-verified attribution possible rather than
// timestamp guessing.
//
// Nothing in here guesses. Where the transcript is lossy — an Edit whose
// originalFile was elided, a Bash command that wrote a file we cannot see into
// — the event says so, and the timeline downstream degrades to origin
// "unknown".
package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Agent names for the harnesses docket can read.
const (
	// AgentClaudeCode is Claude Code's session transcript.
	AgentClaudeCode = "claude-code"
	// AgentCodex is the Codex CLI's rollout file.
	AgentCodex = "codex"
	// AgentOpencode is opencode's session database.
	AgentOpencode = "opencode"
	// AgentCollector is docket watching the working tree itself.
	AgentCollector = "docket-collector"
)

// Actor is who performed an action.
type Actor string

const (
	// ActorAgent is the coding agent.
	ActorAgent Actor = "agent"
	// ActorHuman is the person at the keyboard.
	ActorHuman Actor = "human"
	// ActorUnknown is used when the transcript does not say.
	ActorUnknown Actor = "unknown"
)

// Session is one parsed transcript file.
type Session struct {
	ID string
	// Agent names the harness that produced this session: claude-code, codex,
	// opencode, or docket's own collector. It is recorded in every docket,
	// because "which agent wrote this" is a different question from "which
	// model", and an organisation will want to ask both.
	Agent    string
	Path     string
	CWD      string
	Branch   string
	Version  string
	Model    string
	Started  time.Time
	Ended    time.Time
	Prompts  []Prompt
	Edits    []*FileEdit
	Commands []*Command
	// Events is Edits and Commands interleaved in transcript order.
	Events []Event
}

// Prompt is a human turn.
type Prompt struct {
	UUID string
	Text string
	At   time.Time
}

// Event is anything that can appear on the timeline.
type Event interface {
	When() time.Time
	Seq() int
}

// EditOp is one hunk of a structured patch recorded alongside an edit.
type EditOp struct {
	OldStart, OldLines int
	NewStart, NewLines int
	OldText, NewText   []string
}

// Replacement is an edit expressed as an exact substring swap, which is how
// most edit tools describe themselves. It is the fallback when the harness
// records what was swapped but not what the file looked like.
type Replacement struct {
	Old string
	New string
	All bool
}

// FileEdit is a recorded mutation of one file by one tool call.
type FileEdit struct {
	ID       string // the tool_use id: the stable identity of this edit
	Tool     string // Edit, Write, MultiEdit, NotebookEdit
	Path     string // absolute as recorded
	Sequence int
	At       time.Time

	// Pre and Post are the file's content before and after, when the transcript
	// recorded them. HasPre is false when the pre-image was not recorded, which
	// is different from an empty pre-image (a file creation).
	Pre     []string
	Post    []string
	HasPre  bool
	HasPost bool
	Created bool
	// Patch is the structured patch, always present for Edit and available for
	// Write updates. It is the fallback when the images are missing.
	Patch []EditOp
	// Replace is the substring swap this edit performed, when that is all the
	// harness recorded. The replay applies it to the content the edit actually
	// ran against.
	Replace *Replacement

	Actor        Actor
	AgentID      string
	Model        string
	SessionID    string
	IsSidechain  bool
	Task         string // the human request this edit descends from
	Intent       string // what the agent said it was doing, immediately before
	UserModified bool   // the harness saw the human change this file first
	Skill        string

	// Source records how docket came to know about this edit.
	//
	// "transcript" means the agent harness reported it, which docket has to take
	// on trust. "observed" means docket read the file itself before and after the
	// command that changed it. The distinction is recorded in every docket,
	// because the plan's trust tiering starts here: an observed edit is evidence,
	// a reported one is a claim.
	Source string
	// Command is the shell command responsible, for observed edits.
	Command string
}

// Source values for FileEdit.
const (
	// SourceTranscript is an edit the agent harness reported.
	SourceTranscript = "transcript"
	// SourceObserved is an edit docket saw by reading the file itself.
	SourceObserved = "observed"
)

// When implements Event.
func (e *FileEdit) When() time.Time { return e.At }

// Seq implements Event.
func (e *FileEdit) Seq() int { return e.Sequence }

// TestRun is a recognised test invocation.
type TestRun struct {
	Runner  string
	Outcome string // pass, fail, unknown
	// Confidence records that outcome came from output patterns, because the
	// transcript does not record process exit codes.
	Confidence string
	Summary    string
}

// Command is a shell command run during the session.
type Command struct {
	ID          string
	Sequence    int
	At          time.Time
	Command     string
	Description string
	Stdout      string
	Stderr      string
	Interrupted bool
	// ExitCode is the process's exit status when the harness recorded one.
	// Claude Code does not; Codex and opencode do, and a recorded exit code
	// settles pass or fail without reading tea leaves in the output.
	ExitCode  *int
	Actor     Actor
	SessionID string
	Model     string
	// MayMutateFiles is true for commands that can change the working tree
	// without docket seeing the content. These open a window in which the
	// timeline cannot be trusted, and the timeline marks it.
	MayMutateFiles bool
	MutationHint   string
	Test           *TestRun
}

// When implements Event.
func (c *Command) When() time.Time { return c.At }

// Seq implements Event.
func (c *Command) Seq() int { return c.Sequence }

// ---------------------------------------------------------------- discovery

// ProjectsDir returns the directory Claude Code keeps transcripts in.
func ProjectsDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// slug mirrors the way Claude Code names a project directory: the absolute path
// with every separator replaced by a dash.
func slug(path string) string {
	return strings.ReplaceAll(strings.ReplaceAll(path, string(os.PathSeparator), "-"), ".", "-")
}

// Find locates transcripts for a working directory. It looks in the directory
// Claude Code would have used for that path, then falls back to scanning every
// project directory for sessions whose recorded cwd is inside the tree — which
// is what happens when an agent works on a repository from a parent directory.
func Find(workdir string) ([]string, error) {
	root := ProjectsDir()
	if root == "" {
		return nil, fmt.Errorf("cannot locate Claude Code projects directory")
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	var out []string
	direct := filepath.Join(root, slug(abs))
	if entries, err := os.ReadDir(direct); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".jsonl") {
				out = append(out, filepath.Join(direct, e.Name()))
			}
		}
	}
	// Fallback scan: cheap, because we only read the first line of each file.
	projects, err := os.ReadDir(root)
	if err != nil {
		sort.Strings(out)
		return out, nil
	}
	for _, p := range projects {
		dir := filepath.Join(root, p.Name())
		if dir == direct {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if cwd, ok := peekCWD(full); ok && within(abs, cwd) {
				out = append(out, full)
			}
		}
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

func peekCWD(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	for n := 0; sc.Scan() && n < 40; n++ {
		var rec struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.CWD != "" {
			return rec.CWD, true
		}
	}
	return "", false
}

// maxLine bounds a transcript line. Whole-file writes make these very large, so
// the limit is generous; beyond it the line is skipped and counted as lossy.
const maxLine = 64 << 20

// ---------------------------------------------------------------- parsing

type record struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	ParentUUID  string          `json:"parentUuid"`
	SessionID   string          `json:"sessionId"`
	Timestamp   string          `json:"timestamp"`
	CWD         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	Version     string          `json:"version"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	Message     json.RawMessage `json:"message"`
	ToolResult  json.RawMessage `json:"toolUseResult"`
	Skill       string          `json:"attributionSkill"`
}

type message struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// ParseStats reports how much of a transcript docket could not use. It is
// printed by `docket doctor` so the lossiness is visible rather than implied.
type ParseStats struct {
	Lines           int
	Unparsable      int
	OversizeLines   int
	EditsRecovered  int // patch-only, images missing
	ResultsOrphaned int
}

// Parse reads one transcript file.
func Parse(path string) (*Session, ParseStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ParseStats{}, err
	}
	defer f.Close()

	var stats ParseStats
	s := &Session{Path: path}

	// Pending tool_use blocks, keyed by id, awaiting their result.
	pend := map[string]pending{}

	lastAssistantText := ""
	lastPrompt := ""
	lastTask := "" // subagent task, when a Task tool call is in flight
	seq := 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	for sc.Scan() {
		stats.Lines++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			stats.Unparsable++
			continue
		}
		at := parseTime(rec.Timestamp)
		if s.ID == "" && rec.SessionID != "" {
			s.ID = rec.SessionID
		}
		if rec.CWD != "" {
			s.CWD = rec.CWD
		}
		if rec.GitBranch != "" {
			s.Branch = rec.GitBranch
		}
		if rec.Version != "" {
			s.Version = rec.Version
		}
		if !at.IsZero() {
			if s.Started.IsZero() || at.Before(s.Started) {
				s.Started = at
			}
			if at.After(s.Ended) {
				s.Ended = at
			}
		}

		var msg message
		if len(rec.Message) > 0 {
			_ = json.Unmarshal(rec.Message, &msg)
		}
		if rec.Type == "assistant" && msg.Model != "" && msg.Model != "<synthetic>" {
			s.Model = msg.Model
		}
		blocks := parseBlocks(msg.Content)

		switch rec.Type {
		case "assistant":
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if t := strings.TrimSpace(b.Text); t != "" {
						lastAssistantText = t
					}
				case "tool_use":
					if b.Name == "Task" {
						lastTask = taskOf(b.Input)
					}
					pend[b.ID] = pending{
						name: b.Name, input: b.Input, at: at, model: msg.Model,
						side: rec.IsSidechain, session: rec.SessionID,
						intent: lastAssistantText, task: promptOrTask(rec.IsSidechain, lastPrompt, lastTask),
						skill: rec.Skill,
					}
				}
			}
		case "user":
			// A user record is either a human turn or the transport for a tool
			// result. Only the former is a prompt.
			if len(rec.ToolResult) == 0 && !rec.IsMeta {
				text := textOf(blocks, msg.Content)
				if cmd, ok := humanBashInput(text); ok {
					seq++
					c := &Command{ID: rec.UUID, Sequence: seq, At: at, Command: cmd,
						Description: "run by the human at the keyboard", Actor: ActorHuman, SessionID: rec.SessionID}
					classifyCommand(c)
					s.Commands = append(s.Commands, c)
					s.Events = append(s.Events, c)
				} else if t := humanPrompt(text); t != "" {
					lastPrompt = t
					lastTask = ""
					s.Prompts = append(s.Prompts, Prompt{UUID: rec.UUID, Text: t, At: at})
				}
				continue
			}
			// Tool result.
			id := ""
			for _, b := range blocks {
				if b.Type == "tool_result" && b.ToolUseID != "" {
					id = b.ToolUseID
				}
			}
			p, ok := pend[id]
			if !ok {
				stats.ResultsOrphaned++
				continue
			}
			delete(pend, id)
			switch p.name {
			case "Edit", "Write", "MultiEdit", "NotebookEdit":
				seq++
				e := buildEdit(id, p.name, seq, p, rec.ToolResult)
				if e == nil {
					continue
				}
				if !e.HasPre && !e.Created && len(e.Patch) > 0 {
					stats.EditsRecovered++
				}
				s.Edits = append(s.Edits, e)
				s.Events = append(s.Events, e)
			case "Bash":
				seq++
				c := buildCommand(id, seq, p, rec.ToolResult)
				s.Commands = append(s.Commands, c)
				s.Events = append(s.Events, c)
			}
		}
	}
	if err := sc.Err(); err != nil {
		// A line beyond the buffer cap is lossiness, not a fatal error: the rest
		// of the session is still usable and the stats say what was lost.
		stats.OversizeLines++
	}
	if s.ID == "" {
		s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	s.Agent = AgentClaudeCode
	return s, stats, nil
}

func parseBlocks(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err == nil {
		return bs
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []block{{Type: "text", Text: s}}
	}
	return nil
}

func textOf(blocks []block, raw json.RawMessage) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
			sb.WriteString("\n")
		}
	}
	if sb.Len() == 0 {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return sb.String()
}

var (
	// Wrapped records are the harness talking to itself, not a human request.
	metaWrap    = regexp.MustCompile(`(?s)^\s*<(command-name|command-message|command-args|local-command-stdout|local-command-stderr|bash-stdout|bash-stderr|system-reminder|user-memory-input)>`)
	bashInputRe = regexp.MustCompile(`(?s)<bash-input>(.*?)</bash-input>`)
	interrupted = regexp.MustCompile(`^\s*\[Request interrupted`)
)

func humanPrompt(text string) string {
	t := strings.TrimSpace(text)
	if t == "" || metaWrap.MatchString(t) || interrupted.MatchString(t) {
		return ""
	}
	// Strip system-reminder blocks that ride along with a real prompt.
	t = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`).ReplaceAllString(t, "")
	return strings.TrimSpace(t)
}

func humanBashInput(text string) (string, bool) {
	m := bashInputRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// promptOrTask picks the request an edit descends from: a subagent's edit
// belongs to the task it was handed, everything else to the human's last prompt.
func promptOrTask(sidechain bool, prompt, task string) string {
	if sidechain && task != "" {
		return task
	}
	return prompt
}

func taskOf(input json.RawMessage) string {
	var in struct {
		Description  string `json:"description"`
		Prompt       string `json:"prompt"`
		SubagentType string `json:"subagent_type"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	if in.Description != "" {
		return in.Description
	}
	return firstLine(in.Prompt)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
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
