package transcript

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/diffx"
)

// editResult is the union of the payload shapes the edit tools return. Fields
// absent from a given tool's result stay zero, and the builder below decides
// what can be trusted from what is actually present.
type editResult struct {
	Type         string          `json:"type"` // Write: create | update
	FilePath     string          `json:"filePath"`
	Content      *string         `json:"content"`      // Write: the post-image
	OriginalFile *string         `json:"originalFile"` // Edit/Write: the pre-image
	OldString    *string         `json:"oldString"`
	NewString    *string         `json:"newString"`
	ReplaceAll   bool            `json:"replaceAll"`
	UserModified bool            `json:"userModified"`
	Patch        []patchHunk     `json:"structuredPatch"`
	Edits        json.RawMessage `json:"edits"` // MultiEdit, when echoed back
}

type patchHunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}

type multiEditInput struct {
	FilePath string `json:"file_path"`
	Edits    []struct {
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	} `json:"edits"`
}

type singleEditInput struct {
	FilePath   string  `json:"file_path"`
	OldString  string  `json:"old_string"`
	NewString  string  `json:"new_string"`
	Content    *string `json:"content"`
	ReplaceAll bool    `json:"replace_all"`
}

func buildEdit(id, tool string, seq int, p pending, raw json.RawMessage) *FileEdit {
	var res editResult
	if err := json.Unmarshal(raw, &res); err != nil {
		// A string result means the tool errored: the file was not changed, so
		// there is nothing to attribute.
		return nil
	}
	path := res.FilePath
	if path == "" {
		var in singleEditInput
		if json.Unmarshal(p.input, &in) == nil {
			path = in.FilePath
		}
	}
	if path == "" {
		return nil
	}

	e := &FileEdit{
		ID: id, Tool: tool, Path: path, Sequence: seq, At: p.at,
		Actor: ActorAgent, Model: p.model, SessionID: p.session,
		IsSidechain: p.side, Task: p.task, Intent: p.intent,
		UserModified: res.UserModified, Skill: p.skill,
		AgentID: agentID(p.side, p.task), Source: SourceTranscript,
	}
	e.Patch = convertPatch(res.Patch)

	// preText is kept verbatim: an Edit's old_string is matched against the exact
	// bytes of the file, trailing newline included, so reconstructing the text
	// from split lines would fail to match whole-file replacements.
	preText := ""
	if res.OriginalFile != nil {
		preText = *res.OriginalFile
		e.Pre = diffx.SplitLines(preText)
		e.HasPre = true
	}

	switch tool {
	case "Write":
		if res.Content != nil {
			e.Post = diffx.SplitLines(*res.Content)
			e.HasPost = true
		} else {
			var in singleEditInput
			if json.Unmarshal(p.input, &in) == nil && in.Content != nil {
				e.Post = diffx.SplitLines(*in.Content)
				e.HasPost = true
			}
		}
		// "create" is the harness telling us the file did not exist. That is a
		// known-empty pre-image, not a missing one, and the distinction decides
		// whether the timeline trusts its reconstruction.
		if res.Type == "create" || (res.OriginalFile == nil && res.Type == "") {
			e.Created = res.Type == "create"
			if e.Created {
				e.Pre = nil
				e.HasPre = true
			}
		}
	case "Edit":
		old, new := "", ""
		if res.OldString != nil && res.NewString != nil {
			old, new = *res.OldString, *res.NewString
		} else {
			var in singleEditInput
			if json.Unmarshal(p.input, &in) == nil {
				old, new = in.OldString, in.NewString
				res.ReplaceAll = res.ReplaceAll || in.ReplaceAll
			}
		}
		if e.HasPre && old != "" {
			if post, ok := applyReplace(preText, old, new, res.ReplaceAll); ok {
				e.Post = diffx.SplitLines(post)
				e.HasPost = true
			}
		}
	case "MultiEdit":
		var in multiEditInput
		if json.Unmarshal(p.input, &in) == nil && e.HasPre {
			text := preText
			ok := true
			for _, ed := range in.Edits {
				text, ok = applyReplace(text, ed.OldString, ed.NewString, ed.ReplaceAll)
				if !ok {
					break
				}
			}
			if ok {
				e.Post = diffx.SplitLines(text)
				e.HasPost = true
			}
		}
	case "NotebookEdit":
		// Notebook cells are not line-addressable in the same way; record the
		// edit so the file is known to have been touched, and let the timeline
		// mark its lines unknown rather than pretend to map cells to lines.
	}

	// Last resort: rebuild the post-image from the pre-image and the patch.
	if !e.HasPost && e.HasPre && len(e.Patch) > 0 {
		if post, ok := applyPatch(e.Pre, e.Patch); ok {
			e.Post = post
			e.HasPost = true
		}
	}
	return e
}

