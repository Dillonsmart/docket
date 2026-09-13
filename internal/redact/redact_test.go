package redact

import "strings"

import "testing"

// stripeSample is assembled at run time rather than written out. A literal
// credential shape in the source trips secret scanners on push, and a test for
// a redaction rule should not be the reason a repository is flagged.
var stripeSample = "sk_" + "live_" + "4eC39HqLyjWDarjtT1zdp7dc"

func TestSecretsAreRemoved(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string
		rule   string
	}{
		{"github token", "use ghp_abcdefghijklmnopqrstuvwxyz0123456789 to push", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "github-token"},
		{"aws key id", "AKIAIOSFODNN7EXAMPLE is the key", "AKIAIOSFODNN7EXAMPLE", "aws-access-key-id"},
		{"stripe", "billing key " + stripeSample, stripeSample, "stripe-key"},
		{"assignment", `DB_PASSWORD="hunter2hunter2"`, "hunter2hunter2", "assigned-secret"},
		{"url credentials", "postgres://user:s3cr3tpw@db:5432/app", "s3cr3tpw", "url-credentials"},
		{"jwt", "Cookie: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "eyJhbGciOiJIUzI1NiJ9", "jwt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Text(c.in)
			if strings.Contains(got.Text, c.secret) {
				t.Errorf("secret survived: %q", got.Text)
			}
			if !contains(got.Rules, c.rule) {
				t.Errorf("rules = %v, want %s", got.Rules, c.rule)
			}
		})
	}
}

// Fail-closed: a long random-looking token is removed even though no named rule
// matches it.
func TestHighEntropyTokenIsRemoved(t *testing.T) {
	got := Text("token is Xk7Fq2mZp9Ly4Rt8Wn3Bv6Hs1Jd5Gc0A")
	if strings.Contains(got.Text, "Xk7Fq2mZp9Ly4Rt8Wn3Bv6Hs1Jd5Gc0A") {
		t.Errorf("high-entropy token survived: %q", got.Text)
	}
}

// Over-redaction is the safe direction, but a docket is useless if it cannot
// name an object id or an ordinary identifier.
func TestOrdinaryTextSurvives(t *testing.T) {
	keep := []string{
		"8f3a2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c",
		"func newSessionIdentifierFactory(clock Clock) *Factory",
		"src/auth/session_repository_test.go:104",
		"npx vitest run --coverage",
	}
	for _, in := range keep {
		if got := Text(in); got.Text != in {
			t.Errorf("Text(%q) = %q, should be unchanged", in, got.Text)
		}
	}
}

func TestSensitivePath(t *testing.T) {
	sensitive := []string{".env", ".env.production", "config/secrets.yml", "certs/server.pem", "~/.ssh/id_rsa", "app/.aws/credentials"}
	for _, p := range sensitive {
		if !SensitivePath(p) {
			t.Errorf("SensitivePath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"src/auth.js", "config/app.php", "README.md"} {
		if SensitivePath(p) {
			t.Errorf("SensitivePath(%q) = true, want false", p)
		}
	}
}

func TestExcerptTruncates(t *testing.T) {
	long := strings.Repeat("abcde ", 100)
	got := Excerpt(long, 20)
	if len([]rune(got.Text)) > 21 {
		t.Errorf("excerpt is %d runes: %q", len([]rune(got.Text)), got.Text)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
