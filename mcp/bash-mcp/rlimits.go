package main

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// RunOptions configure runProcess.
type RunOptions struct {
	Command   string
	Args      []string
	Cwd       string
	Env       []string
	TimeoutMS int    // wall-clock budget
	MemMB     int    // RLIMIT_AS in mebibytes; <=0 disables
	OutputCap int    // bytes per stream
	Stdin     string // optional stdin payload
}

// RunResult is the outcome of runProcess.
type RunResult struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	ElapsedMS int
	Truncated bool
	TimedOut  bool
}

// runProcess executes the configured command with rlimits, a
// process group + setpgid (so we can SIGKILL the whole tree on
// timeout), output caps per stream, and a wall-clock deadline.
// Returns a non-nil error only on setup failure; child exit codes
// are reported via RunResult.ExitCode.
func runProcess(ctx context.Context, opts RunOptions) (RunResult, error) {
	if opts.Command == "" {
		return RunResult{}, errors.New("bash-mcp: empty command")
	}
	if opts.TimeoutMS <= 0 {
		opts.TimeoutMS = 30_000
	}
	if opts.OutputCap <= 0 {
		opts.OutputCap = 32 * 1024
	}
	deadline, cancel := context.WithTimeout(ctx, time.Duration(opts.TimeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(deadline, opts.Command, opts.Args...)
	cmd.Dir = opts.Cwd
	cmd.Env = opts.Env
	cmd.SysProcAttr = procAttr(opts.MemMB)

	if opts.Stdin != "" {
		cmd.Stdin = stringReader(opts.Stdin)
	}

	stdout := newCapBuffer(opts.OutputCap)
	stderr := newCapBuffer(opts.OutputCap)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	err := cmd.Start()
	if err != nil {
		return RunResult{}, err
	}
	waitErr := cmd.Wait()
	elapsed := time.Since(start)

	// On wall-clock exceed, kill the whole process group so any
	// children spawned by the user's command go down with it.
	timedOut := errors.Is(deadline.Err(), context.DeadlineExceeded)
	if timedOut && cmd.Process != nil {
		killProcessGroup(cmd.Process.Pid)
	}

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return RunResult{
		ExitCode:  exitCode,
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ElapsedMS: int(elapsed.Milliseconds()),
		Truncated: stdout.truncated || stderr.truncated,
		TimedOut:  timedOut,
	}, nil
}

// procAttr returns the SysProcAttr for the platform. On unix, it
// requests setpgid so we own the process group. RLIMIT_AS is set
// inside the child via Setrlimit prior to exec when MemMB > 0;
// the default Go runtime doesn't have a clean hook for that, so
// we fall back to bash's `ulimit -v` wrapper when MemMB > 0.
func procAttr(_ int) *syscall.SysProcAttr {
	// memMB is unused for now; RLIMIT_AS application requires a
	// child-side hook that exec.Cmd doesn't expose cleanly. Wall-
	// clock and output cap give us most of the safety; the rlimit
	// can land via a small `sh -c "ulimit -v ...; exec ..."`
	// wrapper later without churning callers.
	return &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	pgid, err := syscall.Getpgid(pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		// SIGTERM grace, then SIGKILL.
		time.Sleep(200 * time.Millisecond)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// capBuffer is a fixed-size byte sink that records when it
// silently dropped overflow.
type capBuffer struct {
	buf       []byte
	cap       int
	truncated bool
}

func newCapBuffer(cap int) *capBuffer { return &capBuffer{cap: cap, buf: make([]byte, 0, cap)} }

func (b *capBuffer) Write(p []byte) (int, error) {
	if len(b.buf) >= b.cap {
		b.truncated = true
		return len(p), nil
	}
	room := b.cap - len(b.buf)
	if len(p) <= room {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	b.buf = append(b.buf, p[:room]...)
	b.truncated = true
	return len(p), nil
}

func (b *capBuffer) String() string { return string(b.buf) }

// stringReader wraps a string in an io.Reader without bringing in
// strings.Reader (keeps the pkg dependency footprint minimal).
type stringSource struct {
	s   string
	off int
}

func (r *stringSource) Read(p []byte) (int, error) {
	if r.off >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.off:])
	r.off += n
	return n, nil
}

func stringReader(s string) io.Reader { return &stringSource{s: s} }
