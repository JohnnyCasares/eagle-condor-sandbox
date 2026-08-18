package run

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"eagle-condor-sandbox/internal/catalog"
	"eagle-condor-sandbox/internal/logbuf"
)

// Spec is everything needed to launch one run.
type Spec struct {
	Workflow *catalog.Workflow
	Layout   Layout

	// Env holds the workflow's own variables — the inputs[].env mapping plus
	// ENV. Isolation and timeout variables are added here.
	Env map[string]string

	TimeoutSeconds int
}

// Runtime is the fixed part of the environment: where node and the automation
// project live.
type Runtime struct {
	AutomationDir string
	PlaywrightCLI string
	NodeBin       string
	ReceiptsDir   string
}

// Outcome is what the child process produced.
type Outcome struct {
	ExitCode int
	State    State
	Err      error
}

// playwrightGrace is how long before the wall-clock deadline Playwright is told
// to give up. It needs to fail its own test first so its reporters flush and the
// artifacts exist; if the hard kill lands first, a timed-out run yields nothing.
const playwrightGrace = 120 * time.Second

// Execute runs the workflow to completion, streaming output into buf.
//
// onStart is called with the child's PID as soon as it exists, so the caller can
// record it for cancellation and for cleanup after a restart.
func Execute(
	ctx context.Context,
	rt Runtime,
	spec Spec,
	buf *logbuf.Buffer,
	onStart func(pid int),
) Outcome {
	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logPath := spec.Layout.LogFile()
	logFile, err := os.Create(logPath)
	if err != nil {
		return Outcome{ExitCode: -1, State: StateErrored,
			Err: fmt.Errorf("creating %s: %w", logPath, err)}
	}
	defer logFile.Close()

	args := []string{
		rt.PlaywrightCLI,
		"test",
		filepath.ToSlash(spec.Workflow.Spec),
		"--output=" + spec.Layout.Output(),
	}
	cmd := exec.Command(rt.NodeBin, args...)
	cmd.Dir = rt.AutomationDir
	cmd.Env = buildEnv(rt, spec)
	prepareCmd(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Outcome{ExitCode: -1, State: StateErrored, Err: err}
	}
	cmd.Stderr = cmd.Stdout // one interleaved stream, same as a terminal

	buf.Append(fmt.Sprintf("$ %s %s", rt.NodeBin, filepath.Base(rt.PlaywrightCLI)+
		" test "+spec.Workflow.Spec))

	if err := cmd.Start(); err != nil {
		return Outcome{ExitCode: -1, State: StateErrored,
			Err: fmt.Errorf("starting playwright: %w", err)}
	}
	pid := cmd.Process.Pid
	if onStart != nil {
		onStart(pid)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // PS error dumps get long
		w := bufio.NewWriter(logFile)
		defer w.Flush()
		for sc.Scan() {
			line := sc.Text()
			buf.Append(line)
			w.WriteString(logbuf.StripANSI(line))
			w.WriteByte('\n')
		}
	}()

	// exec.CommandContext would kill only the node process, leaving Chromium
	// behind, so supervise the deadline here and tear down the whole tree.
	killed := make(chan State, 1)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			state := StateCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				state = StateTimedOut
			}
			killed <- state
			if err := KillTree(pid); err != nil {
				buf.Append("[server] failed to kill process tree: " + err.Error())
			}
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	close(done)
	wg.Wait()

	exitCode := 0
	var ee *exec.ExitError
	if waitErr != nil {
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			return Outcome{ExitCode: -1, State: StateErrored, Err: waitErr}
		}
	}

	select {
	case state := <-killed:
		return Outcome{ExitCode: exitCode, State: state}
	default:
	}

	if exitCode == 0 {
		return Outcome{ExitCode: 0, State: StateSucceeded}
	}
	return Outcome{ExitCode: exitCode, State: StateFailed}
}

// buildEnv constructs the child environment explicitly.
//
// Deliberately NOT os.Environ(): a stray TA_USER_ID or ENV exported on the
// server would otherwise apply silently to every run, because core/users.js and
// core/environments.js fall back to whatever is set. An allowlist makes each run
// depend only on what the request asked for.
func buildEnv(rt Runtime, spec Spec) []string {
	env := map[string]string{}

	// Minimum for node and a browser to start.
	passthrough := []string{
		"PATH", "SystemRoot", "windir", "TEMP", "TMP", "HOME", "USERPROFILE",
		"LANG", "LC_ALL", "TZ",
		"PLAYWRIGHT_BROWSERS_PATH", "NODE_OPTIONS",
	}
	for _, k := range passthrough {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}

	// Per-run isolation. Without these, concurrent runs share the receipts
	// library, the temp dir and the report locations.
	env["PS_RECEIPTS_DIR"] = rt.ReceiptsDir
	env["PS_RUN_TMP_DIR"] = spec.Layout.Tmp()
	env["PW_HTML_REPORT_DIR"] = spec.Layout.Report()
	env["PW_RESULT_JSON"] = spec.Layout.ReportJSON()

	// Playwright must give up before the wall clock does, so its reporters run.
	pwTimeout := time.Duration(spec.TimeoutSeconds)*time.Second - playwrightGrace
	if pwTimeout < 30*time.Second {
		pwTimeout = 30 * time.Second
	}
	ms := strconv.FormatInt(pwTimeout.Milliseconds(), 10)
	env["PS_RUN_TIMEOUT_MS"] = ms
	env["PW_TIMEOUT"] = ms

	// Server runs are always headless, never record traces by default, and do
	// not write to the shared input catalogue.
	env["PW_HEADLESS"] = "true"
	env["PW_TRACE"] = "off"
	env["TRACK_INPUTS"] = "false"

	// The request's own variables win, so PW_TRACE=on can be asked for.
	for k, v := range spec.Env {
		env[k] = v
	}

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
