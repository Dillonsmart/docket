package diffx

import "testing"

const sample = `diff --git a/src/auth.js b/src/auth.js
index 1234567..89abcde 100644
--- a/src/auth.js
+++ b/src/auth.js
@@ -3,1 +3,2 @@ export function newSession(user) {
-  return { id: crypto.randomUUID(), user };
+  return { id: mintId(), user, created: Date.now() };
+}
@@ -10,0 +12,1 @@
+export const TTL = 3600;
diff --git a/README.md b/README.md
new file mode 100644
--- /dev/null
+++ b/README.md
@@ -0,0 +1,2 @@
+# Title
+body
`

func TestParseUnified(t *testing.T) {
	files := ParseUnified(sample)
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	auth := files[0]
	if auth.Path != "src/auth.js" {
		t.Errorf("path = %q", auth.Path)
	}
	if len(auth.Hunks) != 2 {
		t.Fatalf("got %d hunks, want 2", len(auth.Hunks))
	}
	h := auth.Hunks[0]
	if got := h.NewRange(); got != [2]int{3, 4} {
		t.Errorf("new range = %v, want [3 4]", got)
	}
	if len(h.AddedLines) != 2 || h.AddedLines[0].Num != 3 || h.AddedLines[1].Num != 4 {
		t.Errorf("added lines = %+v", h.AddedLines)
	}
	if len(h.RemovedText) != 1 {
		t.Errorf("removed = %+v", h.RemovedText)
	}
	if files[1].Path != "README.md" || !files[1].Added {
		t.Errorf("second file = %+v", files[1])
	}
}

// A zero-length old range is reported by git as the line the change sits after;
// getting this wrong shifts every attribution in the hunk by one.
func TestParseZeroLengthRange(t *testing.T) {
	files := ParseUnified(sample)
	h := files[0].Hunks[1]
	if h.OldLines != 0 || h.OldStart != 11 {
		t.Errorf("old range = %d,%d want 11,0", h.OldStart, h.OldLines)
	}
	if h.NewStart != 12 {
		t.Errorf("new start = %d, want 12", h.NewStart)
	}
}

func TestAlignCarriesEqualLines(t *testing.T) {
	a := []string{"one", "two", "three", "four"}
	b := []string{"one", "TWO", "three", "four", "five"}
	ops := Align(a, b)

	equalPairs := map[int]int{}
	inserts := 0
	for _, op := range ops {
		switch op.Kind {
		case Equal:
			equalPairs[op.AIdx] = op.BIdx
		case Insert:
			inserts++
		}
	}
	for _, i := range []int{0, 2, 3} {
		if equalPairs[i] != i {
			t.Errorf("line %d should align to itself, got %d", i, equalPairs[i])
		}
	}
	if inserts != 2 {
		t.Errorf("got %d inserts, want 2 (the changed line and the new one)", inserts)
	}
}

func TestAlignHandlesEmptySides(t *testing.T) {
	if ops := Align(nil, []string{"a"}); len(ops) != 1 || ops[0].Kind != Insert {
		t.Errorf("insert into empty = %+v", ops)
	}
	if ops := Align([]string{"a"}, nil); len(ops) != 1 || ops[0].Kind != Delete {
		t.Errorf("delete to empty = %+v", ops)
	}
	if ops := Align(nil, nil); len(ops) != 0 {
		t.Errorf("empty/empty = %+v", ops)
	}
}

func TestSplitLines(t *testing.T) {
	cases := map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2, "a\r\nb\r\n": 2}
	for in, want := range cases {
		if got := len(SplitLines(in)); got != want {
			t.Errorf("SplitLines(%q) = %d lines, want %d", in, got, want)
		}
	}
}
