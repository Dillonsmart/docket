package build

import "testing"

func TestNamingLineIgnoresHeredocBodiesAndOtherPaths(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		path string
		want string
	}{
		{
			name: "the line that writes the file, not the first line of the script",
			cmd:  "cat > .gitignore <<'EOF'\n/docket\nEOF\ncat > README.md <<'MD'\n# docket\nMD",
			path: "README.md",
			want: "cat > README.md <<'MD'",
		},
		{
			name: "a mention inside a heredoc body is content, not a command",
			cmd:  "cat > notes.txt <<'EOF'\nsee README.md\nEOF",
			path: "README.md",
			want: "",
		},
		{
			name: "dash form with a tab-indented terminator",
			cmd:  "cat <<-END > out\n\tREADME.md\n\tEND\necho done",
			path: "README.md",
			want: "",
		},
		{
			name: "an unterminated body runs to the end",
			cmd:  "cat > x <<'EOF'\nREADME.md",
			path: "README.md",
			want: "",
		},
		{
			name: "a different file with the same basename",
			cmd:  "cat > action/README.md <<'MD'\nx\nMD",
			path: "README.md",
			want: "",
		},
		{
			name: "a longer path ending in this one",
			cmd:  "sed -i '' s/a/b/ other/internal/x.go",
			path: "internal/x.go",
			want: "",
		},
		{
			name: "the basename of a nested file",
			cmd:  "cd internal && sed -i '' s/a/b/ x.go",
			path: "internal/x.go",
			want: "cd internal && sed -i '' s/a/b/ x.go",
		},
		{
			name: "a dot-relative path",
			cmd:  "cp ./README.md /tmp/out",
			path: "README.md",
			want: "cp ./README.md /tmp/out",
		},
		{
			name: "an absolute path",
			cmd:  "cat /home/me/repo/internal/x.go",
			path: "internal/x.go",
			want: "cat /home/me/repo/internal/x.go",
		},
		{
			name: "a redirect with no space",
			cmd:  "echo hi >README.md",
			path: "README.md",
			want: "echo hi >README.md",
		},
		{
			name: "a home-relative path",
			cmd:  "wc -l ~/repo/README.md",
			path: "README.md",
			want: "wc -l ~/repo/README.md",
		},
		{
			name: "python fed by heredoc that opens the file is still not a mention",
			cmd:  "python3 - <<PY\nopen('README.md')\nPY",
			path: "README.md",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := c.path
			if i := len(name) - 1; i >= 0 {
				for j := i; j >= 0; j-- {
					if name[j] == '/' {
						name = name[j+1:]
						break
					}
				}
			}
			if got := namingLine(c.cmd, c.path, name); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
