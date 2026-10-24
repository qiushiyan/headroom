package check

// The installed binaries, and the seams in them: strings each vendor's binary
// must still carry for what headroom does with it to keep working.

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// missingSeams lists the needles the binary at path does not carry, in the
// order given.
func missingSeams(path string, needles []string) []string {
	found := searchFile(path, needles)
	var missing []string
	for _, n := range needles {
		if !found[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

func claudeBinary() string {
	path, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func codexBinary() (path, resolved string) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", ""
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return path, r
	}
	return path, path
}

// isLauncherScript reports a `#!` file: a wrapper, not the native binary.
func isLauncherScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 2)
	n, _ := f.Read(head)
	return n == 2 && string(head) == "#!"
}

// searchFile streams the (large) binary through searchReader.
func searchFile(path string, needles []string) map[string]bool {
	f, err := os.Open(path)
	if err != nil {
		return map[string]bool{}
	}
	defer f.Close()
	return searchReader(f, needles, 4<<20)
}

// searchReader scans r in bufSize chunks, keeping an overlap of the longest
// needle so a needle split across a chunk boundary is still found.
func searchReader(r io.Reader, needles []string, bufSize int) map[string]bool {
	found := map[string]bool{}
	overlap := 0
	for _, n := range needles {
		if len(n) > overlap {
			overlap = len(n)
		}
	}
	buf := make([]byte, bufSize)
	var tail []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append(tail, buf[:n]...)
			for _, nd := range needles {
				if !found[nd] && bytes.Contains(chunk, []byte(nd)) {
					found[nd] = true
				}
			}
			if len(found) == len(needles) {
				return found
			}
			if len(chunk) > overlap {
				tail = append(tail[:0], chunk[len(chunk)-overlap:]...)
			} else {
				tail = chunk
			}
		}
		if err != nil {
			return found
		}
	}
}
