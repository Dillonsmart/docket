// Package gitx is the only place in docket that shells out to git.
//
// Everything here uses plumbing rather than porcelain so that output is stable
// across git versions and unaffected by the user's config, aliases or pagers.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a handle on a git working tree.
type Repo struct {
	Root string
	// GitDir is the resolved .git directory (which is not always Root/.git:
	// worktrees and submodules point elsewhere).
	GitDir string
}

// ErrNotARepo is returned by Open when dir is not inside a working tree.
var ErrNotARepo = errors.New("not a git repository")

// Open resolves the repository containing dir.
func Open(dir string) (*Repo, error) {
	root, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, ErrNotARepo
	}
	gitdir, err := run(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, ErrNotARepo
	}
	return &Repo{Root: strings.TrimSpace(root), GitDir: strings.TrimSpace(gitdir)}, nil
}

// Git runs a git command in the repository and returns stdout.
func (r *Repo) Git(args ...string) (string, error) { return run(r.Root, args...) }

// GitBytes is Git for commands whose output is not text (blob contents).
func (r *Repo) GitBytes(args ...string) ([]byte, error) { return runBytes(r.Root, nil, args...) }

// GitStdin runs a git command with stdin attached.
func (r *Repo) GitStdin(stdin []byte, args ...string) (string, error) {
	out, err := runBytes(r.Root, stdin, args...)
	return strings.TrimSpace(string(out)), err
}

func run(dir string, args ...string) (string, error) {
	out, err := runBytes(dir, nil, args...)
	return strings.TrimSpace(string(out)), err
}

func runBytes(dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Keep git non-interactive: a hook must never block a commit on a prompt.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ---------------------------------------------------------------- reading

// RevParse resolves a revision to a full object id.
func (r *Repo) RevParse(rev string) (string, error) { return r.Git("rev-parse", "--verify", rev) }

// HasRev reports whether a revision exists.
func (r *Repo) HasRev(rev string) bool {
	_, err := r.RevParse(rev)
	return err == nil
}

// FileAt returns the contents of path at rev. ok is false when the path does
// not exist at that revision (a file added by the commit, typically).
func (r *Repo) FileAt(rev, path string) (data []byte, ok bool) {
	out, err := r.GitBytes("show", rev+":"+path)
	if err != nil {
		return nil, false
	}
	return out, true
}

// WorktreeFile reads path from the working tree.
func (r *Repo) WorktreeFile(path string) (data []byte, ok bool) {
	out, err := os.ReadFile(filepath.Join(r.Root, path))
	if err != nil {
		return nil, false
	}
	return out, true
}

// CommitMeta is the subset of commit metadata a docket records.
type CommitMeta struct {
	SHA       string
	Parent    string
	Author    string
	Email     string
	When      string
	Subject   string
	Body      string
	Committer string
}

// Commit reads metadata for a revision.
func (r *Repo) Commit(rev string) (CommitMeta, error) {
	const sep = "\x1f"
	out, err := r.Git("show", "-s", "--format=%H"+sep+"%P"+sep+"%an"+sep+"%ae"+sep+"%aI"+sep+"%s"+sep+"%cn"+sep+"%b", rev)
	if err != nil {
		return CommitMeta{}, err
	}
	f := strings.SplitN(out, sep, 8)
	for len(f) < 8 {
		f = append(f, "")
	}
	parents := strings.Fields(f[1])
	first := ""
	if len(parents) > 0 {
		first = parents[0]
	}
	return CommitMeta{SHA: f[0], Parent: first, Author: f[2], Email: f[3], When: f[4], Subject: f[5], Committer: f[6], Body: f[7]}, nil
}

// Trailer returns the value of the last trailer with the given key on rev.
func (r *Repo) Trailer(rev, key string) (string, bool) {
	out, err := r.Git("show", "-s", "--format=%B", rev)
	if err != nil {
		return "", false
	}
	return TrailerIn(out, key)
}

// TrailerIn extracts a trailer from a raw commit message.
func TrailerIn(msg, key string) (string, bool) {
	lines := strings.Split(msg, "\n")
	prefix := strings.ToLower(key) + ":"
	val, found := "", false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(strings.ToLower(t), prefix) {
			val = strings.TrimSpace(t[len(prefix):])
			found = true // last one wins
		}
	}
	return val, found
}

// ---------------------------------------------------------------- diffing

// DiffOpts selects what to diff.
type DiffOpts struct {
	Base   string // empty for --cached
	Head   string // empty for the index/worktree
	Staged bool
	Paths  []string
}

