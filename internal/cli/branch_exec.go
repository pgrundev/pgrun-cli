package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/pgrundev/pgrun-cli/internal/api"
)

const branchExecUsage = "usage: pgrun branch exec [<project>] <name> -- <command...>\n" +
	"   or: pgrun branch exec [<project>] --create <newname> [--ttl 1h|6h|24h|7d] [--from <parent> | --template <t> | --schema-only] [--delete-after] [--timeout 300s] [--timings] -- <command...>"

// branchExec implements `pgrun branch exec`: the headline agent primitive.
// It resolves a branch's DATABASE_URL, runs a command with it set in the
// child process's environment (never on argv, never written to a file),
// passes stdio straight through, and exits with the command's own exit
// code. With --create it creates (and waits for) a fresh branch first, and
// with --delete-after it deletes that branch afterward — even if the
// command itself failed.
//
// Positional shape: an optional <project> (falling back to .pgrun/project,
// see resolveProject) may lead. Exactly one of a following <name> (use an
// existing ready branch) or --create <newname> (make a new one) selects the
// branch — never both, never neither. Everything after a literal "--" is
// the command to run (flag.FlagSet treats "--" as its own terminator, so
// this falls out of fs.Parse for free once the leading positionals are
// peeled off by hand).
func branchExec(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) > 2 {
		return usageErrf(stderr, "branch exec: unexpected argument %q — "+branchExecUsage, pos[2])
	}

	fs := flag.NewFlagSet("branch exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	create := fs.String("create", "", "create a fresh branch with this name before running the command")
	ttl := fs.String("ttl", "", "time-to-live for --create: 1h, 6h, 24h, or 7d")
	from := fs.String("from", "", "parent branch for --create (id, branch_<id>, or name)")
	template := fs.String("template", "", "mint --create's branch from this CI template (schema + seed)")
	schemaOnly := fs.Bool("schema-only", false, "--create a schema-only branch (the built-in \""+api.SchemaOnlyTemplate+"\" template)")
	deleteAfter := fs.Bool("delete-after", false, "delete the --create'd branch after the command runs, even if it fails")
	timeout := fs.Duration("timeout", 300*time.Second, "max time to wait for --create's branch to become ready")
	timings := fs.Bool("timings", false, "print one `pgrun: timings ...` line to stderr: per-phase durations, engine, database")
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	command := fs.Args()

	// Resolve <project>/<name> from the leading positionals now that
	// --create is known: with --create, a lone positional is the project
	// (falling back to .pgrun/project when absent) and a second one is the
	// existing "cannot combine <name> with --create" error; without it, the
	// familiar two-positional <project> <name> (or one-positional <name>,
	// project from the fallback) pattern applies.
	var project, name string
	switch {
	case *create != "":
		if len(pos) == 2 {
			return usageErrf(stderr, "branch exec: cannot combine <name> (%q) with --create %q — "+branchExecUsage, pos[1], *create)
		}
		explicit := ""
		if len(pos) == 1 {
			explicit = pos[0]
		}
		var code int
		var ok bool
		project, code, ok = resolveProject(explicit, stderr)
		if !ok {
			return code
		}
	case len(pos) == 2:
		project, name = pos[0], pos[1]
	case len(pos) == 1:
		name = pos[0]
		var code int
		var ok bool
		project, code, ok = resolveProject("", stderr)
		if !ok {
			return code
		}
	default: // len(pos) == 0, no --create
		return usageErrf(stderr, "branch exec: either <name> or --create <newname> is required — "+branchExecUsage)
	}

	if *create == "" && (*ttl != "" || *from != "" || *deleteAfter || *template != "" || *schemaOnly) {
		return usageErrf(stderr, "branch exec: --ttl/--from/--template/--schema-only/--delete-after require --create")
	}
	tpl, define, msg := templateChoice(*from, *template, *schemaOnly)
	if msg != "" {
		return usageErrf(stderr, "branch exec: %s", msg)
	}
	if *ttl != "" && !api.ValidTTL(*ttl) {
		return usageErrf(stderr, "branch exec: --ttl must be one of 1h, 6h, 24h, 7d")
	}
	if len(command) == 0 {
		return usageErrf(stderr, "branch exec: missing a command after -- — "+branchExecUsage)
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	// From here on a SIGINT/SIGTERM/SIGHUP (a CI runner cancelling the job,
	// a Ctrl-C) must not kill pgrun outright: that would skip --delete-after
	// and leave the branch to its TTL. The signal is routed to execSignals
	// instead — it aborts a readiness wait, or is forwarded to the command —
	// and the deferred delete then runs as usual.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	t := &execTimings{enabled: *timings, start: time.Now()}
	if *create == "" {
		t.branch = name
		code = execAgainstExistingBranch(client, project, name, command, sigs, t, stdout, stderr)
		t.print(stderr, code)
		return code
	}
	t.branch, t.template = *create, tpl
	if tpl != "" {
		began := time.Now()
		ready := ensureTemplateReady(client, project, tpl, define, templateWaitTimeout, stderr)
		t.templateWait, t.templated = time.Since(began), true
		if !ready {
			t.print(stderr, exitFailure)
			return exitFailure
		}
	}
	code = execAgainstCreatedBranch(client, project, *create, *ttl, *from, tpl, *deleteAfter, *timeout, command, sigs, t, stdout, stderr)
	t.print(stderr, code)
	return code
}

// execTimings collects the per-phase durations `--timings` prints. Phases
// that did not happen (no --create, no --delete-after) print as -1 so the
// line always has the same keys and is easy to grep or parse in CI logs.
// It never holds the connection URL: only the engine and database name.
type execTimings struct {
	enabled          bool
	start            time.Time
	branch           string
	template         string
	engine, database string
	templateWait     time.Duration
	templated        bool
	create           time.Duration
	readyWait        time.Duration
	command          time.Duration
	delete           time.Duration
	created, waited  bool
	ran, deleted     bool
}

func (t *execTimings) note(b api.Branch) {
	if b.Engine != "" {
		t.engine = b.Engine
	}
	if b.Database != "" {
		t.database = b.Database
	}
}

func (t *execTimings) print(w io.Writer, code int) {
	if !t.enabled {
		return
	}
	ms := func(ok bool, d time.Duration) int64 {
		if !ok {
			return -1
		}
		return d.Milliseconds()
	}
	orDash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	fmt.Fprintf(w, "pgrun: timings branch=%s template=%s engine=%s database=%s template_wait_ms=%d create_ms=%d ready_wait_ms=%d command_ms=%d delete_ms=%d total_ms=%d exit=%d\n",
		t.branch, orDash(t.template), orDash(t.engine), orDash(t.database),
		ms(t.templated, t.templateWait), ms(t.created, t.create), ms(t.waited, t.readyWait), ms(t.ran, t.command), ms(t.deleted, t.delete),
		time.Since(t.start).Milliseconds(), code)
}

// signalExitCode is the shell convention for "terminated by signal s".
func signalExitCode(s os.Signal) int {
	if n, ok := s.(syscall.Signal); ok {
		return 128 + int(n)
	}
	return exitFailure
}

// execAgainstExistingBranch is the plain `branch exec <project> <name> --
// <command>` path: the branch must already exist and be ready.
func execAgainstExistingBranch(client *api.Client, project, name string, command []string, sigs <-chan os.Signal, t *execTimings, stdout, stderr io.Writer) int {
	_, branch, err := client.GetBranch(context.Background(), project, name)
	if err != nil {
		return handleAPIError(err, false, nil, stdout, stderr)
	}
	if branch.Status != api.StatusReady {
		fmt.Fprintf(stderr, "pgrun: branch %s is not ready (status: %s)\n", name, branch.Status)
		return exitFailure
	}
	if branch.ConnectionURL == "" {
		fmt.Fprintf(stderr, "pgrun: branch %s is ready but not yet credentialed\n", name)
		return exitFailure
	}
	t.note(branch)
	return t.runCommand(command, branch.ConnectionURL, sigs, stdout, stderr)
}

func (t *execTimings) runCommand(command []string, databaseURL string, sigs <-chan os.Signal, stdout, stderr io.Writer) int {
	began := time.Now()
	code := runCommandWithDatabaseURL(command, databaseURL, sigs, stdout, stderr)
	t.command, t.ran = time.Since(began), true
	return code
}

// execAgainstCreatedBranch is the `--create` path: create, wait for ready,
// run the command, and — if deleteAfter — delete the branch afterward. The
// delete is registered via defer as soon as the branch is known to exist
// (right after a successful CreateBranch), so it fires on every exit from
// that point on: a failed/timed-out wait, or any exit code from the
// command. That's stronger than the spec's minimum ("even on command
// failure") on purpose — an agent that asked for cleanup shouldn't leak a
// branch that got stuck mid-provision either; the branch's TTL is the
// backstop of last resort, not the plan.
func execAgainstCreatedBranch(client *api.Client, project, name, ttl, parent, template string, deleteAfter bool, timeout time.Duration, command []string, sigs <-chan os.Signal, t *execTimings, stdout, stderr io.Writer) (code int) {
	began := time.Now()
	raw, branch, err := client.CreateBranch(context.Background(), project, api.CreateBranchRequest{
		Name: name, TTL: ttl, ParentBranchID: parent, Template: template,
	})
	t.create, t.created = time.Since(began), true
	if err != nil {
		return handleAPIError(err, false, raw, stdout, stderr)
	}

	if deleteAfter {
		defer func() {
			began := time.Now()
			if _, _, delErr := client.DeleteBranch(context.Background(), project, name); delErr != nil {
				fmt.Fprintf(stderr, "pgrun: warning: failed to delete branch %s after exec: %v\n", name, delErr)
			}
			t.delete, t.deleted = time.Since(began), true
		}()
	}

	// A signal during the readiness wait cancels it: nothing has started
	// yet, so there is nothing to forward to — just stop and clean up.
	waitCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var interrupted os.Signal
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case s := <-sigs:
			interrupted = s
			cancel()
		case <-stopWatch:
		}
	}()
	began = time.Now()
	raw, branch, err = client.WaitForTerminal(waitCtx, project, raw, branch)
	close(stopWatch)
	<-watchDone
	t.readyWait, t.waited = time.Since(began), true
	if interrupted != nil {
		fmt.Fprintf(stderr, "pgrun: %s while waiting for branch %s; not running the command\n", interrupted, name)
		return signalExitCode(interrupted)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(stderr, "pgrun: timed out after %s waiting for branch %s to become ready (last status: %s)\n", timeout, branch.Name, branch.Status)
			return exitFailure
		}
		return handleAPIError(err, false, raw, stdout, stderr)
	}
	if branch.Status != api.StatusReady {
		fmt.Fprintf(stderr, "pgrun: %s\n", api.WaitFailureReason(branch.Name, branch.Status))
		return exitFailure
	}
	if branch.ConnectionURL == "" {
		fmt.Fprintf(stderr, "pgrun: branch %s is ready but has no connection_url yet\n", branch.Name)
		return exitFailure
	}

	t.note(branch)
	return t.runCommand(command, branch.ConnectionURL, sigs, stdout, stderr)
}

