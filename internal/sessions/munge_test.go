package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Claude Code names a project's store dir by replacing every character
// outside [a-zA-Z0-9] with '-' (read from 2.1.318's own path helper). A
// project whose path carries other punctuation — an underscore, a space — must
// still verify, or its row reads "dir gone" and the picker offers dd for a
// session that resumes fine.
func TestACwdVerifiesAgainstTheVendorsStoreDirName(t *testing.T) {
	vendor := regexp.MustCompile(`[^a-zA-Z0-9]`)
	projects, write := storeFixture(t)
	dir := filepath.Join(t.TempDir(), "my_project (v2)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(vendor.ReplaceAllString(dir, "-"), "s-slug.jsonl", rec("s-slug", dir, "underscored"), time.Now())

	l := Collect(Input{ProjectsDir: projects, CWD: "/nowhere"})
	if len(l.Sessions) != 1 {
		t.Fatalf("sessions = %d", len(l.Sessions))
	}
	if s := l.Sessions[0]; s.CWD != dir || !s.DirOK {
		t.Errorf("CWD = %q DirOK = %v, want %q resumable", s.CWD, s.DirOK, dir)
	}
}

// The vendor's rule, by the vendor's own output: testdata/store-names.js is
// its naming helper copied from the binary, and store-names.jsonl what node
// prints running it — separators, the punctuation the old rule missed, a
// letter outside ASCII, a character outside the BMP (two UTF-16 units), and
// names cut at 200 with the hash suffix, one of them from a negative hash.
func TestMungeIsTheVendorsStoreDirName(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "store-names.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var pair [2]string
		if err := json.Unmarshal([]byte(line), &pair); err != nil {
			t.Fatal(err)
		}
		if got := Munge(pair[0]); got != pair[1] {
			t.Errorf("Munge(%.40q…) = %.60q…, want %.60q…", pair[0], got, pair[1])
		}
	}
}
