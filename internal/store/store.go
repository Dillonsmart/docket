// Package store keeps dockets in the repository itself, on an orphan ref.
//
// This is the storage decision the plan locks: zero infrastructure, the records
// travel with the repository, and nothing leaves the git host the team already
// trusts. A record is a blob; the ref is a commit history over a tree of those
// blobs, indexed by digest and by commit id.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/gitx"
)

// Write stores a record and returns its payload digest.
func Write(repo *gitx.Repo, rec *cer.Record) (string, error) {
	data, err := rec.Canonical()
	if err != nil {
		return "", err
	}
	digest := rec.PayloadHash
	if digest == "" {
		digest, err = rec.Digest()
		if err != nil {
			return "", err
		}
	}
	blob, err := repo.WriteBlob(data)
	if err != nil {
		return "", err
	}
	entries := []gitx.TreeEntry{{Path: cer.Path(digest), Blob: blob}}
	if rec.Commit != "" {
		// The by-commit index survives a lost trailer: a squash merge or a rebase
		// rewrites the message, and the record should still be findable.
		pointer, err := repo.WriteBlob([]byte(digest + "\n"))
		if err != nil {
			return "", err
		}
		entries = append(entries, gitx.TreeEntry{Path: cer.CommitPath(rec.Commit), Blob: pointer})
	}
	msg := fmt.Sprintf("docket: %s", short(digest))
	if rec.Commit != "" {
		msg = fmt.Sprintf("docket for %s", short(rec.Commit))
	}
	if _, err := repo.CommitRecords(cer.Ref, msg, entries); err != nil {
		return "", err
	}
	return digest, nil
}

// ReadByDigest loads a record by its payload digest.
func ReadByDigest(repo *gitx.Repo, digest string) (*cer.Record, error) {
	data, ok := repo.ReadRecord(cer.Ref, cer.Path(digest))
	if !ok {
		return nil, fmt.Errorf("no docket stored for %s", short(digest))
	}
	return decode(data)
}

// ReadByCommit finds the record for a commit: by its trailer when it has one,
// otherwise through the by-commit index.
func ReadByCommit(repo *gitx.Repo, rev string) (*cer.Record, string, error) {
	sha, err := repo.RevParse(rev)
	if err != nil {
		return nil, "", err
	}
	if digest, ok := repo.Trailer(sha, cer.Trailer); ok && digest != "" {
		rec, err := ReadByDigest(repo, digest)
		if err == nil {
			return rec, digest, nil
		}
	}
	if data, ok := repo.ReadRecord(cer.Ref, cer.CommitPath(sha)); ok {
		digest := strings.TrimSpace(string(data))
		rec, err := ReadByDigest(repo, digest)
		if err == nil {
			return rec, digest, nil
		}
	}
	return nil, "", fmt.Errorf("no docket for %s: the commit has no %s trailer and nothing is indexed for it", short(sha), cer.Trailer)
}

func decode(data []byte) (*cer.Record, error) {
	var rec cer.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("stored docket is not valid JSON: %w", err)
	}
	return &rec, nil
}

// List returns the digests of every stored record.
func List(repo *gitx.Repo) ([]string, error) {
	paths, err := repo.ListRecords(cer.Ref)
	if err != nil {
		return nil, nil // no ref yet
	}
	var out []string
	for _, p := range paths {
		if !strings.HasPrefix(p, "records/") || !strings.HasSuffix(p, ".json") {
			continue
		}
		trimmed := strings.TrimSuffix(strings.TrimPrefix(p, "records/"), ".json")
		out = append(out, "sha256:"+strings.ReplaceAll(trimmed, "/", ""))
	}
	return out, nil
}

// ---------------------------------------------------------------- pending

// Pending is a record built before its commit existed.
//
// prepare-commit-msg writes the trailer, and only then does git create the
// commit. So the record is parked here, keyed by the digest that went into the
// trailer, and post-commit picks it up, stamps the commit id on it and stores it.
func pendingDir(repo *gitx.Repo) string { return filepath.Join(repo.StateDir(), "pending") }

// SavePending parks a built record.
func SavePending(repo *gitx.Repo, rec *cer.Record) error {
	dir := pendingDir(repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := rec.Canonical()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fileFor(rec.PayloadHash)), data, 0o644)
}

// LoadPending retrieves a parked record.
func LoadPending(repo *gitx.Repo, digest string) (*cer.Record, error) {
	data, err := os.ReadFile(filepath.Join(pendingDir(repo), fileFor(digest)))
	if err != nil {
		return nil, err
	}
	return decode(data)
}

// ClearPending removes parked records, keeping the most recent few in case a
// commit is amended.
func ClearPending(repo *gitx.Repo, digest string) {
	_ = os.Remove(filepath.Join(pendingDir(repo), fileFor(digest)))
}

func fileFor(digest string) string {
	return strings.ReplaceAll(strings.TrimPrefix(digest, "sha256:"), "/", "") + ".json"
}

func short(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
