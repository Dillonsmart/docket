// Package collect is docket's own observer.
//
// Reading the agent transcript recovers edits made with the Edit and Write
// tools, but an agent that writes files through the shell — a heredoc, sed -i,
// a code generator, a formatter — leaves no recoverable pre- and post-image
// there at all. Measured on real sessions, that is the difference between 96%
// and 59% of hunks explained.
//
// So docket watches the working tree itself. Before a command runs it takes a
// content snapshot; after the command it diffs the snapshot and records what
// changed, with both images stored as git blobs. Those events are observed
// rather than reported, which makes them the strongest evidence docket can
// gather without a CI runner.
package collect

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/diffx"
	"github.com/Dillonsmart/docket/internal/gitx"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// Limits keep the hook fast enough to run on every tool call. A snapshot that
// cost a second would get uninstalled within a day.
const (
	// MaxFileSize is the largest file docket will hash and store. Bigger files
	// are tracked by stat only, and a change to one is reported as an untracked
	// mutation rather than silently ignored.
	MaxFileSize = 2 << 20
	// MaxFiles bounds a snapshot.
	MaxFiles = 20000
)

// Entry is one file in a snapshot.
type Entry struct {
	Size    int64  `json:"size"`
	ModNano int64  `json:"mtime"`
	Digest  string `json:"sha256,omitempty"`
	Blob    string `json:"blob,omitempty"`
	TooBig  bool   `json:"too_big,omitempty"`
}

// Snapshot is the state of the working tree at a point in time.
type Snapshot struct {
	Version int              `json:"version"`
	Taken   time.Time        `json:"taken"`
	Session string           `json:"session,omitempty"`
	Files   map[string]Entry `json:"files"`
}

// Event is a recorded observation, appended to a session log as JSON lines.
type Event struct {
	Kind      string    `json:"kind"` // file_edit | file_delete
	At        time.Time `json:"at"`
	Session   string    `json:"session,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	Command   string    `json:"command,omitempty"`
	Path      string    `json:"path"`
	PreBlob   string    `json:"pre_blob,omitempty"`
	PostBlob  string    `json:"post_blob,omitempty"`
	Created   bool      `json:"created,omitempty"`
	Deleted   bool      `json:"deleted,omitempty"`
	Oversize  bool      `json:"oversize,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	ToolUseID string    `json:"tool_use_id,omitempty"`
}

// Store is the on-disk collector state for one repository.
type Store struct {
	repo *gitx.Repo
	dir  string
}

// Open returns the collector store, creating its directory.
func Open(repo *gitx.Repo) (*Store, error) {
	dir := filepath.Join(repo.StateDir(), "collect")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{repo: repo, dir: dir}, nil
}

func (s *Store) snapshotPath() string { return filepath.Join(s.dir, "snapshot.json") }

func (s *Store) eventsPath(session string) string {
	if session == "" {
		session = "unknown"
	}
	return filepath.Join(s.dir, "events-"+safeName(session)+".jsonl")
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// candidates lists the files docket will watch: tracked files plus untracked
// files git would not ignore. Ignored files are out of scope by definition —
// they cannot appear in a commit.
func (s *Store) candidates() ([]string, error) {
	out, err := s.repo.Git("ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	files := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		files = append(files, p)
		if len(files) >= MaxFiles {
			break
		}
	}
	sort.Strings(files)
	return files, nil
}

// Take builds a snapshot, reusing digests from prev for files whose size and
// mtime are unchanged. That reuse is what keeps the hook in the milliseconds.
func (s *Store) Take(session string, prev *Snapshot) (*Snapshot, error) {
	files, err := s.candidates()
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Version: 1, Taken: time.Now().UTC(), Session: session, Files: make(map[string]Entry, len(files))}
	for _, rel := range files {
		abs := filepath.Join(s.repo.Root, rel)
		st, err := os.Lstat(abs)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		e := Entry{Size: st.Size(), ModNano: st.ModTime().UnixNano()}
		if st.Size() > MaxFileSize {
			e.TooBig = true
			snap.Files[rel] = e
			continue
		}
		if prev != nil {
			if old, ok := prev.Files[rel]; ok && old.Size == e.Size && old.ModNano == e.ModNano && old.Digest != "" {
				e.Digest, e.Blob = old.Digest, old.Blob
				snap.Files[rel] = e
				continue
			}
		}
		digest, blob, err := s.store(abs)
		if err != nil {
			continue
		}
		e.Digest, e.Blob = digest, blob
		snap.Files[rel] = e
	}
	return snap, nil
}

