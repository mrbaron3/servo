package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

type MonitorStore interface {
	ManagesComponent(string) bool
	LifecycleMode(context.Context) (lifecycle.Mode, error)
	MonitorCursor(context.Context, string, string) (map[string]any, error)
	SaveMonitorCursor(context.Context, string, string, map[string]any, time.Time) error
	EnqueueWork(context.Context, Registration, string, string, WorkItem) (string, bool, error)
	ReceiveWebhook(
		context.Context,
		string,
		string,
		string,
		*string,
		map[string]string,
		map[string]any,
	) (WebhookReceipt, error)
	UpsertActualState(
		context.Context,
		Registration,
		string,
		string,
		string,
		error,
	) error
}

type MonitorSource interface {
	Poll(
		context.Context,
		Registration,
		string,
		map[string]any,
	) ([]WorkItem, map[string]any, time.Time, error)
}

type Command interface {
	StdoutPipe() (io.ReadCloser, error)
	StderrPipe() (io.ReadCloser, error)
	Start() error
	Wait() error
}

type CommandFactory func(context.Context, string, ...string) Command

type ProductionRunner struct {
	Store          MonitorStore
	Source         MonitorSource
	SupervisorID   string
	PollInterval   time.Duration
	TransientRetry time.Duration
	ForwarderRetry time.Duration
	HealthInterval time.Duration
	Command        CommandFactory
	Log            *slog.Logger
}

func (runner *ProductionRunner) Run(
	ctx context.Context,
	registration Registration,
	component string,
) error {
	switch component {
	case ComponentIssueMonitor:
		return runner.runMonitor(ctx, registration, "issue", component)
	case ComponentPRMonitor:
		return runner.runMonitor(ctx, registration, "pull_request", component)
	case ComponentForwarder:
		return runner.runForwarder(ctx, registration)
	default:
		return fmt.Errorf("unknown component %s", component)
	}
}