// pending is the in-flight tool-call state carried from the assistant record
// that made the call to the user record that carries its result.
type pending struct {
	name    string
	input   json.RawMessage
	at      time.Time
	model   string
	side    bool
	session string
	intent  string
	task    string
	skill   string
}

// agentID names the agent that made an edit. Claude Code does not record which
// subagent definition a sidechain belongs to, so a subagent is identified as
// such without claiming to know which one it was.
func agentID(sidechain bool, task string) string {
	if sidechain {
		return "claude-code/subagent"
	}
	return "claude-code/main"
}

// applyReplace mirrors the Edit tool's own semantics: an exact substring
// replacement, once or everywhere. If the target is not present, the edit cannot
// be reconstructed and ok is false — which is honest lossiness, not a failure.
func applyReplace(text, old, new string, all bool) (string, bool) {
	if old == "" {
		return text, false
	}
	if !strings.Contains(text, old) {
		return text, false
	}
	if all {
		return strings.ReplaceAll(text, old, new), true
	}
	return strings.Replace(text, old, new, 1), true
}

func convertPatch(hunks []patchHunk) []EditOp {
	var out []EditOp
	for _, h := range hunks {
		op := EditOp{OldStart: h.OldStart, OldLines: h.OldLines, NewStart: h.NewStart, NewLines: h.NewLines}
		for _, ln := range h.Lines {
			if ln == "" {
				// An empty entry is an empty context line.
				op.OldText = append(op.OldText, "")
				op.NewText = append(op.NewText, "")
				continue
			}
			switch ln[0] {
			case '+':
				op.NewText = append(op.NewText, ln[1:])
			case '-':
				op.OldText = append(op.OldText, ln[1:])
			case ' ':
				op.OldText = append(op.OldText, ln[1:])
				op.NewText = append(op.NewText, ln[1:])
			case '\\':
				// "\ No newline at end of file"
			default:
				op.OldText = append(op.OldText, ln)
				op.NewText = append(op.NewText, ln)
			}
		}
		out = append(out, op)
	}
	return out
}

// applyPatch rebuilds a post-image from a pre-image and a structured patch. It
// verifies every hunk's old-side text against the pre-image and refuses rather
// than applying a hunk that does not match.
func applyPatch(pre []string, ops []EditOp) ([]string, bool) {
	out := make([]string, 0, len(pre)+16)
	cursor := 0 // 0-based index into pre
	for _, op := range ops {
		start := op.OldStart - 1
		if start < 0 || start > len(pre) || start < cursor {
			return nil, false
		}
		out = append(out, pre[cursor:start]...)
		if start+len(op.OldText) > len(pre) {
			return nil, false
		}
		for i, want := range op.OldText {
			if pre[start+i] != want {
				return nil, false
			}
		}
		out = append(out, op.NewText...)
		cursor = start + len(op.OldText)
	}
	out = append(out, pre[cursor:]...)
	return out, true
}

// ---------------------------------------------------------------- commands

func buildCommand(id string, seq int, p pending, raw json.RawMessage) *Command {
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(p.input, &in)
	var res struct {
		Stdout      string `json:"stdout"`
		Stderr      string `json:"stderr"`
		Interrupted bool   `json:"interrupted"`
	}
	_ = json.Unmarshal(raw, &res)
	c := &Command{
		ID: id, Sequence: seq, At: p.at, Command: in.Command, Description: in.Description,
		Stdout: res.Stdout, Stderr: res.Stderr, Interrupted: res.Interrupted,
		Actor: ActorAgent, SessionID: p.session, Model: p.model,
	}
	classifyCommand(c)
	return c
}

