package sessions

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PS is the process table's answer to a PIDProbe, from ps(1): when pid
// started, or os.ErrNotExist for a process that is not running. An absent
// process is kept distinct from a failed inspection, which is never evidence
// of absence.
func PS(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 && len(exit.Stderr) == 0 {
			return 0, os.ErrNotExist
		}
		return 0, err
	}
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}
