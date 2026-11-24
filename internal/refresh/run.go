package refresh

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/qiushiyan/headroom/internal/agy"
)

// run is a read made by running the vendor's own program: Antigravity's agy,
// which signs in by itself and prints its quota. bin is resolved on PATH when
// it runs; dir is the working directory headroom owns for it, so agy registers
// no repository of the caller's as a workspace; env is the launch
// constructor's.
type run struct {
	bin string
	dir string
	env []string
}

// runTimeout bounds one run like the HTTP client's bound bounds a GET, so a
// round ends when it always did. A read takes about two seconds (measured on
// agy 1.5.13); a variable so a test can prove the bound with a wedged stub.
var runTimeout = 10 * time.Second

// ask sends the candidate's prepared request through its transport.
func (c Candidate) ask(ctx context.Context, client *http.Client) response {
	if c.run != nil {
		return c.run.ask(ctx)
	}
	return fetch(ctx, client, c)
}

// ask runs the read once. A run that could not start, or outlived the bound,
// is a transport failure; one that exited non-zero reports its status and
// nothing else; one that exited 0 hands back headroom's envelope of what it
// printed and whose sign-in its log names — or no body at all, when what it
// printed was not a JSON object, which the parser then calls unparseable.
func (r *run) ask(ctx context.Context) response {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return response{Err: err}
	}
	logFile, err := os.CreateTemp("", "headroom-agy-*.log")
	if err != nil {
		return response{Err: err}
	}
	logPath := logFile.Name()
	logFile.Close()
	defer os.Remove(logPath)

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin, agy.Args(logPath)...)
	cmd.Dir, cmd.Env = r.dir, r.env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// A grandchild holding the output pipe must not outlive the bound.
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return response{Err: ctx.Err()}
	case errors.As(err, &exit):
		return response{Exit: exit.ExitCode()}
	case err != nil:
		return response{Err: err}
	}
	if stdout.Len() > 4<<20 {
		return response{}
	}
	log, _ := os.ReadFile(logPath)
	body, err := agy.Wrap(stdout.Bytes(), log)
	if err != nil {
		return response{}
	}
	return response{Body: body}
}
