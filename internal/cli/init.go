package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/collect"
	"github.com/Dillonsmart/docket/internal/gitx"
)

// gitHooks are the two git hooks docket installs.
//
// The commit trailer is written by prepare-commit-msg on purpose: the audited
// agent must never be the thing that writes its own audit record, and a hook
// fires for human commits too.
var gitHooks = []string{"prepare-commit-msg", "post-commit"}

func cmdInit(env *env, args []string) error {
	fs, dir := newFlags("init", env)
	binary := fs.String("binary", "", "path to the docket binary to call from hooks")
	remote := fs.String("remote", "origin", "remote to configure the docket refspec on")
	noAgent := fs.Bool("no-agent-hooks", false, "skip the Claude Code hooks that observe shell edits")
	force := fs.Bool("force", false, "overwrite existing git hooks that are not docket's")
	if _, err := parse(fs, args); err != nil {
		return exitError{code: 2}
	}
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}
	bin := *binary
	if bin == "" {
		bin = resolveBinary()
	}

	fmt.Fprintf(env.stdout, "docket init in %s\n\n", repo.Root)

	// 1. Git hooks.
	hooksDir := repo.HooksDir()
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	for _, name := range gitHooks {
		path := filepath.Join(hooksDir, name)
		script := hookScript(name, bin)
		existing, err := os.ReadFile(path)
		switch {
		case err == nil && strings.Contains(string(existing), "docket hook "):
			if string(existing) != script {
				if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
					return err
				}
				fmt.Fprintf(env.stdout, "  updated  %s\n", rel(repo.Root, path))
			} else {
				fmt.Fprintf(env.stdout, "  ok       %s already installed\n", rel(repo.Root, path))
			}
		case err == nil && !*force:
			// Someone else's hook is there. Overwriting it silently would be a
			// hostile thing for a tool to do on a machine it has just met.
			snippet := path + ".docket"
			if err := os.WriteFile(snippet, []byte(script), 0o755); err != nil {
				return err
			}
			fmt.Fprintf(env.stdout, "  WARNING  %s exists and is not docket's.\n", rel(repo.Root, path))
			fmt.Fprintf(env.stdout, "           Wrote %s instead. Add this line to your hook:\n", rel(repo.Root, snippet))
			fmt.Fprintf(env.stdout, "             %s hook %s \"$@\"\n", bin, name)
		default:
			if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
				return err
			}
			fmt.Fprintf(env.stdout, "  wrote    %s\n", rel(repo.Root, path))
		}
	}

	// 2. Refspec, so records travel with a clone.
	//
	// Orphan refs are not fetched by default. This is the rough edge the plan
	// calls out; configuring it here is the whole mitigation.
	refspec := "+refs/docket/*:refs/docket/*"
	key := "remote." + *remote + ".fetch"
	if _, ok := repo.ConfigGet("remote." + *remote + ".url"); ok {
		if !contains(repo.ConfigGetAll(key), refspec) {
			if err := repo.ConfigAdd(key, refspec); err != nil {
				return err
			}
			fmt.Fprintf(env.stdout, "  wrote    %s = %s\n", key, refspec)
		} else {
			fmt.Fprintf(env.stdout, "  ok       %s already configured\n", key)
		}
	} else {
		fmt.Fprintf(env.stdout, "  skipped  no remote %q yet; run docket init again after adding one\n", *remote)
	}

	// 3. A local signing key, so records are signed from the first commit.
	signer, err := cer.LocalSigner(repo.StateDir())
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "  ok       local signing key %s (never leaves %s)\n", signer.KeyID(), rel(repo.Root, repo.StateDir()))

	// 4. Agent hooks, which are what make shell-driven edits attributable.
	if !*noAgent {
		changed, path, err := installAgentHooks(repo, bin)
		if err != nil {
			fmt.Fprintf(env.stdout, "  WARNING  could not configure the agent hooks: %v\n", err)
		} else if changed {
			fmt.Fprintf(env.stdout, "  wrote    %s (PreToolUse/PostToolUse on Bash)\n", rel(repo.Root, path))
		} else {
			fmt.Fprintf(env.stdout, "  ok       %s already has docket's hooks\n", rel(repo.Root, path))
		}
	}

	// 5. A collector baseline. Without one, the first observed diff has nothing
	// to compare against and would have to be discarded.
	if st, err := collect.Open(repo); err == nil {
		if err := st.Before(""); err != nil {
			fmt.Fprintf(env.stdout, "  WARNING  could not take a baseline snapshot: %v\n", err)
		} else {
			fmt.Fprintf(env.stdout, "  ok       baseline snapshot taken\n")
		}
	}

	fmt.Fprintf(env.stdout, `
Done. No account, nothing sent anywhere.

  every commit         gets a %s trailer and a record on %s
  docket show HEAD     read the record for a commit
  docket push          send records to the remote (or: git push %s %s)
  docket doctor        check what docket can see here
`, cer.Trailer, cer.Ref, *remote, cer.Ref)
	return nil
}

func hookScript(name, bin string) string {
	// The hook exits 0 when docket is missing rather than failing the commit.
	// Somebody will clone this repository without the binary installed, and a
	// tool that blocks their commits is a tool they will delete.
	return fmt.Sprintf(`#!/bin/sh
# Installed by docket (https://github.com/Dillonsmart/docket).
#
# docket never fails a commit: this hook exits 0 when docket is absent or when
# the record cannot be built. Failures are written to .git/docket/docket.log.
BIN=%q
if [ ! -x "$BIN" ]; then
	BIN=$(command -v docket 2>/dev/null) || exit 0
fi
[ -n "$BIN" ] || exit 0
exec "$BIN" hook %s "$@"
`, bin, name)
}

// agentHooksPath is where Claude Code reads project settings from.
func agentHooksPath(repo *gitx.Repo) string {
	return filepath.Join(repo.Root, ".claude", "settings.json")
}

// installAgentHooks adds the PreToolUse and PostToolUse Bash hooks that let
// docket observe edits made through the shell, merging into whatever is already
// configured rather than replacing it.
func installAgentHooks(repo *gitx.Repo, bin string) (bool, string, error) {
	path := agentHooksPath(repo)
	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return false, path, fmt.Errorf("%s is not valid JSON", rel(repo.Root, path))
		}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	for event, sub := range map[string]string{"PreToolUse": "pre", "PostToolUse": "post"} {
		command := fmt.Sprintf("%s collect %s", bin, sub)
		list, _ := hooks[event].([]any)
		if hasCommand(list, "docket collect "+sub) {
			continue
		}
		list = append(list, map[string]any{
			"matcher": "Bash",
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": command,
				"timeout": 20,
			}},
		})
		hooks[event] = list
		changed = true
	}
	if !changed {
		return false, path, nil
	}
	settings["hooks"] = hooks
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, path, err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, path, err
	}
	return true, path, os.WriteFile(path, append(data, '\n'), 0o644)
}

func hasCommand(list []any, needle string) bool {
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, needle) {
				return true
			}
		}
	}
	return false
}

// resolveBinary works out what hooks should call. During `go run` the executable
// lives in a temporary directory that will not exist later, so fall back to the
// name and let PATH resolve it.
func resolveBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "docket"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.Contains(exe, os.TempDir()) || strings.Contains(exe, "/go-build") {
		return "docket"
	}
	return exe
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
