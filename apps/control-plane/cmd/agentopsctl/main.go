package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/control"
	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentopsctl: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()
	manager := newManager(cfg, lifecycle.NewAppleRuntime())
	switch args[0] {
	case "deploy":
		flags := flag.NewFlagSet("deploy", flag.ContinueOnError)
		timeout := flags.Duration("timeout", 10*time.Minute, "maximum graceful drain wait")
		requestID := flags.String("request-id", "", "durable staged-deployment identity")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *timeout <= 0 {
			return usageError()
		}
		return manager.DeployActive(
			ctx,
			*timeout,
			commandID("deploy", *requestID),
		)
	case "start":
		flags := flag.NewFlagSet("start", flag.ContinueOnError)
		modeRaw := flags.String("mode", "MONITOR_ONLY", "MONITOR_ONLY or ACTIVE")
		build := flags.Bool("build", false, "build the standard OCI images first")
		requestID := flags.String("request-id", "", "durable idempotency identity")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return usageError()
		}
		mode, err := lifecycle.ParseMode(*modeRaw)
		if err != nil {
			return err
		}
		if mode != lifecycle.ModeMonitorOnly && mode != lifecycle.ModeActive {
			return fmt.Errorf("start mode must be MONITOR_ONLY or ACTIVE")
		}
		return manager.Start(ctx, mode, *build, commandID("start", *requestID))
	case "drain":
		flags := flag.NewFlagSet("drain", flag.ContinueOnError)
		timeout := flags.Duration("timeout", 10*time.Minute, "maximum graceful drain wait")
		requestID := flags.String("request-id", "", "durable idempotency identity")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *timeout <= 0 {
			return usageError()
		}
		return manager.Drain(ctx, *timeout, commandID("drain", *requestID))
	case "stop":
		flags := flag.NewFlagSet("stop", flag.ContinueOnError)
		timeout := flags.Duration("timeout", 10*time.Minute, "maximum graceful drain wait")
		requestID := flags.String("request-id", "", "durable idempotency identity")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *timeout <= 0 {
			return usageError()
		}
		return manager.Stop(ctx, *timeout, commandID("stop", *requestID))
	case "rotate-postgres-admin":
		flags := flag.NewFlagSet("rotate-postgres-admin", flag.ContinueOnError)
		requestID := flags.String("request-id", "", "durable rotation identity")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return usageError()
		}
		return manager.RotatePostgresAdmin(
			ctx,
			commandID("rotate-postgres-admin", *requestID),
		)
	case "migrate-labels":
		flags := flag.NewFlagSet("migrate-labels", flag.ContinueOnError)
		// The read-only inventory is the default spelling because the mutating
		// form deletes and recreates containers.
		apply := flags.Bool(
			"apply",
			false,
			"migrate old-only containers instead of only inventorying them",
		)
		evidenceDir := flags.String(
			"evidence-dir",
			"",
			"where to write the durable inventory and sweep records",
		)
		only := flags.String(
			"only",
			"",
			"comma separated exact container identities to migrate; "+
				"required with --apply",
		)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return usageError()
		}
		return manager.MigrateLabels(
			ctx, *apply, *evidenceDir, splitIdentities(*only),
		)
	case "status":
		flags := flag.NewFlagSet("status", flag.ContinueOnError)
		asJSON := flags.Bool("json", false, "emit machine-readable JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return usageError()
		}
		status, err := manager.Status(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(status)
		}
		printStatus(status)
		return nil
	case "progress":
		flags := flag.NewFlagSet("progress", flag.ContinueOnError)
		asJSON := flags.Bool("json", false, "emit machine-readable JSON")
		limit := flags.Int("limit", 50, "maximum events")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || *limit < 1 || *limit > 200 {
			return usageError()
		}
		repository, issueNumber, err := parseProgressTarget(flags.Arg(0))
		if err != nil {
			return err
		}
		report, err := manager.Progress(ctx, repository, issueNumber, *limit)
		if err != nil {
			return err
		}
		if *asJSON {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(report)
		}
		printProgress(report, repository, issueNumber)
		return nil
	case "worktree":
		flags := flag.NewFlagSet("worktree", flag.ContinueOnError)
		showDiff := flags.Bool("diff", false, "also show the retained worktree diff")
		openShell := flags.Bool("shell", false, "open an interactive shell in the retained worktree")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return usageError()
		}
		repository, issueNumber, err := parseProgressTarget(flags.Arg(0))
		if err != nil {
			return err
		}
		return manager.Worktree(ctx, repository, issueNumber, *showDiff, *openShell)
	case "logs":
		flags := flag.NewFlagSet("logs", flag.ContinueOnError)
		component := flags.String(
			"component",
			"control",
			"control, github-broker, triage, runner, or postgres",
		)
		follow := flags.Bool("follow", false, "follow log output")
		lines := flags.Int("lines", 200, "tail line count")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *lines < 1 {
			return usageError()
		}
		return manager.Logs(ctx, *component, *lines, *follow)
	case "open":
		flags := flag.NewFlagSet("open", flag.ContinueOnError)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return usageError()
		}
		return manager.Open(ctx)
	default:
		return usageError()
	}
}

func commandID(operation, provided string) string {
	if value := strings.TrimSpace(provided); value != "" {
		return value
	}
	return fmt.Sprintf(
		"%s-%d-%d",
		operation,
		time.Now().UTC().UnixNano(),
		os.Getpid(),
	)
}

func parseProgressTarget(value string) (string, int64, error) {
	repository, rawIssue, present := strings.Cut(strings.TrimSpace(value), "#")
	repository = strings.ToLower(strings.TrimSpace(repository))
	if !present || strings.Contains(rawIssue, "#") ||
		!control.ValidRepositoryIdentity(repository) {
		return "", 0, fmt.Errorf("progress target must be canonical owner/name#issue")
	}
	issue, err := strconv.ParseInt(strings.TrimSpace(rawIssue), 10, 64)
	if err != nil || issue < 1 {
		return "", 0, fmt.Errorf("progress target must include a positive Issue number")
	}
	return repository, issue, nil
}

func usageError() error {
	return fmt.Errorf(
		"usage: agentopsctl deploy|start|drain|stop|rotate-postgres-admin|migrate-labels|status|progress|worktree|logs|open (use -h after a command)",
	)
}