func (runner *ProductionRunner) runMonitor(
	ctx context.Context,
	registration Registration,
	kind, component string,
) error {
	ticker := time.NewTicker(runner.PollInterval)
	defer ticker.Stop()
	transientRetries := 0
	for {
		if err := runner.pollOnce(ctx, registration, kind, component); err != nil {
			if errors.Is(err, ErrMonitorBrokerTransientProvider) &&
				transientRetries < MaxMonitorBrokerTransientRetry {
				transientRetries++
				if stateErr := runner.Store.UpsertActualState(
					ctx,
					registration,
					component,
					"starting",
					runner.SupervisorID,
					nil,
				); stateErr != nil {
					return stateErr
				}
				retry := runner.TransientRetry
				if retry <= 0 {
					retry = 2 * time.Second
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(retry):
					continue
				}
			}
			return err
		}
		transientRetries = 0
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (runner *ProductionRunner) pollOnce(
	ctx context.Context,
	registration Registration,
	kind, component string,
) error {
	cursor, err := runner.Store.MonitorCursor(ctx, registration.ID, kind)
	if err != nil {
		return err
	}
	items, nextCursor, observedAt, err := runner.Source.Poll(ctx, registration, kind, cursor)
	if err != nil {
		return err
	}
	mode, err := runner.Store.LifecycleMode(ctx)
	if err != nil {
		return err
	}
	if !registration.ExecutionEnabled || mode != lifecycle.ModeActive {
		// This cursor is the durable processing boundary, not merely a read
		// watermark. Advancing it while execution is disabled or MONITOR_ONLY
		// would permanently skip work observed before the next ACTIVE turn.
		// Keep polling for freshness, but leave the cursor frozen so ACTIVE
		// deterministically re-observes and enqueues the same identities.
		return runner.Store.UpsertActualState(
			ctx,
			registration,
			component,
			"running",
			runner.SupervisorID,
			nil,
		)
	}
	for _, item := range items {
		_, _, err := runner.Store.EnqueueWork(
			ctx,
			registration,
			"poll",
			item.IdempotencyKey(),
			item,
		)
		if err != nil {
			if errors.Is(err, ErrRepositoryBusy) ||
				errors.Is(err, ErrOperatingMode) {
				return runner.Store.UpsertActualState(
					ctx,
					registration,
					component,
					"running",
					runner.SupervisorID,
					nil,
				)
			}
			return err
		}
	}
	if err := runner.Store.SaveMonitorCursor(
		ctx,
		registration.ID,
		kind,
		nextCursor,
		observedAt,
	); err != nil {
		return err
	}
	return runner.Store.UpsertActualState(
		ctx,
		registration,
		component,
		"running",
		runner.SupervisorID,
		nil,
	)
}

func (runner *ProductionRunner) runForwarder(
	ctx context.Context,
	registration Registration,
) error {
	events := forwarderEvents(registration)
	if len(events) == 0 {
		return nil
	}
	if !runner.Store.ManagesComponent(ComponentForwarder) {
		<-ctx.Done()
		return nil
	}
	commandFactory := runner.Command
	if commandFactory == nil {
		commandFactory = func(ctx context.Context, name string, args ...string) Command {
			command := exec.CommandContext(ctx, name, args...)
			command.Env = sanitizedForwarderEnvironment(os.Environ())
			return command
		}
	}
	for {
		commandContext, cancelCommand := context.WithCancel(ctx)
		command := commandFactory(
			commandContext,
			"gh",
			"webhook",
			"forward",
			"--repo="+registration.Repository,
			"--events="+strings.Join(events, ","),
		)
		stdout, err := command.StdoutPipe()
		if err != nil {
			cancelCommand()
			return err
		}
		stderr, err := command.StderrPipe()
		if err != nil {
			cancelCommand()
			return err
		}
		if err := command.Start(); err != nil {
			cancelCommand()
			return err
		}
		errorLines := make(chan string, 1)
		go collectLastLine(stderr, errorLines)
		if err := runner.Store.UpsertActualState(
			ctx,
			registration,
			ComponentForwarder,
			"running",
			runner.SupervisorID,
			nil,
		); err != nil {
			cancelCommand()
			_ = command.Wait()
			<-errorLines
			return err
		}
		healthContext, cancelHealth := context.WithCancel(commandContext)
		healthDone := make(chan error, 1)
		go func() {
			healthDone <- runner.heartbeatForwarder(healthContext, registration)
		}()
		scanDone := make(chan error, 1)
		go func() {
			scanDone <- runner.scanForwarder(commandContext, registration, stdout)
		}()
		var scanErr, healthErr error
		select {
		case scanErr = <-scanDone:
			cancelHealth()
			healthErr = <-healthDone
		case healthErr = <-healthDone:
			if healthErr != nil {
				cancelCommand()
			}
			scanErr = <-scanDone
		case <-ctx.Done():
			cancelCommand()
			scanErr = <-scanDone
			cancelHealth()
			healthErr = <-healthDone
		}
		waitErr := command.Wait()
		cancelHealth()
		cancelCommand()
		if ctx.Err() != nil {
			return nil
		}
		lastError := <-errorLines
		if healthErr != nil {
			return healthErr
		}
		if scanErr != nil {
			return scanErr
		}
		exitError := waitErr
		if exitError == nil {
			exitError = errors.New("forwarder exited without an error status")
		}
		if err := runner.Store.UpsertActualState(
			ctx,
			registration,
			ComponentForwarder,
			"failed",
			runner.SupervisorID,
			exitError,
		); err != nil {
			return err
		}
		runner.Log.Warn(
			"GitHub forwarder exited; restarting",
			"repository", registration.Repository,
			"error", waitErr,
			"stderr", lastError,
		)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(runner.ForwarderRetry):
		}
		if err := runner.Store.UpsertActualState(
			ctx,
			registration,
			ComponentForwarder,
			"starting",
			runner.SupervisorID,
			nil,
		); err != nil {
			return err
		}
	}
}

func sanitizedForwarderEnvironment(environment []string) []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"GH_CONFIG_DIR": true, "GH_TOKEN": true, "GITHUB_TOKEN": true,
		"GH_ENTERPRISE_TOKEN": true, "GH_HOST": true, "HTTP_PROXY": true,
		"HTTPS_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true,
		"SSL_CERT_DIR": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"SSH_AUTH_SOCK": true,
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, present := strings.Cut(entry, "=")
		if present && allowed[name] {
			result = append(result, entry)
		}
	}
	return result
}