// runCommandWithDatabaseURL execs command with DATABASE_URL set in its
// environment only — never appended to argv (where it would show up in
// `ps`), never written to a file. Stdout/stderr are the writers Run was
// given (os.Stdout/os.Stderr in production, so a real terminal command
// behaves exactly as if it had been run directly); stdin is the process's
// real os.Stdin, matching how `pgrun mcp serve` also reaches past Run's
// stdout/stderr-only signature for the one thing that needs more.
//
// While the command runs, SIGTERM and SIGHUP that reach pgrun are forwarded
// to it, so a cancelled CI job stops the test suite and pgrun lives on to
// run --delete-after. SIGINT is not forwarded: the command shares pgrun's
// process group, so a terminal Ctrl-C (or a group-wide SIGINT) already
// reached it, and a second SIGINT makes tools like RSpec abort without
// their own cleanup.
func runCommandWithDatabaseURL(command []string, databaseURL string, sigs <-chan os.Signal, stdout, stderr io.Writer) int {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "DATABASE_URL="+databaseURL)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "pgrun: branch exec: %v\n", err)
		return exitFailure
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var err error
	for done := false; !done; {
		select {
		case err = <-waited:
			done = true
		case s := <-sigs:
			if s != syscall.SIGINT {
				_ = cmd.Process.Signal(s)
			}
		}
	}
	if err == nil {
		return exitSuccess
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(stderr, "pgrun: branch exec: %v\n", err)
	return exitFailure
}