// store hashes a file and writes it into the repository's object database, which
// gives docket content-addressed access to every pre- and post-image without a
// store of its own.
func (s *Store) store(abs string) (digest, blob string, err error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	oid, err := s.repo.WriteBlob(data)
	if err != nil {
		return hex.EncodeToString(sum[:]), "", nil
	}
	return hex.EncodeToString(sum[:]), oid, nil
}

// Load reads the stored snapshot, returning nil when there is none.
func (s *Store) Load() *Snapshot {
	data, err := os.ReadFile(s.snapshotPath())
	if err != nil {
		return nil
	}
	var snap Snapshot
	if json.Unmarshal(data, &snap) != nil {
		return nil
	}
	if snap.Files == nil {
		snap.Files = map[string]Entry{}
	}
	return &snap
}

// Save writes the snapshot atomically: a half-written snapshot would make the
// next diff wrong, and a wrong diff is a wrong attribution.
func (s *Store) Save(snap *Snapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp := s.snapshotPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.snapshotPath())
}

// Before records the state of the tree ahead of a command.
func (s *Store) Before(session string) error {
	snap, err := s.Take(session, s.Load())
	if err != nil {
		return err
	}
	return s.Save(snap)
}

// After diffs the tree against the stored snapshot, appends an event per changed
// file and re-saves the snapshot.
func (s *Store) After(meta EventMeta) ([]Event, error) {
	prev := s.Load()
	next, err := s.Take(meta.Session, prev)
	if err != nil {
		return nil, err
	}
	events := diffSnapshots(prev, next, meta)
	if len(events) > 0 {
		if err := s.append(meta.Session, events); err != nil {
			return nil, err
		}
	}
	return events, s.Save(next)
}

// EventMeta is what the caller knows about the command being observed.
type EventMeta struct {
	Session   string
	Tool      string
	Command   string
	Actor     string
	ToolUseID string
	At        time.Time
}

func diffSnapshots(prev, next *Snapshot, meta EventMeta) []Event {
	at := meta.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var events []Event
	if prev == nil {
		// With no baseline there is nothing to compare against. Recording
		// everything as created would be a lie: the files existed already.
		return nil
	}
	paths := map[string]bool{}
	for p := range prev.Files {
		paths[p] = true
	}
	for p := range next.Files {
		paths[p] = true
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	for _, p := range ordered {
		before, hadBefore := prev.Files[p]
		after, hasAfter := next.Files[p]
		switch {
		case hadBefore && !hasAfter:
			events = append(events, Event{
				Kind: "file_delete", At: at, Session: meta.Session, Tool: meta.Tool, Command: meta.Command,
				Path: p, PreBlob: before.Blob, Deleted: true, Actor: meta.Actor, ToolUseID: meta.ToolUseID,
			})
		case !hadBefore && hasAfter:
			events = append(events, Event{
				Kind: "file_edit", At: at, Session: meta.Session, Tool: meta.Tool, Command: meta.Command,
				Path: p, PostBlob: after.Blob, Created: true, Oversize: after.TooBig,
				Actor: meta.Actor, ToolUseID: meta.ToolUseID,
			})
		case hadBefore && hasAfter && (before.Digest != after.Digest || before.TooBig || after.TooBig):
			if before.TooBig || after.TooBig {
				events = append(events, Event{
					Kind: "file_edit", At: at, Session: meta.Session, Tool: meta.Tool, Command: meta.Command,
					Path: p, Oversize: true, Actor: meta.Actor, ToolUseID: meta.ToolUseID,
				})
				continue
			}
			events = append(events, Event{
				Kind: "file_edit", At: at, Session: meta.Session, Tool: meta.Tool, Command: meta.Command,
				Path: p, PreBlob: before.Blob, PostBlob: after.Blob,
				Actor: meta.Actor, ToolUseID: meta.ToolUseID,
			})
		}
	}
	return events
}

func (s *Store) append(session string, events []Event) error {
	f, err := os.OpenFile(s.eventsPath(session), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- replay

// Sessions reads every collected event log and returns them as transcript
// sessions, so the timeline can replay observed and reported edits together in
// one ordered stream.
func (s *Store) Sessions() ([]*transcript.Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*transcript.Session
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "events-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		sess, err := s.readLog(filepath.Join(s.dir, name))
		if err != nil || sess == nil {
			continue
		}
		out = append(out, sess)
	}
	return out, nil
}

func (s *Store) readLog(path string) (*transcript.Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sess := &transcript.Session{Path: path, CWD: s.repo.Root}
	seq := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 4<<20)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if sess.ID == "" {
			sess.ID = ev.Session
		}
		seq++
		edit := s.toEdit(ev, seq)
		if edit == nil {
			continue
		}
		sess.Edits = append(sess.Edits, edit)
		sess.Events = append(sess.Events, edit)
		if sess.Started.IsZero() || ev.At.Before(sess.Started) {
			sess.Started = ev.At
		}
		if ev.At.After(sess.Ended) {
			sess.Ended = ev.At
		}
	}
	if sess.ID == "" {
		sess.ID = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "events-"), ".jsonl")
	}
	return sess, nil
}