func (runner *ProductionRunner) heartbeatForwarder(
	ctx context.Context,
	registration Registration,
) error {
	interval := runner.HealthInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := runner.Store.UpsertActualState(
				ctx,
				registration,
				ComponentForwarder,
				"running",
				runner.SupervisorID,
				nil,
			); err != nil {
				return fmt.Errorf("persist forwarder health: %w", err)
			}
		}
	}
}

func (runner *ProductionRunner) scanForwarder(
	ctx context.Context,
	registration Registration,
	output io.Reader,
) error {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err != nil {
			runner.Log.Debug("ignored non-payload forwarder output", "repository", registration.Repository)
			continue
		}
		repository, err := webhookRepository(payload)
		if err != nil || repository != registration.Repository {
			return fmt.Errorf("forwarder repository identity mismatch")
		}
		event := inferWebhookEvent(payload)
		if event == "" || !eventAllowed(registration, event) {
			runner.Log.Warn(
				"ignored unrecognized or disabled forwarder event",
				"repository", registration.Repository,
			)
			continue
		}
		digest := sha256.New()
		_, _ = digest.Write([]byte(registration.Repository))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(event))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write(line)
		deliveryKey := "pipe-" + hex.EncodeToString(digest.Sum(nil))
		action := actionFromPayload(payload)
		if _, err := runner.Store.ReceiveWebhook(
			ctx,
			deliveryKey,
			registration.Repository,
			event,
			action,
			map[string]string{
				"content-type":      "application/json",
				"x-github-delivery": deliveryKey,
				"x-github-event":    event,
			},
			payload,
		); err != nil {
			return err
		}
	}
	return scanner.Err()
}

type GitHubSource struct {
	Client  *http.Client
	BaseURL string
	Token   string
}

func (source GitHubSource) Poll(
	ctx context.Context,
	registration Registration,
	kind string,
	cursor map[string]any,
) ([]WorkItem, map[string]any, time.Time, error) {
	baseURL := strings.TrimSuffix(source.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	endpoint := "issues"
	if kind == "pull_request" {
		endpoint = "pulls"
	}
	updatedAfter, _ := cursor["updatedAfter"].(string)
	var cursorTime time.Time
	if updatedAfter != "" {
		parsed, err := time.Parse(time.RFC3339Nano, updatedAfter)
		if err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("monitor cursor has invalid updatedAfter")
		}
		cursorTime = parsed
	}
	pageURL := fmt.Sprintf(
		"%s/repos/%s/%s?state=open&sort=updated&direction=asc&per_page=100",
		baseURL,
		registration.Repository,
		endpoint,
	)
	if endpoint == "issues" && updatedAfter != "" {
		pageURL += "&since=" + url.QueryEscape(updatedAfter)
	}
	observedAt := time.Now().UTC()
	maxUpdated := cursorTime
	var items []WorkItem
	for pageURL != "" {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if source.Token != "" {
			request.Header.Set("Authorization", "Bearer "+source.Token)
		}
		response, err := source.Client.Do(request)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024))
		_ = response.Body.Close()
		if readErr != nil {
			return nil, nil, time.Time{}, readErr
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, nil, time.Time{}, fmt.Errorf(
				"GitHub %s poll returned %s: %s",
				kind,
				response.Status,
				strings.TrimSpace(string(body)),
			)
		}
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, nil, time.Time{}, err
		}
		for _, row := range rows {
			if kind == "issue" {
				if _, pullRequest := row["pull_request"]; pullRequest {
					continue
				}
			}
			number, ok := jsonNumber(row["number"])
			if !ok {
				return nil, nil, time.Time{}, fmt.Errorf("GitHub item has no numeric identity")
			}
			updatedRaw, _ := row["updated_at"].(string)
			updatedAt, err := time.Parse(time.RFC3339, updatedRaw)
			if err != nil {
				return nil, nil, time.Time{}, fmt.Errorf("GitHub item has invalid updated_at")
			}
			if !cursorTime.IsZero() && updatedAt.Before(cursorTime) {
				continue
			}
			entityKind, supported := canonicalWorkItemKind(kind)
			if !supported {
				return nil, nil, time.Time{}, fmt.Errorf("unsupported monitor entity kind %q", kind)
			}
			items = append(items, WorkItem{
				Repository: registration.Repository,
				Kind:       entityKind,
				Number:     number,
				UpdatedAt:  updatedAt,
			})
			if maxUpdated.IsZero() || updatedAt.After(maxUpdated) {
				maxUpdated = updatedAt
			}
		}
		pageURL = nextLink(response.Header.Get("Link"))
	}
	nextUpdatedAfter := ""
	if !maxUpdated.IsZero() {
		nextUpdatedAfter = maxUpdated.UTC().Format(time.RFC3339Nano)
	}
	return items, map[string]any{"updatedAfter": nextUpdatedAfter}, observedAt, nil
}