// Runner patterns. Each entry names the runner and how its output reports
// success or failure, because the transcript does not record exit codes.
var runners = []struct {
	name string
	re   *regexp.Regexp
	kind string
	pass *regexp.Regexp
	fail *regexp.Regexp
}{
	{"vitest", regexp.MustCompile(`\bvitest\b`), "test",
		regexp.MustCompile(`(?m)Tests\s+\d+ passed|✓ .*\(\d+ tests?\)|Test Files\s+\d+ passed`),
		regexp.MustCompile(`(?m)Tests\s+\d+ failed|FAIL\b|✗|Test Files\s+\d+ failed`)},
	{"jest", regexp.MustCompile(`\bjest\b`), "test",
		regexp.MustCompile(`(?m)Tests:\s+\d+ passed|PASS\b`),
		regexp.MustCompile(`(?m)Tests:.*\d+ failed|FAIL\b`)},
	{"node-test", regexp.MustCompile(`node\s+--test|node:test`), "test",
		regexp.MustCompile(`(?m)^# pass \d+`), regexp.MustCompile(`(?m)^# fail [1-9]|^not ok `)},
	{"go-test", regexp.MustCompile(`\bgo test\b`), "test",
		regexp.MustCompile(`(?m)^ok\s|^PASS\b`), regexp.MustCompile(`(?m)^FAIL\b|^---\s+FAIL`)},
	{"pytest", regexp.MustCompile(`\bpytest\b|python -m pytest`), "test",
		regexp.MustCompile(`(?m)\d+ passed`), regexp.MustCompile(`(?m)\d+ failed|\d+ error`)},
	{"phpunit", regexp.MustCompile(`\bphpunit\b`), "test",
		regexp.MustCompile(`(?m)^OK \(|Tests:\s+\d+,\s+Assertions`), regexp.MustCompile(`(?m)FAILURES!|ERRORS!|Tests:.*Failures: [1-9]`)},
	{"pest", regexp.MustCompile(`\bpest\b|artisan test`), "test",
		regexp.MustCompile(`(?m)Tests:\s+.*\d+ passed`), regexp.MustCompile(`(?m)Tests:\s+.*\d+ failed|FAILED`)},
	{"cargo-test", regexp.MustCompile(`\bcargo test\b`), "test",
		regexp.MustCompile(`(?m)test result: ok`), regexp.MustCompile(`(?m)test result: FAILED`)},
	{"rspec", regexp.MustCompile(`\brspec\b`), "test",
		regexp.MustCompile(`(?m)\d+ examples?, 0 failures`), regexp.MustCompile(`(?m)\d+ examples?, [1-9]\d* failures?`)},
	{"bun-test", regexp.MustCompile(`\bbun test\b`), "test",
		regexp.MustCompile(`(?m)\d+ pass`), regexp.MustCompile(`(?m)[1-9]\d* fail`)},
	{"npm-test", regexp.MustCompile(`\b(?:npm|pnpm|yarn|bun)\s+(?:run\s+)?test\b`), "test",
		regexp.MustCompile(`(?m)pass(?:ed|ing)?\b`), regexp.MustCompile(`(?m)\bfail(?:ed|ing)?\b|\bERR!`)},
	{"tsc", regexp.MustCompile(`\btsc\b|tsc --noEmit`), "typecheck",
		regexp.MustCompile(`(?m)^\s*$|Found 0 errors`), regexp.MustCompile(`(?m)error TS\d+`)},
	{"eslint", regexp.MustCompile(`\beslint\b`), "static_check",
		regexp.MustCompile(`(?m)^\s*$`), regexp.MustCompile(`(?m)\d+ problems?|error\b`)},
	{"phpstan", regexp.MustCompile(`\bphpstan\b`), "static_check",
		regexp.MustCompile(`(?m)\[OK\] No errors`), regexp.MustCompile(`(?m)\[ERROR\]|Found \d+ error`)},
	{"go-vet", regexp.MustCompile(`\bgo vet\b`), "static_check",
		regexp.MustCompile(`(?m)^\s*$`), regexp.MustCompile(`(?m)\.go:\d+`)},
	{"golangci-lint", regexp.MustCompile(`\bgolangci-lint\b`), "static_check",
		regexp.MustCompile(`(?m)^\s*$`), regexp.MustCompile(`(?m)\.go:\d+`)},
	{"pint", regexp.MustCompile(`\bpint\b`), "static_check",
		regexp.MustCompile(`(?m)PASS`), regexp.MustCompile(`(?m)FAIL`)},
}