// Diff returns a unified diff with zero context lines, which keeps hunk
// boundaries tight: context lines would let one hunk swallow unrelated code and
// blur the evidence boundary.
func (r *Repo) Diff(o DiffOpts) (string, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff", "--unified=0", "--no-renames", "--irreversible-delete"}
	if o.Staged {
		args = append(args, "--cached")
		if o.Base != "" {
			args = append(args, o.Base)
		}
	} else {
		if o.Base == "" {
			return "", errors.New("diff: base required")
		}
		args = append(args, o.Base)
		if o.Head != "" {
			args = append(args, o.Head)
		}
	}
	if len(o.Paths) > 0 {
		args = append(args, "--")
		args = append(args, o.Paths...)
	}
	out, err := r.GitBytes(args...)
	return string(out), err
}

// EmptyTree is the well-known object id of the empty tree, used as the base
// when a commit has no parent.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// BaseOf returns the revision a commit should be diffed against.
func (r *Repo) BaseOf(rev string) (string, error) {
	c, err := r.Commit(rev)
	if err != nil {
		return "", err
	}
	if c.Parent == "" {
		return EmptyTree, nil
	}
	return c.Parent, nil
}

// ---------------------------------------------------------------- writing

// WriteBlob stores data as a loose object and returns its object id.
func (r *Repo) WriteBlob(data []byte) (string, error) {
	return r.GitStdin(data, "hash-object", "-w", "-t", "blob", "--stdin")
}

// TreeEntry is one file in a tree being built.
type TreeEntry struct {
	Path string // path within the tree, forward slashes
	Blob string // object id
	Mode string // defaults to 100644
}

// CommitRecords adds entries to the tree of ref (creating it if absent) and
// commits the result on that ref, returning the new commit id.
//
// This is what makes refs/docket/* an orphan history: the ref never shares a
// commit with the code branches, so the records travel with the repository
// without touching its DAG.
func (r *Repo) CommitRecords(ref, message string, entries []TreeEntry) (string, error) {
	idx, err := os.CreateTemp("", "docket-index-")
	if err != nil {
		return "", err
	}
	idxPath := idx.Name()
	idx.Close()
	os.Remove(idxPath) // git wants to create it itself
	defer os.Remove(idxPath)

	withIndex := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.Root
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+idxPath, "GIT_TERMINAL_PROMPT=0")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}

	parent, hasParent := "", false
	if p, err := r.RevParse(ref); err == nil {
		parent, hasParent = p, true
		if _, err := withIndex("read-tree", ref); err != nil {
			return "", err
		}
	}
	for _, e := range entries {
		mode := e.Mode
		if mode == "" {
			mode = "100644"
		}
		if _, err := withIndex("update-index", "--add", "--cacheinfo", mode+","+e.Blob+","+e.Path); err != nil {
			return "", err
		}
	}
	tree, err := withIndex("write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree, "-m", message}
	if hasParent {
		args = append(args, "-p", parent)
	}
	commit, err := r.Git(args...)
	if err != nil {
		return "", err
	}
	updateArgs := []string{"update-ref", ref, commit}
	if hasParent {
		updateArgs = append(updateArgs, parent)
	}
	if _, err := r.Git(updateArgs...); err != nil {
		return "", err
	}
	return commit, nil
}

// ReadRecord reads a path out of the tree of ref.
func (r *Repo) ReadRecord(ref, path string) ([]byte, bool) {
	out, err := r.GitBytes("cat-file", "-p", ref+":"+path)
	if err != nil {
		return nil, false
	}
	return out, true
}

// ListRecords lists the paths held in the tree of ref.
func (r *Repo) ListRecords(ref string) ([]string, error) {
	out, err := r.Git("ls-tree", "-r", "--name-only", ref)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// ---------------------------------------------------------------- config

// ConfigGet reads a config key, returning ok=false when unset.
func (r *Repo) ConfigGet(key string) (string, bool) {
	v, err := r.Git("config", "--get", key)
	if err != nil {
		return "", false
	}
	return v, true
}

// ConfigGetAll reads every value of a multi-valued config key.
func (r *Repo) ConfigGetAll(key string) []string {
	v, err := r.Git("config", "--get-all", key)
	if err != nil || v == "" {
		return nil
	}
	return strings.Split(v, "\n")
}

// ConfigSet sets a local config key.
func (r *Repo) ConfigSet(key, value string) error {
	_, err := r.Git("config", key, value)
	return err
}

// ConfigAdd appends to a multi-valued local config key.
func (r *Repo) ConfigAdd(key, value string) error {
	_, err := r.Git("config", "--add", key, value)
	return err
}

// HooksDir resolves where hooks live, honouring core.hooksPath.
func (r *Repo) HooksDir() string {
	if p, ok := r.ConfigGet("core.hooksPath"); ok && p != "" {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(r.Root, p)
	}
	return filepath.Join(r.GitDir, "hooks")
}

// StateDir is where docket keeps local, never-pushed state: the signing key and
// pending records handed from prepare-commit-msg to post-commit.
func (r *Repo) StateDir() string { return filepath.Join(r.GitDir, "docket") }