func forwarderEvents(registration Registration) []string {
	return []string{
		"issues",
		"issue_comment",
		"pull_request",
		"pull_request_review",
		"pull_request_review_comment",
		"check_run",
		"check_suite",
		"push",
	}
}

func collectLastLine(reader io.Reader, result chan<- string) {
	defer close(result)
	scanner := bufio.NewScanner(reader)
	last := ""
	for scanner.Scan() {
		last = scanner.Text()
	}
	result <- last
}

func webhookRepository(payload map[string]any) (string, error) {
	repository, ok := payload["repository"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("payload repository is missing")
	}
	fullName, ok := repository["full_name"].(string)
	if !ok {
		return "", fmt.Errorf("payload repository.full_name is missing")
	}
	fullName = strings.ToLower(strings.TrimSpace(fullName))
	if !safeRepositoryIdentity(fullName) {
		return "", fmt.Errorf("payload repository.full_name is invalid")
	}
	return fullName, nil
}

func actionFromPayload(payload map[string]any) *string {
	action, ok := payload["action"].(string)
	if !ok || strings.TrimSpace(action) == "" {
		return nil
	}
	return &action
}

func inferWebhookEvent(payload map[string]any) string {
	if _, ok := payload["check_run"].(map[string]any); ok {
		return "check_run"
	}
	if _, ok := payload["check_suite"].(map[string]any); ok {
		return "check_suite"
	}
	if _, ok := payload["pull_request"].(map[string]any); ok {
		if _, comment := payload["comment"].(map[string]any); comment {
			return "pull_request_review_comment"
		}
		if _, review := payload["review"].(map[string]any); review {
			return "pull_request_review"
		}
		return "pull_request"
	}
	if _, ok := payload["issue"].(map[string]any); ok {
		if _, comment := payload["comment"].(map[string]any); comment {
			return "issue_comment"
		}
		return "issues"
	}
	if _, ref := payload["ref"].(string); ref {
		if _, before := payload["before"].(string); before {
			if _, after := payload["after"].(string); after {
				return "push"
			}
		}
	}
	return ""
}

func eventAllowed(registration Registration, event string) bool {
	switch event {
	case "issues", "issue_comment":
		return registration.Enabled && registration.IssueMonitorEnabled
	default:
		return registration.Enabled && registration.PRMonitorEnabled
	}
}

func jsonNumber(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		return int64(number), number == float64(int64(number))
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	case int64:
		return number, true
	case int:
		return int64(number), true
	case string:
		parsed, err := strconv.ParseInt(number, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		segments := strings.Split(strings.TrimSpace(part), ";")
		if len(segments) < 2 || !strings.Contains(segments[1], `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(segments[0]), "<>")
	}
	return ""
}

var _ Command = (*exec.Cmd)(nil)
