package signin

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Vendor is a vendor login run for the dir — by a holder or by the batch —
// registered at Paths.Vendor from its launch until it is reaped, so that
// whoever next holds the login lock can end one its owner left behind: the
// vendor outlives the input it was given (measured, 2.1.311).
type Vendor struct {
	cmd    *exec.Cmd
	reaped chan struct{}
	err    error
	stop   sync.Once
}

// StartVendor starts cmd as the dir's vendor login and registers it, by its
// pid and its start as ps reads it. A vendor runs only while registered: one
// whose registration cannot be written is ended before this returns, since
// nothing could find it should its owner die.
func (p Paths) StartVendor(cmd *exec.Cmd, ps func(int) (int64, error)) (*Vendor, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	start, err := ps(pid)
	if err == nil {
		err = WriteVendor(p.Vendor(), Proc{PID: pid, Start: start})
	}
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("claude auth login could not be registered, so it was ended: %w", err)
	}
	v := &Vendor{cmd: cmd, reaped: make(chan struct{})}
	go func() {
		v.err = cmd.Wait()
		os.Remove(p.Vendor())
		close(v.reaped)
	}()
	return v, nil
}

// Wait returns how the vendor exited, once it is reaped and forgotten.
func (v *Vendor) Wait() error {
	<-v.reaped
	return v.err
}

// Stop ends the vendor — SIGTERM, which it exits on, then a kill after
// grace — and returns once it is reaped: an owner about to return does not
// leave the kill to a goroutine that would die with it.
func (v *Vendor) Stop(grace time.Duration) {
	v.stop.Do(func() {
		v.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-v.reaped:
		case <-time.After(grace):
			v.cmd.Process.Kill()
			<-v.reaped
		}
	})
}

// EndVendor ends the vendor a dead owner left registered, when that pid still
// runs with its recorded start, and forgets it; the caller holds the login
// lock, so no owner is alive. First it waits up to first for the vendor to
// end by itself — a code may be in flight — then SIGTERM, and a kill after
// grace.
func (p Paths) EndVendor(ps func(int) (int64, error), first, grace time.Duration) {
	vendor, ok := ReadVendor(p.Vendor())
	if !ok {
		return
	}
	alive := func() bool {
		start, err := ps(vendor.PID)
		return err == nil && start == vendor.Start
	}
	for deadline := time.Now().Add(first); alive() && time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
	}
	if alive() {
		syscall.Kill(vendor.PID, syscall.SIGTERM)
		for deadline := time.Now().Add(grace); alive() && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
		}
		if alive() {
			syscall.Kill(vendor.PID, syscall.SIGKILL)
		}
	}
	os.Remove(p.Vendor())
}