// Commands that can change files in ways docket cannot see. Their presence
// between two recorded edits is what turns a confident attribution into
// origin "unknown" — the timeline cannot know what they did.
var mutators = []struct {
	name string
	re   *regexp.Regexp
}{
	{"shell-redirect", regexp.MustCompile(`(^|[^0-9>])>>?\s*[^\s&|]`)},
	{"heredoc", regexp.MustCompile(`<<-?\s*'?[A-Za-z_]`)},
	{"sed-in-place", regexp.MustCompile(`\bsed\b[^|]*\s-i\b|\bperl\b[^|]*\s-i\b`)},
	{"tee", regexp.MustCompile(`\btee\b`)},
	{"move-or-copy", regexp.MustCompile(`\b(?:mv|cp|rsync|install)\s`)},
	{"remove", regexp.MustCompile(`\brm\s`)},
	{"patch-apply", regexp.MustCompile(`\bpatch\s|\bgit\s+apply\b`)},
	{"git-worktree-change", regexp.MustCompile(`\bgit\s+(?:checkout|restore|revert|reset|stash|merge|rebase|cherry-pick|clean)\b`)},
	{"formatter", regexp.MustCompile(`\b(?:prettier|gofmt|goimports|black|ruff\s+format|php-cs-fixer|pint|rustfmt|biome)\b`)},
	{"codegen", regexp.MustCompile(`\b(?:artisan\s+make|go\s+generate|npx\s+\w+\s+init|composer\s+(?:install|update|require)|npm\s+(?:install|i|ci)|pnpm\s+(?:install|add)|yarn\s+add)\b`)},
	{"truncate", regexp.MustCompile(`\b(?:truncate|dd)\s`)},
	{"interpreter-script", regexp.MustCompile(`\b(?:python3?|node|ruby|php)\s+-\b|\b(?:python3?|node)\s+<<`)},
}

func classifyCommand(c *Command) {
	out := c.Stdout + "\n" + c.Stderr
	for _, r := range runners {
		if !r.re.MatchString(c.Command) {
			continue
		}
		t := &TestRun{Runner: r.name, Outcome: "unknown", Confidence: "output_pattern"}
		switch {
		case c.Interrupted:
			t.Outcome = "unknown"
			t.Summary = "interrupted before completion"
		case r.fail.MatchString(out):
			t.Outcome = "fail"
		case r.pass.MatchString(out):
			t.Outcome = "pass"
		}
		t.Summary = strings.TrimSpace(t.Summary)
		c.Test = t
		c.MutationHint = r.kind
		// A test run is not a mutation, but a formatter masquerading as a check
		// is, so fall through to the mutator scan below.
		break
	}
	for _, m := range mutators {
		if m.re.MatchString(c.Command) {
			c.MayMutateFiles = true
			if c.Test == nil {
				c.MutationHint = m.name
			} else {
				c.MutationHint = c.MutationHint + "+" + m.name
			}
			return
		}
	}
}

// Kind reports what sort of check a command was, if any.
func (c *Command) Kind() string {
	if c.Test == nil {
		return ""
	}
	switch c.Test.Runner {
	case "tsc":
		return "typecheck"
	case "eslint", "phpstan", "go-vet", "golangci-lint", "pint":
		return "static_check"
	default:
		return "test"
	}
}