func (s *Store) toEdit(ev Event, seq int) *transcript.FileEdit {
	if ev.Kind == "file_delete" {
		// A deletion contributes no lines to a commit, and recording it as an
		// edit with an empty post-image would let it swallow provenance.
		return nil
	}
	abs := filepath.Join(s.repo.Root, ev.Path)
	e := &transcript.FileEdit{
		ID:   "observed:" + shortBlob(ev.PostBlob) + ":" + fmt.Sprint(seq),
		Tool: firstNonEmpty(ev.Tool, "Bash"), Path: abs, Sequence: seq, At: ev.At,
		Actor: actorOf(ev.Actor), AgentID: agentOf(ev.Actor), SessionID: ev.Session,
		Source: transcript.SourceObserved, Command: ev.Command, Created: ev.Created,
	}
	if ev.Oversize {
		// The content was never read, so nothing about its lines can be claimed.
		return e
	}
	if ev.PreBlob != "" {
		if data, ok := s.blob(ev.PreBlob); ok {
			e.Pre = diffx.SplitLines(string(data))
			e.HasPre = true
		}
	} else if ev.Created {
		e.HasPre = true // known empty: the file did not exist
	}
	if ev.PostBlob != "" {
		if data, ok := s.blob(ev.PostBlob); ok {
			e.Post = diffx.SplitLines(string(data))
			e.HasPost = true
		}
	}
	return e
}

func (s *Store) blob(oid string) ([]byte, bool) {
	out, err := s.repo.GitBytes("cat-file", "blob", oid)
	if err != nil {
		return nil, false
	}
	return out, true
}

func actorOf(a string) transcript.Actor {
	switch a {
	case "human":
		return transcript.ActorHuman
	case "agent", "":
		return transcript.ActorAgent
	default:
		return transcript.ActorUnknown
	}
}

func agentOf(a string) string {
	if a == "human" {
		return ""
	}
	return "claude-code/main"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func shortBlob(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	if oid == "" {
		return "none"
	}
	return oid
}

// HookInput is the JSON a Claude Code hook receives on stdin. Every field is
// optional: docket must keep working if the harness adds or renames one.
type HookInput struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// ReadHookInput parses hook JSON, tolerating an empty or malformed body.
func ReadHookInput(r io.Reader) HookInput {
	var in HookInput
	data, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil || len(data) == 0 {
		return in
	}
	_ = json.Unmarshal(data, &in)
	return in
}

// Command extracts the shell command from a Bash tool input.
func (h HookInput) Command() string {
	if len(h.ToolInput) == 0 {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(h.ToolInput, &in) != nil {
		return ""
	}
	return in.Command
}
