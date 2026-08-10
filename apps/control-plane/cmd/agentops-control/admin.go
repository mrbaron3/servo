package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/control"
	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

var rotatePostgresAdmin = lifecycle.RotatePostgresAdmin

func runAdministrativeCommand(args []string) error {
	databaseURL := strings.TrimSpace(os.Getenv("AGENTOPS_DATABASE_URL"))
	if databaseURL == "" {
		return fmt.Errorf("AGENTOPS_DATABASE_URL is required")
	}
	root, err := applicationRoot()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	switch args[0] {
	case "migrate":
		if len(args) != 1 {
			return fmt.Errorf("usage: agentops-control migrate")
		}
		if err := control.MigrateSchema(ctx, databaseURL, root); err != nil {
			return err
		}
		return writeJSON(map[string]any{
			"schemaVersion": control.ControlSchemaVersion,
			"migrated":      true,
		})
	case "bootstrap-roles":
		if len(args) != 1 {
			return fmt.Errorf("usage: agentops-control bootstrap-roles")
		}
		if err := lifecycle.BootstrapRoles(
			ctx,
			databaseURL,
			os.Getenv("AGENTOPS_CONTROL_DB_PASSWORD"),
			os.Getenv("AGENTOPS_TRIAGE_DB_PASSWORD"),
			os.Getenv("AGENTOPS_RUNNER_DB_PASSWORD"),
		); err != nil {
			return err
		}
		return writeJSON(map[string]any{"rolesBootstrapped": true})
	case "rotate-postgres-admin":
		flags := flag.NewFlagSet("rotate-postgres-admin", flag.ContinueOnError)
		requestID := flags.String("request-id", "", "durable rotation identity")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected rotate-postgres-admin arguments")
		}
		if strings.TrimSpace(*requestID) == "" {
			return fmt.Errorf("rotate-postgres-admin request ID is required")
		}
		if err := rotatePostgresAdmin(
			ctx,
			databaseURL,
			os.Getenv("AGENTOPS_NEXT_POSTGRES_PASSWORD"),
			*requestID,
		); err != nil {
			return err
		}
		return writeJSON(map[string]any{
			"rotated":   true,
			"requestId": *requestID,
		})
	case "progress":
		flags := flag.NewFlagSet("progress", flag.ContinueOnError)
		repository := flags.String("repository", "", "canonical owner/name")
		issue := flags.Int64("issue", 0, "GitHub Issue number")
		limit := flags.Int("limit", 50, "maximum events")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		canonical := strings.ToLower(strings.TrimSpace(*repository))
		if flags.NArg() != 0 || !control.ValidRepositoryIdentity(canonical) ||
			*issue < 0 || *limit < 1 || *limit > 200 {
			return fmt.Errorf(
				"usage: agentops-control progress --repository owner/name [--issue N] [--limit 1..200]",
			)
		}
		store, err := openStandardControlStore(ctx, databaseURL, root)
		if err != nil {
			return err
		}
		defer store.Close()
		var issueNumber *int64
		if *issue > 0 {
			issueNumber = issue
		}
		events, err := store.DevelopmentProgress(
			ctx,
			canonical,
			issueNumber,
			*limit,
		)
		if err != nil {
			return err
		}
		return writeJSON(map[string]any{
			"items":      events,
			"observedAt": time.Now().UTC(),
		})
	case "lifecycle":
		return runLifecycleCommand(ctx, databaseURL, args[1:])
	default:
		return fmt.Errorf("unknown agentops-control command %q", args[0])
	}
}

func runLifecycleCommand(
	ctx context.Context,
	databaseURL string,
	args []string,
) error {
	if len(args) == 0 {
		return fmt.Errorf(
			"usage: agentops-control lifecycle status|transition|failure|reconcile-expired",
		)
	}
	store, err := lifecycle.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return fmt.Errorf("usage: agentops-control lifecycle status")
		}
		status, err := store.Status(ctx)
		if err != nil {
			return err
		}
		return writeJSON(status)
	case "transition":
		flags := flag.NewFlagSet("lifecycle transition", flag.ContinueOnError)
		toRaw := flags.String("to", "", "target lifecycle mode")
		key := flags.String("idempotency-key", "", "durable command identity")
		actor := flags.String("actor", "agentopsctl", "audit actor")
		deadlineRaw := flags.String("drain-deadline", "", "RFC3339 drain deadline")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected lifecycle transition arguments")
		}
		to, err := lifecycle.ParseMode(*toRaw)
		if err != nil {
			return err
		}
		var deadline *time.Time
		if strings.TrimSpace(*deadlineRaw) != "" {
			value, err := time.Parse(time.RFC3339Nano, *deadlineRaw)
			if err != nil {
				return fmt.Errorf("invalid drain deadline: %w", err)
			}
			value = value.UTC()
			deadline = &value
		}
		transition, state, err := store.Transition(
			ctx,
			*actor,
			*key,
			to,
			deadline,
			map[string]any{"source": "agentopsctl"},
		)
		if err != nil {
			_ = writeJSON(map[string]any{
				"transition": transition,
				"state":      state,
				"error":      err.Error(),
			})
			return err
		}
		return writeJSON(map[string]any{
			"transition": transition,
			"state":      state,
		})
	case "failure":
		flags := flag.NewFlagSet("lifecycle failure", flag.ContinueOnError)
		actor := flags.String("actor", "agentopsctl", "audit actor")
		operation := flags.String("operation", "", "failed operation")
		message := flags.String("message", "", "redacted failure message")
		timedOut := flags.Bool("drain-timeout", false, "mark drain timeout")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected lifecycle failure arguments")
		}
		if err := store.RecordFailure(
			ctx,
			*actor,
			*operation,
			*message,
			*timedOut,
			map[string]any{"source": "agentopsctl"},
		); err != nil {
			return err
		}
		return writeJSON(map[string]any{"recorded": true})
	case "reconcile-expired":
		flags := flag.NewFlagSet("lifecycle reconcile-expired", flag.ContinueOnError)
		maxAttempts := flags.Int(
			"max-attempts",
			lifecycle.DefaultReconcileMaxAttempts,
			"terminal attempt ceiling",
		)
		retryBase := flags.Duration(
			"retry-base",
			lifecycle.DefaultReconcileRetryBase,
			"retry backoff base",
		)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected lifecycle reconciliation arguments")
		}
		reconciled, err := store.ReconcileExpiredRunnerWork(
			ctx,
			*maxAttempts,
			*retryBase,
		)
		if err != nil {
			return err
		}
		return writeJSON(map[string]any{"reconciled": reconciled})
	default:
		return fmt.Errorf("unknown lifecycle command %q", args[0])
	}
}

func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
