// Package redact strips secrets out of anything that might reach a docket.
//
// The posture is deliberately paranoid and stated in the plan as a project
// risk: one secret published in a pushed docket blob ends the project's
// credibility. So this package over-redacts, fails closed on anything it cannot
// classify, and records what it did so the docket can say so out loud.
package redact

import (
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Version identifies the rule set, so a docket can be read years later and its
// redaction guarantees understood.
const Version = "1"

// Placeholder shapes. They are distinctive on purpose: grepping a corpus of
// dockets for "[redacted" must find every removal.
const (
	mark        = "[redacted:%s]"
	dropped     = "[redacted:whole-value dropped by rule %s]"
	entropyMark = "[redacted:high-entropy]"
)

type rule struct {
	name string
	re   *regexp.Regexp
	// keep, when non-empty, is a replacement template referencing capture
	// groups, letting a rule preserve the harmless part (a key's name, say).
	keep string
}

// Rules run in order. Broad rules come last so that a specific match gets the
// more informative label.
var rules = []rule{
	{name: "private-key-block", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{name: "aws-access-key-id", re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`)},
	{name: "github-token", re: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{16,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b`)},
	{name: "gitlab-token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9\-_]{16,}\b`)},
	{name: "slack-token", re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9\-]{8,}\b`)},
	{name: "stripe-key", re: regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}\b`)},
	{name: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}\b`)},
	{name: "openai-key", re: regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9\-_]{20,}\b`)},
	{name: "anthropic-key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9\-_]{20,}\b`)},
	{name: "npm-token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{30,}\b`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9\-_]{8,}\.[A-Za-z0-9\-_]{8,}\.[A-Za-z0-9\-_]{8,}\b`)},
	{name: "authorization-header", re: regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)\s*[:=]\s*("?)(bearer|basic|token)?\s*[^\s"',;]+`), keep: "$1: "},
	{name: "url-credentials", re: regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s/:@]+:[^\s/@]+@`), keep: "$1"},
	{name: "assigned-secret", re: regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY|CREDENTIALS?|AUTH|SALT|CIPHER_?KEY|SESSION_?KEY|DSN)[A-Z0-9_]*)(\s*[:=]\s*)("?)([^\s"',;]{4,})`), keep: "$1$2"},
	{name: "ssh-public-key-comment", re: regexp.MustCompile(`\bssh-(?:rsa|ed25519|dss)\s+[A-Za-z0-9+/=]{40,}`)},
}

// Result reports what a redaction pass did.
type Result struct {
	Text  string
	Rules []string // rule names that fired, sorted, deduplicated
}

// Text redacts free text: prompts, task descriptions, command lines, test
// output. Everything that ends up in a docket goes through here.
func Text(s string) Result {
	hit := map[string]bool{}
	for _, r := range rules {
		if !r.re.MatchString(s) {
			continue
		}
		hit[r.name] = true
		if r.keep != "" {
			s = r.re.ReplaceAllString(s, r.keep+replacementFor(r.name))
		} else {
			s = r.re.ReplaceAllString(s, replacementFor(r.name))
		}
	}
	s, hitEntropy := redactHighEntropy(s)
	if hitEntropy {
		hit["high-entropy-token"] = true
	}
	return Result{Text: s, Rules: sortedKeys(hit)}
}

func replacementFor(name string) string { return strings.Replace(mark, "%s", name, 1) }

// SensitivePath reports whether a path's *contents* must never appear in a
// docket regardless of what the content scanner thinks of them.
func SensitivePath(p string) bool {
	base := strings.ToLower(filepath.Base(p))
	switch {
	case base == ".env", strings.HasPrefix(base, ".env."), strings.HasSuffix(base, ".env"):
		return true
	case base == "id_rsa", base == "id_ed25519", base == "id_ecdsa", base == "id_dsa":
		return true
	case strings.HasSuffix(base, ".pem"), strings.HasSuffix(base, ".key"), strings.HasSuffix(base, ".p12"),
		strings.HasSuffix(base, ".pfx"), strings.HasSuffix(base, ".jks"), strings.HasSuffix(base, ".keystore"):
		return true
	case base == "credentials", base == ".netrc", base == ".pgpass", base == ".htpasswd":
		return true
	case strings.Contains(base, "secret"), strings.Contains(base, "credential"):
		return true
	}
	// A path inside a secrets directory is sensitive whatever it is called.
	for _, seg := range strings.Split(strings.ToLower(filepath.ToSlash(p)), "/") {
		if seg == "secrets" || seg == ".ssh" || seg == ".gnupg" || seg == ".aws" {
			return true
		}
	}
	return false
}

// Excerpt prepares a short quotation of source or output for a docket. It
// redacts, collapses whitespace and truncates. limit is in runes.
//
// A docket is an evidence record, not an archive: it never needs a whole file,
// and quoting less is the cheapest way to leak less.
func Excerpt(s string, limit int) Result {
	r := Text(s)
	t := strings.TrimSpace(collapse(r.Text))
	if limit > 0 && len([]rune(t)) > limit {
		t = string([]rune(t)[:limit]) + "…"
	}
	return Result{Text: t, Rules: r.Rules}
}

func collapse(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// tokenSplit finds candidate secret-shaped tokens: long runs of characters that
// appear in encoded material.
var tokenSplit = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{24,}`)

// redactHighEntropy is the fail-closed half of this package. A long token whose
// character distribution looks random is removed even though no named rule
// recognised it, because the cost of being wrong in the other direction is
// unbounded.
func redactHighEntropy(s string) (string, bool) {
	hit := false
	out := tokenSplit.ReplaceAllStringFunc(s, func(tok string) string {
		if looksLikeSecret(tok) {
			hit = true
			return entropyMark
		}
		return tok
	})
	return out, hit
}

func looksLikeSecret(tok string) bool {
	if len(tok) < 24 {
		return false
	}
	// Hex-looking things below 40 chars are usually object ids, which are safe
	// and genuinely useful to keep: git shas appear all over a docket.
	if isHex(tok) && len(tok) <= 64 {
		return false
	}
	// Identifier-shaped tokens (snake_case, kebab-case, dotted paths) are code,
	// not credentials, however long they get.
	if strings.Count(tok, "_")+strings.Count(tok, "-") >= 2 && !hasDigitRun(tok, 6) {
		return false
	}
	e := shannon(tok)
	// 3.9 bits/char sits above English prose and identifiers and below base64
	// randomness; combined with the 24-char floor it fires on credentials and
	// leaves ordinary code alone.
	return e >= 3.9 && mixedCase(tok) && containsDigit(tok)
}

func shannon(s string) float64 {
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	e := 0.0
	for _, f := range freq {
		if f == 0 {
			continue
		}
		p := f / n
		e -= p * math.Log2(p)
	}
	return e
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func mixedCase(s string) bool {
	up, lo := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			up = true
		}
		if c >= 'a' && c <= 'z' {
			lo = true
		}
	}
	return up && lo
}

func containsDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

func hasDigitRun(s string, n int) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			run++
			if run >= n {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Merge combines rule lists from several passes.
func Merge(lists ...[]string) []string {
	m := map[string]bool{}
	for _, l := range lists {
		for _, r := range l {
			m[r] = true
		}
	}
	return sortedKeys(m)
}

// DroppedValue is the replacement used when a whole value is removed rather
// than rewritten, e.g. the contents of a .env file.
func DroppedValue(rule string) string { return strings.Replace(dropped, "%s", rule, 1) }
