// This file implements `pgrun source copy`: the last step of Connect
// Postgres -> Protect Data -> Create Safe Copy. It consumes Task 1's
// GetSource/CopySource/WaitForSource/api.CopySettled and Task 2's
// sourceNameArgs/sourceNext.
//
// The hard rule throughout (see the task brief's global constraints): the
// server is the sole authority on whether a Safe Copy may be created. Every
// pre-flight message here is derived from the source object purely to avoid
// a pointless round trip and give a better message than a raw 409/422 would
// — it is never a client-side override of what the server would decide.
// action_required in particular never reaches CopySource: the four-line
// drift message is printed and the command exits before any POST. Every
// refusal the server itself makes on the POST (409/422) is surfaced
// verbatim through handleAPIError, exactly like every other source command.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/pgrundev/pgrun-cli/internal/api"
)

func sourceCopy(args []string, stdout, stderr io.Writer) int {
	name, rest, code, ok := sourceNameArgs("copy", args, stderr)
	if !ok {
		return code
	}

	fs := flag.NewFlagSet("source copy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wait := fs.Bool("wait", false, "poll until the Safe Copy is ready or failed")
	timeout := fs.Duration("timeout", 30*time.Minute, "max time to wait with --wait")
	jsonFlag := addJSONFlag(fs)
	apiURLFlag, tokenFlag := addSourceAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "source copy: unexpected argument %s", redactedArg(fs.Args()[0]))
	}
	jsonOut := *jsonFlag

	cfg, code, ok := resolveOrHint(*apiURLFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)
	ctx := context.Background()

	raw, src, err := client.GetSource(ctx, name)
	if err != nil {
		return handleAPIError(err, jsonOut, raw, stdout, stderr)
	}

	switch src.SafeCopyStatus {
	case api.SourceActionRequired:
		// Production schema drifted after the policy was approved — fail
		// closed, no POST. The customer's fix is to review protect, never
		// something this command can paper over.
		if jsonOut {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintf(stderr, "pgrun: Safe Copy cannot be created.\n\nProduction schema changed after the protection policy was approved.\n\nReview changes:\n  pgrun source protect %s\n", name)
		return exitFailure

	case api.SourceReady:
		if jsonOut {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintf(stderr, "pgrun: %s already has a Safe Copy\n", name)
		return exitFailure

	case api.SourceFailed:
		if jsonOut {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintf(stderr, "pgrun: the Safe Copy for %s failed — write to support@postgresrun.com\n", name)
		return exitFailure

	case api.SourceConnect, api.SourceChecking, api.SourceProtect:
		if jsonOut {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintf(stderr, "pgrun: finish data protection first — %s\n", sourceNext(src))
		return exitFailure

	case api.SourceCopying:
		if !*wait {
			if jsonOut {
				dumpJSON(stdout, raw)
				return exitSuccess
			}
			fmt.Fprintf(stdout, "→ Safe Copy for %s is being created\n\n", name)
			fmt.Fprintln(stdout, "Next:")
			fmt.Fprintf(stdout, "  pgrun source status %s\n", name)
			return exitSuccess
		}
		return sourceCopyWait(ctx, client, name, *timeout, jsonOut, raw, src, stdout, stderr)

	case api.SourceReadyToCopy:
		if !jsonOut {
			fmt.Fprintln(stdout, "✓ Connection verified")
			fmt.Fprintf(stdout, "✓ Data protection active (policy v%d)\n", src.PolicyVersion)
			// Read off schema_changed rather than assumed from
			// ready_to_copy: if a server ever reports that pair
			// inconsistently, the checklist says what the source object
			// actually claims instead of asserting something reassuring. The
			// POST still goes ahead — ready_to_copy is the server's word, and
			// it has the final say on the request itself.
			if src.SchemaChanged {
				fmt.Fprintln(stdout, "! Production schema changed")
			} else {
				fmt.Fprintln(stdout, "✓ Schema unchanged")
			}
		}
		// No return here (unlike every other case): execution continues past
		// the switch to the POST below.

	default:
		// Fail closed rather than POST for a status this command doesn't
		// recognize — better an actionable refusal than a surprise request.
		if jsonOut {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintf(stderr, "pgrun: %s is not ready for a Safe Copy (status: %s) — %s\n", name, src.SafeCopyStatus, sourceNext(src))
		return exitFailure
	}

	// ready_to_copy: the server is authoritative from here — 409/422 go
	// through handleAPIError exactly like any other source command.
	raw, src, err = client.CopySource(ctx, name)
	if err != nil {
		return handleAPIError(err, jsonOut, raw, stdout, stderr)
	}
	if !jsonOut {
		fmt.Fprintln(stdout, "→ Creating Safe Copy")
	}

	if !*wait {
		if jsonOut {
			dumpJSON(stdout, raw)
			return exitSuccess
		}
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Next:")
		fmt.Fprintf(stdout, "  pgrun source status %s\n", name)
		return exitSuccess
	}

	return sourceCopyWait(ctx, client, name, *timeout, jsonOut, raw, src, stdout, stderr)
}

// sourceCopyWait polls until the Safe Copy settles (api.CopySettled): ready
// is the one successful outcome, failed is a support case, running out of
// timeout says so without pretending either way, and any other terminal
// status (action_required, landed mid-copy) gets the generic "ended in"
// message. --json dumps the last raw body on every non-zero path, exactly
// like branch create --wait.
func sourceCopyWait(ctx context.Context, client *api.Client, name string, timeout time.Duration, jsonOut bool, raw []byte, src api.Source, stdout, stderr io.Writer) int {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, src, err := client.WaitForSource(waitCtx, name, raw, src, api.CopySettled)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			if jsonOut {
				dumpJSON(stdout, raw)
			}
			fmt.Fprintf(stderr, "pgrun: still creating the Safe Copy for %s after %s — run `pgrun source status %s`\n", name, timeout, name)
			return exitFailure
		}
		return handleAPIError(err, jsonOut, raw, stdout, stderr)
	}

	switch src.SafeCopyStatus {
	case api.SourceReady:
		if jsonOut {
			dumpJSON(stdout, raw)
			return exitSuccess
		}
		fmt.Fprintln(stdout, "✓ Safe Copy ready")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Next:")
		fmt.Fprintf(stdout, "  pgrun branch create %s --name dev --wait\n", name)
		return exitSuccess

	case api.SourceFailed:
		if jsonOut {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintln(stderr, "pgrun: ✗ Safe Copy failed — write to support@postgresrun.com")
		return exitFailure

	default:
		if jsonOut {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintf(stderr, "pgrun: Safe Copy ended in %s — run `pgrun source status %s`\n", src.SafeCopyStatus, name)
		return exitFailure
	}
}
