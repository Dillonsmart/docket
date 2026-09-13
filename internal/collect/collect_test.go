package collect

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dillonsmart/docket/internal/gitx"
	"github.com/Dillonsmart/docket/internal/transcript"
)

func tempRepo(t *testing.T) *gitx.Repo {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func write(t *testing.T, repo *gitx.Repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo.Root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestObservesFileChangesAroundACommand(t *testing.T) {
	repo := tempRepo(t)
	write(t, repo, "a.txt", "one\ntwo\n")
	store, err := Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Before("s1"); err != nil {
		t.Fatal(err)
	}

	// The agent runs a shell command that rewrites the file and creates another.
	write(t, repo, "a.txt", "one\nTWO\n")
	write(t, repo, "b.txt", "new\n")

	events, err := store.After(EventMeta{Session: "s1", Tool: "Bash", Command: "sed -i '' s/two/TWO/ a.txt", Actor: "agent", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	byPath := map[string]Event{}
	for _, e := range events {
		byPath[e.Path] = e
	}
	if a := byPath["a.txt"]; a.PreBlob == "" || a.PostBlob == "" || a.Created {
		t.Errorf("a.txt event = %+v, want both images and not a creation", a)
	}
	if b := byPath["b.txt"]; !b.Created || b.PostBlob == "" {
		t.Errorf("b.txt event = %+v, want a creation", b)
	}

	// Replayed back, the event must carry the real content on both sides: that
	// is what makes a shell-written line attributable.
	sessions, err := store.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	var edit *transcript.FileEdit
	for _, s := range sessions {
		for _, e := range s.Edits {
			if strings.HasSuffix(e.Path, "a.txt") {
				edit = e
			}
		}
	}
	if edit == nil {
		t.Fatal("no replayed edit for a.txt")
	}
	if !edit.HasPre || !edit.HasPost {
		t.Fatalf("replayed edit is missing images: %+v", edit)
	}
	if strings.Join(edit.Pre, "|") != "one|two" || strings.Join(edit.Post, "|") != "one|TWO" {
		t.Errorf("images = %v → %v", edit.Pre, edit.Post)
	}
	if edit.Source != transcript.SourceObserved || edit.Command == "" {
		t.Errorf("provenance of an observed edit = %+v", edit)
	}
}

func TestIgnoredFilesAreNotWatched(t *testing.T) {
	repo := tempRepo(t)
	write(t, repo, ".gitignore", "secrets/\n")
	write(t, repo, "secrets/token.txt", "sk_live_not_a_real_key\n")
	store, err := Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Take("s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := snap.Files["secrets/token.txt"]; found {
		t.Error("an ignored file was snapshotted; it can never appear in a commit")
	}
}

// Without a baseline there is nothing to compare against, and reporting every
// file as newly created would be a lie.
func TestNoBaselineProducesNoEvents(t *testing.T) {
	repo := tempRepo(t)
	write(t, repo, "a.txt", "one\n")
	store, err := Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.After(EventMeta{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events with no baseline", len(events))
	}
}

func TestHookInputParsing(t *testing.T) {
	in := ReadHookInput(strings.NewReader(`{"session_id":"abc","cwd":"/repo","tool_name":"Bash","tool_input":{"command":"ls -la"}}`))
	if in.SessionID != "abc" || in.CWD != "/repo" || in.Command() != "ls -la" {
		t.Errorf("parsed = %+v, command %q", in, in.Command())
	}
	// A malformed body must not stop the agent.
	if got := ReadHookInput(strings.NewReader("not json")); got.SessionID != "" {
		t.Errorf("malformed input = %+v", got)
	}
}
