package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests drive the real command line against a real repository. The whole
// product is a chain — observe, attribute, record, store, verify — and unit
// tests on the links would not catch a break between them.

type harness struct {
	t   *testing.T
	dir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	// Keep the test away from the developer's own transcripts.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude-config"))
	t.Setenv("NO_COLOR", "1")
	isolatePATH(t, dir)
	h := &harness{t: t, dir: dir}
	h.git("init", "-q")
	h.git("config", "user.email", "test@example.com")
	h.git("config", "user.name", "Test")
	t.Chdir(dir)
	return h
}

// isolatePATH gives the test a PATH with git on it and nothing else.
//
// `docket init` installs hooks that fall back to whatever docket is on PATH, so
// a developer with docket installed would have their own binary — possibly an
// older one — running inside these tests. The tests drive the hooks through
// cli.Main directly and must not race with an installed copy.
func isolatePATH(t *testing.T, dir string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required for these tests: %v", err)
	}
	bin := filepath.Join(dir, "isolated-bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func (h *harness) git(args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = h.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (h *harness) run(stdin string, args ...string) (string, int) {
	h.t.Helper()
	var out bytes.Buffer
	code := Main(args, strings.NewReader(stdin), &out, &out)
	return out.String(), code
}

func (h *harness) write(rel, body string) {
	h.t.Helper()
	path := filepath.Join(h.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) hookInput(command string) string {
	h.t.Helper()
	data, err := json.Marshal(map[string]any{
		"session_id": "sess-test", "cwd": h.dir, "tool_name": "Bash",
		"tool_input": map[string]string{"command": command},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

// commit runs the two git hooks around a commit the way git would, which is
// where the trailer and the stored record come from.
func (h *harness) commit(message string) string {
	h.t.Helper()
	h.git("add", "-A")
	msgPath := filepath.Join(h.dir, ".git", "COMMIT_EDITMSG")
	if err := os.WriteFile(msgPath, []byte(message+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	if out, code := h.run("", "hook", "prepare-commit-msg", msgPath); code != 0 {
		h.t.Fatalf("prepare-commit-msg exited %d: %s", code, out)
	}
	h.git("commit", "-q", "--no-verify", "-F", msgPath)
	if out, code := h.run("", "hook", "post-commit"); code != 0 {
		h.t.Fatalf("post-commit exited %d: %s", code, out)
	}
	return h.git("rev-parse", "HEAD")
}

func TestEndToEndObservedEditBecomesAVerifiedRecord(t *testing.T) {
	h := newHarness(t)

	if out, code := h.run("", "init"); code != 0 {
		t.Fatalf("init exited %d: %s", code, out)
	}

	// An agent writes a file through the shell, which the collector observes.
	input := h.hookInput("cat > src/auth.js <<'EOF' ...")
	if _, code := h.run(input, "collect", "pre"); code != 0 {
		t.Fatal("collect pre failed")
	}
	h.write("src/auth.js", "export function newSession(user) {\n  return { id: mintId(), user };\n}\n")
	if _, code := h.run(input, "collect", "post"); code != 0 {
		t.Fatal("collect post failed")
	}

	sha := h.commit("feat: session helpers")

	body := h.git("show", "-s", "--format=%B", sha)
	if !strings.Contains(body, "Docket: sha256:") {
		t.Fatalf("commit has no docket trailer:\n%s", body)
	}

	show, code := h.run("", "show", sha, "--all")
	if code != 0 {
		t.Fatalf("show exited %d: %s", code, show)
	}
	if !strings.Contains(show, "src/auth.js") {
		t.Errorf("show does not mention the file:\n%s", show)
	}
	if !strings.Contains(show, "docket read the file before and after") {
		t.Errorf("the shell edit was not recorded as observed:\n%s", show)
	}
	if !strings.Contains(show, "digest and signature check out") {
		t.Errorf("record did not verify in show:\n%s", show)
	}

	if out, code := h.run("", "verify", sha); code != 0 {
		t.Fatalf("verify exited %d: %s", code, out)
	}

	md, code := h.run("", "review", sha, "--format", "md")
	if code != 0 {
		t.Fatalf("review exited %d: %s", code, md)
	}
	if !strings.Contains(md, "docket:evidence") || !strings.Contains(md, "src/auth.js") {
		t.Errorf("markdown review looks wrong:\n%s", md)
	}
}

// A commit with no evidence behind it must be reported as such, not smoothed
// over: that is the number the whole product is about.
func TestUnexplainedCodeIsReportedAsUnknown(t *testing.T) {
	h := newHarness(t)
	if _, code := h.run("", "init"); code != 0 {
		t.Fatal("init failed")
	}
	// No collector hooks fire: this is a file that simply appeared.
	h.write("vendor/lib.js", "module.exports = 1;\n")
	sha := h.commit("chore: vendored library")

	out, code := h.run("", "show", sha, "--all")
	if code != 0 {
		t.Fatalf("show exited %d: %s", code, out)
	}
	if !strings.Contains(out, "unknown — nothing recorded accounts for these lines") {
		t.Errorf("unattributed code was not reported as unknown:\n%s", out)
	}
	if !strings.Contains(out, "no verification evidence at all") {
		t.Errorf("zero-evidence summary missing:\n%s", out)
	}
}

func TestReviewFailsUnderAThreshold(t *testing.T) {
	h := newHarness(t)
	if _, code := h.run("", "init"); code != 0 {
		t.Fatal("init failed")
	}
	h.write("src/x.js", "export const x = 1;\n")
	sha := h.commit("feat: x")

	_, code := h.run("", "review", sha, "--fail-under", "0.5")
	if code != 3 {
		t.Errorf("review exit code = %d, want 3 for a policy failure", code)
	}
	_, code = h.run("", "review", sha, "--fail-under", "0")
	if code != 0 {
		t.Errorf("review exit code = %d with no threshold, want 0", code)
	}
}

// A commit made with the hooks disabled has no record. Silence is the honest
// answer; inventing one after the fact would defeat the point.
//
// Note that --no-verify is not how you get here: git bypasses pre-commit and
// commit-msg with that flag, but still runs prepare-commit-msg.
func TestCommitWithoutTheHookHasNoRecord(t *testing.T) {
	h := newHarness(t)
	if _, code := h.run("", "init"); code != 0 {
		t.Fatal("init failed")
	}
	h.write("src/y.js", "export const y = 2;\n")
	h.git("add", "-A")
	h.git("-c", "core.hooksPath="+filepath.Join(h.dir, "no-hooks"), "commit", "-q", "-m", "feat: y")
	if _, code := h.run("", "hook", "post-commit"); code != 0 {
		t.Fatal("post-commit should not fail on an untracked commit")
	}
	out, code := h.run("", "verify")
	if code == 0 {
		t.Errorf("verify should fail when there is no record:\n%s", out)
	}
}

func TestInitIsIdempotentAndDoesNotClobberHooks(t *testing.T) {
	h := newHarness(t)
	hookPath := filepath.Join(h.dir, ".git", "hooks", "prepare-commit-msg")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho mine\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("", "init")
	if code != 0 {
		t.Fatalf("init exited %d: %s", code, out)
	}
	existing, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(existing) != "#!/bin/sh\necho mine\n" {
		t.Errorf("init overwrote an existing hook:\n%s", existing)
	}
	if _, err := os.Stat(hookPath + ".docket"); err != nil {
		t.Error("init should have written the hook alongside for the user to wire in")
	}

	// Running init again must not duplicate the agent hooks.
	if _, code := h.run("", "init"); code != 0 {
		t.Fatal("second init failed")
	}
	settings, err := os.ReadFile(filepath.Join(h.dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(settings), "collect pre") != 1 {
		t.Errorf("agent hooks were duplicated:\n%s", settings)
	}
}

func TestDoctorReportsWhatIsMissing(t *testing.T) {
	h := newHarness(t)
	out, code := h.run("", "doctor")
	if code != 0 {
		t.Fatalf("doctor exited %d: %s", code, out)
	}
	if !strings.Contains(out, "MISSING") {
		t.Errorf("doctor should report missing hooks before init:\n%s", out)
	}
	if _, code := h.run("", "init"); code != 0 {
		t.Fatal("init failed")
	}
	out, _ = h.run("", "doctor")
	if strings.Contains(out, "prepare-commit-msg MISSING") {
		t.Errorf("doctor still reports missing hooks after init:\n%s", out)
	}
}

// Amending rewrites the commit, so the record has to be rebuilt and the trailer
// replaced. Leaving the old one would point the commit at a record of content
// it no longer has.
func TestAmendReplacesTheTrailerAndTheRecord(t *testing.T) {
	h := newHarness(t)
	if _, code := h.run("", "init"); code != 0 {
		t.Fatal("init failed")
	}
	h.write("src/a.js", "export const a = 1;\n")
	sha := h.commit("feat: a")
	first, _ := trailerOf(t, h, sha)

	// Amend with more content than the original commit had.
	h.write("src/a.js", "export const a = 1;\nexport const b = 2;\n")
	h.git("add", "-A")
	msgPath := filepath.Join(h.dir, ".git", "COMMIT_EDITMSG")
	if out, code := h.run("", "hook", "prepare-commit-msg", msgPath, "commit", sha); code != 0 {
		t.Fatalf("prepare-commit-msg exited %d: %s", code, out)
	}
	h.git("commit", "-q", "--amend", "--no-verify", "-F", msgPath)
	if out, code := h.run("", "hook", "post-commit"); code != 0 {
		t.Fatalf("post-commit exited %d: %s", code, out)
	}

	amended := h.git("rev-parse", "HEAD")
	second, count := trailerOf(t, h, amended)
	if count != 1 {
		t.Errorf("the amended commit carries %d docket trailers, want 1", count)
	}
	if second == first {
		t.Error("the trailer still names the record built from the pre-amend content")
	}

	out, code := h.run("", "verify", amended)
	if code != 0 {
		t.Fatalf("verify exited %d: %s", code, out)
	}
	if !strings.Contains(out, "describes     ok") {
		t.Errorf("the record should describe the amended commit:\n%s", out)
	}
}

// A cherry-pick or rebase re-applies a commit onto a different parent without
// running prepare-commit-msg, so the trailer rides along onto a diff it was
// never built from. Verify has to notice.
func TestRecordThatDoesNotDescribeItsCommitIsReported(t *testing.T) {
	h := newHarness(t)
	if _, code := h.run("", "init"); code != 0 {
		t.Fatal("init failed")
	}
	h.write("base.js", "export const base = 0;\n")
	root := h.commit("chore: base")

	h.write("base.js", "export const base = 1;\nexport const extra = 2;\n")
	h.commit("chore: more base")

	h.write("src/a.js", "export const a = 1;\nexport const b = 2;\n")
	feature := h.commit("feat: a")

	// Re-apply the last commit onto the root, the way a rebase would: same
	// change, different parent from the one its record was built against.
	h.git("checkout", "-q", "-b", "side", root)
	h.git("cherry-pick", "--no-commit", feature)
	h.git("commit", "-q", "--no-verify", "-m", h.git("show", "-s", "--format=%B", feature))

	picked := h.git("rev-parse", "HEAD")
	if _, count := trailerOf(t, h, picked); count != 1 {
		t.Fatalf("expected the trailer to travel with the message, got %d", count)
	}
	out, code := h.run("", "verify", picked)
	if !strings.Contains(out, "describes     NO") {
		t.Errorf("verify should report that the record does not describe this commit (exit %d):\n%s", code, out)
	}
}

func trailerOf(t *testing.T, h *harness, sha string) (digest string, count int) {
	t.Helper()
	body := h.git("show", "-s", "--format=%B", sha)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Docket:") {
			count++
			digest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Docket:"))
		}
	}
	return digest, count
}
