#!/usr/bin/env bash
#
# Build a throwaway repository with a recorded agent session in it, then print
# the docket for the commit. Nothing here touches your own repositories.
#
#   scripts/demo.sh
#   DOCKET=./docket scripts/demo.sh    # use a specific binary
#
# What it stages: an agent fixes a session fixation bug, gets it wrong once,
# sees a test fail, fixes it properly, and runs the suite. It also edits a
# second file through the shell, which the transcript does not record — so the
# two hunks in the commit have very different evidence behind them.
set -euo pipefail

DOCKET="${DOCKET:-docket}"
command -v "$DOCKET" >/dev/null 2>&1 || { echo "docket not found; set DOCKET=/path/to/docket" >&2; exit 1; }

WORK=$(mktemp -d 2>/dev/null || mktemp -d -t docket-demo)
trap 'rm -rf "$WORK"' EXIT
export CLAUDE_CONFIG_DIR="$WORK/agent-home"

git init -q "$WORK/repo"
REPO=$(cd "$WORK/repo" && git rev-parse --show-toplevel)
cd "$REPO"
git config user.email demo@example.com
git config user.name "Demo"

printf 'coverage/\n.claude/\n' > .gitignore
mkdir -p src
cat > src/session.ts <<'TS'
import { randomUUID } from "node:crypto";

export function createSession(user: User): Session {
  return { id: randomUUID(), user, created: Date.now() };
}

export function isExpired(s: Session, now: number): boolean {
  return now - s.created > SESSION_TTL_MS;
}
TS
cat > src/audit.ts <<'TS'
export function recordLogin(userId: string, ip: string) {
  logger.info("login", { userId, ip });
}
TS
git add -A
git commit -q -m "chore: session scaffold"

"$DOCKET" init >/dev/null

# --- the shell edit, observed by docket's own hooks -------------------------
HOOK_JSON="{\"session_id\":\"demo-session\",\"cwd\":\"$REPO\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"sed -i '' 's/{ userId, ip }/{ userId, ip, at: Date.now() }/' src/audit.ts\"}}"
printf '%s' "$HOOK_JSON" | "$DOCKET" collect pre
cat > src/audit.ts <<'TS'
export function recordLogin(userId: string, ip: string) {
  logger.info("login", { userId, ip, at: Date.now() });
  metrics.increment("auth.login");
}
TS
printf '%s' "$HOOK_JSON" | "$DOCKET" collect post

# --- the recorded session ---------------------------------------------------
SLUG=$(printf '%s' "$REPO" | tr '/.' '--')
PROJECT="$CLAUDE_CONFIG_DIR/projects/$SLUG"
mkdir -p "$PROJECT"

# Three versions of the file: what it was, the approach that failed, and the
# one that shipped. The transcript records the images, which is what makes the
# attribution verifiable rather than inferred.
BEFORE=$(cat src/session.ts)

FIRST=$(cat <<'TS'
import { randomUUID } from "node:crypto";

export function createSession(user: User): Session {
  // regenerate on every login so a pre-auth id cannot be replayed
  return { id: randomUUID(), user, created: Date.now(), rotated: true };
}

export function isExpired(s: Session, now: number): boolean {
  return now - s.created > SESSION_TTL_MS;
}
TS
)

FINAL=$(cat <<'TS'
import { randomUUID } from "node:crypto";

export function createSession(user: User): Session {
  // A session id minted before authentication must never survive it: the
  // attacker planted it. Mint a fresh one, namespaced so a leaked pre-auth
  // id cannot be replayed against this table.
  return { id: mintSessionId(), user, created: Date.now() };
}

function mintSessionId(): string {
  return `sess_${randomUUID()}`;
}

export function isExpired(s: Session, now: number): boolean {
  return now - s.created > SESSION_TTL_MS;
}
TS
)
printf '%s\n' "$FINAL" > src/session.ts

json_string() { printf '%s' "$1" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))'; }
now() { python3 -c "import datetime,sys; print((datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(seconds=int(sys.argv[1]))).isoformat().replace('+00:00','Z'))" "$1"; }

FILE=$(json_string "$REPO/src/session.ts")
# The images are quoted exactly as the harness records them, trailing newline
# included: an edit tool matches bytes, not lines.
B=$(json_string "$BEFORE
"); F=$(json_string "$FIRST
"); V=$(json_string "$FINAL
")

# acceptEdits, so the docket says the agent wrote without asking rather than
# implying someone read the diff.
cat > "$PROJECT/demo-session.jsonl" <<JSONL
{"type":"permission-mode","permissionMode":"acceptEdits","sessionId":"demo-session"}
{"type":"user","uuid":"u1","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 300)","message":{"role":"user","content":"Fix the session fixation issue on login — an id minted before auth is still valid afterwards"}}
{"type":"assistant","uuid":"a1","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 290)","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"Rotating the id at login and flagging it, so a pre-auth id is no longer accepted."},{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":$FILE}}]}}
{"type":"user","uuid":"u2","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 289)","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"e1"}]},"toolUseResult":{"filePath":$FILE,"oldString":$B,"newString":$F,"originalFile":$B,"structuredPatch":[],"userModified":false,"replaceAll":false}}
{"type":"assistant","uuid":"a2","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 280)","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"npx vitest run","description":"run the suite"}}]}}
{"type":"user","uuid":"u3","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 260)","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b1"}]},"toolUseResult":{"stdout":" FAIL  tests/session.spec.ts > rejects a pre-auth id\n Tests  1 failed | 7 passed","stderr":"","interrupted":false}}
{"type":"assistant","uuid":"a3","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 240)","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"The rotated flag advertises that a fixation attempt happened, and a raw uuid is still guessable across tables. Minting a namespaced id instead."},{"type":"tool_use","id":"e2","name":"Edit","input":{"file_path":$FILE}}]}}
{"type":"user","uuid":"u4","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 239)","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"e2"}]},"toolUseResult":{"filePath":$FILE,"oldString":$F,"newString":$V,"originalFile":$F,"structuredPatch":[],"userModified":false,"replaceAll":false}}
{"type":"assistant","uuid":"a4","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 220)","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"b2","name":"Bash","input":{"command":"npx vitest run --coverage","description":"suite with coverage"}}]}}
{"type":"user","uuid":"u5","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 200)","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b2"}]},"toolUseResult":{"stdout":" Test Files  4 passed (4)\n      Tests  8 passed (8)","stderr":"","interrupted":false}}
{"type":"assistant","uuid":"a5","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 190)","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"b3","name":"Bash","input":{"command":"npx tsc --noEmit","description":"typecheck"}}]}}
{"type":"user","uuid":"u6","sessionId":"demo-session","cwd":"$REPO","timestamp":"$(now 180)","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b3"}]},"toolUseResult":{"stdout":"Found 0 errors.","stderr":"","interrupted":false}}
JSONL

# Coverage for the tested file only, so the other hunk has nothing behind it.
mkdir -p coverage
python3 - "$REPO" <<'PY' > coverage/coverage-final.json
import json, sys
p = sys.argv[1] + "/src/session.ts"
print(json.dumps({p: {"path": p,
  "statementMap": {str(i): {"start": {"line": i + 1}, "end": {"line": i + 1}} for i in range(16)},
  "s": {str(i): 6 for i in range(16)}}}))
PY

git add -A
git commit -q -m "fix: mint a fresh session id at login

An id minted before authentication survived it, so an attacker who planted
one kept a valid session afterwards."

echo
"$DOCKET" show HEAD --all
