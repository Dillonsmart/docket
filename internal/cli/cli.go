// Package cli is docket's command line.
//
// Two rules shape it. Hooks must never break a commit, so anything running
// inside one logs its failure and exits zero. And nothing requires an account:
// every command here works against a bare repository with no network.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/build"
	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/gitx"
)

// Version is set at build time; it is recorded in every docket.
var Version = "0.1.0-dev"

const usage = `docket — a per-commit evidence record for agent-written code

usage: docket <command> [options]

  init                install the hooks and refspec in this repository
  build               build a record for a commit or the staged change
  show [<rev>]        show the record for a commit (default HEAD)
  explain <file>:<n>  show why a line exists, found through git blame
  review [<rev>]      risk-ordered review output, for a terminal or a PR comment
  verify [<rev>]      check a record's digest, signature and commit binding
  gate                measure attribution quality against real history
  doctor              report what docket can and cannot see here
  push [<remote>]     push refs/docket/* to a remote
  fetch [<remote>]    fetch refs/docket/* from a remote
  collect <pre|post>  agent hook entry point, reads hook JSON on stdin
  hook <name>         git hook entry point
  version             print the version

Run docket <command> -h for that command's options.
`

// Main runs the CLI and returns a process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	build.Version = Version
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	env := &env{stdin: stdin, stdout: stdout, stderr: stderr}
	// -C before the subcommand works the way it does in git, which is where the
	// muscle memory comes from. It also still works after it, as a flag.
	for len(args) >= 2 && args[0] == "-C" {
		if err := os.Chdir(args[1]); err != nil {
			fmt.Fprintf(stderr, "docket: %v\n", err)
			return 1
		}
		args = args[2:]
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(env, rest)
	case "build":
		err = cmdBuild(env, rest)
	case "show":
		err = cmdShow(env, rest)
	case "explain":
		err = cmdExplain(env, rest)
	case "review":
		err = cmdReview(env, rest)
	case "verify":
		err = cmdVerify(env, rest)
	case "gate":
		err = cmdGate(env, rest)
	case "doctor":
		err = cmdDoctor(env, rest)
	case "push":
		err = cmdPush(env, rest)
	case "fetch":
		err = cmdFetch(env, rest)
	case "collect":
		// A collector failure must never interrupt the agent, so this path
		// swallows its errors after logging them.
		return collectMain(env, rest)
	case "hook":
		return hookMain(env, rest)
	case "version":
		fmt.Fprintf(stdout, "docket %s\n", Version)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "docket: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		if ee, ok := err.(exitError); ok {
			if ee.msg != "" {
				fmt.Fprintf(stderr, "docket: %s\n", ee.msg)
			}
			return ee.code
		}
		fmt.Fprintf(stderr, "docket: %v\n", err)
		return 1
	}
	return 0
}

type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// exitError carries a specific exit code, used by commands that gate a pipeline.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error {
	return exitError{code: code, msg: fmt.Sprintf(format, a...)}
}

// openRepo finds the repository from the working directory or -C.
func openRepo(dir string) (*gitx.Repo, error) {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	repo, err := gitx.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %s", dir)
	}
	return repo, nil
}

// signerFor returns the signing key to use: the CI key when one is configured,
// otherwise this machine's local key.
func signerFor(repo *gitx.Repo, unsigned bool) (*cer.Signer, error) {
	if unsigned {
		return nil, nil
	}
	if s, ok, err := cer.SignerFromEnv(); err != nil {
		return nil, err
	} else if ok {
		return s, nil
	}
	return cer.LocalSigner(repo.StateDir())
}

// logf appends to the repository's docket log. Hooks report here rather than to
// the terminal, where they would look like commit failures.
func logf(repo *gitx.Repo, format string, a ...any) {
	if repo == nil {
		return
	}
	dir := repo.StateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "docket.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(fmt.Sprintf(format, a...)))
}

// newFlags builds a flag set that prints to stderr and takes the common -C flag.
func newFlags(name string, env *env) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	dir := fs.String("C", "", "run as if docket was started in this directory")
	return fs, dir
}

// parse reads flags wherever they appear, so `docket show HEAD --all` works as
// well as `docket show --all HEAD`. Go's flag package stops at the first
// positional argument, and nobody types commands that way.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}
