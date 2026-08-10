package control

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

const (
	ComponentIssueMonitor = "issue_monitor"
	ComponentPRMonitor    = "pr_monitor"
	ComponentForwarder    = "forwarder"
	ComponentExecution    = "execution"
	ComponentQueue        = "queue"
)

var repositoryPattern = regexp.MustCompile(
	`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9_.-]{1,100}$`,
)
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var uuidPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)

var (
	ErrNotFound            = errors.New("not found")
	ErrConflict            = errors.New("conflict")
	ErrRepositoryBusy      = errors.New("repository already has active work")
	ErrStaleRegistration   = errors.New("registration is stale or disabled")
	ErrStoreUnavailable    = errors.New("control store unavailable")
	ErrOperatingMode       = errors.New("operating mode does not permit execution")
	ErrIdempotencyConflict = errors.New("idempotency key was reused for a different request")
	ErrNoChange            = errors.New("registration patch does not change desired state")
)

type RegistrationCommandRejection struct {
	Cause      error
	Reason     string
	RecordedAt time.Time
}

func (rejection *RegistrationCommandRejection) Error() string {
	return rejection.Cause.Error()
}

func (rejection *RegistrationCommandRejection) Unwrap() error {
	return rejection.Cause
}

type Registration struct {
	ID                  string `json:"id"`
	Repository          string `json:"repository"`
	Enabled             bool   `json:"enabled"`
	IssueMonitorEnabled bool   `json:"issueMonitorEnabled"`
	PRMonitorEnabled    bool   `json:"prMonitorEnabled"`
	ExecutionEnabled    bool   `json:"executionEnabled"`
	// Configuration is persisted desired state. Readers preserve bounded
	// historical grader aliases; create and patch writers enforce the current
	// canonical contract, so arbitrary commands, credentials, paths, and
	// environment remain unrepresentable.
	Configuration json.RawMessage `json:"configuration"`
	Version       int64           `json:"version"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// MergeMethod returns the Registration-owned integration strategy. Omission
// preserves the historical squash behavior without making workspace config a
// second authority for control-plane jobs.
func (registration Registration) MergeMethod() string {
	var configuration struct {
		MergeMethod string `json:"mergeMethod"`
	}
	if json.Unmarshal(registration.Configuration, &configuration) != nil ||
		configuration.MergeMethod == "" {
		return "squash"
	}
	return configuration.MergeMethod
}

type CreateRegistration struct {
	Repository          string          `json:"repository"`
	Enabled             *bool           `json:"enabled,omitempty"`
	IssueMonitorEnabled *bool           `json:"issueMonitorEnabled,omitempty"`
	PRMonitorEnabled    *bool           `json:"prMonitorEnabled,omitempty"`
	ExecutionEnabled    *bool           `json:"executionEnabled,omitempty"`
	Configuration       json.RawMessage `json:"configuration,omitempty"`
}

type RegistrationPatch struct {
	Enabled             *bool           `json:"enabled,omitempty"`
	IssueMonitorEnabled *bool           `json:"issueMonitorEnabled,omitempty"`
	PRMonitorEnabled    *bool           `json:"prMonitorEnabled,omitempty"`
	ExecutionEnabled    *bool           `json:"executionEnabled,omitempty"`
	Configuration       json.RawMessage `json:"configuration,omitempty"`
}

func (input CreateRegistration) Validated() (Registration, error) {
	repository := strings.ToLower(strings.TrimSpace(input.Repository))
	if !safeRepositoryIdentity(repository) {
		return Registration{}, fmt.Errorf("repository must be canonical owner/name")
	}
	enabled, issue, pr, execution := true, true, true, true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	if input.IssueMonitorEnabled != nil {
		issue = *input.IssueMonitorEnabled
	}
	if input.PRMonitorEnabled != nil {
		pr = *input.PRMonitorEnabled
	}
	if input.ExecutionEnabled != nil {
		execution = *input.ExecutionEnabled
	}
	configuration := input.Configuration
	if len(configuration) == 0 {
		configuration = json.RawMessage(`{}`)
	}
	if !validJSONObject(configuration) {
		return Registration{}, fmt.Errorf("configuration must be a JSON object")
	}
	return Registration{
		Repository: repository, Enabled: enabled,
		IssueMonitorEnabled: issue, PRMonitorEnabled: pr,
		ExecutionEnabled: execution, Configuration: configuration,
	}, nil
}

func safeRepositoryIdentity(repository string) bool {
	if !repositoryPattern.MatchString(repository) {
		return false
	}
	_, name, present := strings.Cut(repository, "/")
	return present && name != "." && name != ".."
}

// ValidRepositoryIdentity exposes the same canonical GitHub owner/name
// contract to process-boundary configuration without duplicating a looser
// regular expression in each binary.
func ValidRepositoryIdentity(repository string) bool {
	return repository == strings.ToLower(strings.TrimSpace(repository)) &&
		safeRepositoryIdentity(repository)
}

func (patch RegistrationPatch) Validate() error {
	if patch.Enabled == nil && patch.IssueMonitorEnabled == nil &&
		patch.PRMonitorEnabled == nil && patch.ExecutionEnabled == nil &&
		len(patch.Configuration) == 0 {
		return fmt.Errorf("registration patch is empty")
	}
	if len(patch.Configuration) > 0 && !validJSONObject(patch.Configuration) {
		return fmt.Errorf("configuration must be a JSON object")
	}
	return nil
}

//go:generate npm --prefix ../../../agentops run sync-hard-gates

//go:embed hard_gate_signal_names.generated.json
var supportedRepositoryGraderSignalsJSON []byte

var supportedRepositoryGraderSignals = mustLoadRepositoryGraderSignals()

func mustLoadRepositoryGraderSignals() map[string]struct{} {
	var names []string
	if err := json.Unmarshal(supportedRepositoryGraderSignalsJSON, &names); err != nil {
		panic(fmt.Sprintf("decode generated hard gate signal names: %v", err))
	}
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			panic("generated hard gate signal name must not be empty")
		}
		if _, duplicate := result[name]; duplicate {
			panic(fmt.Sprintf("duplicate generated hard gate signal name %q", name))
		}
		result[name] = struct{}{}
	}
	if len(result) == 0 {
		panic("generated hard gate signal namespace must not be empty")
	}
	return result
}

func validJSONObject(raw json.RawMessage) bool {
	supportedReviewPerspectives := map[string]struct{}{
		"functionality": {},
		"codeQuality":   {},
		"testQuality":   {},
		"ux":            {},
		"accessibility": {},
		"security":      {},
		"type-design":   {},
	}
	type gateSignal struct {
		Source string `json:"source"`
		Name   string `json:"name"`
	}
	type releasePolicy struct {
		Authority                  string       `json:"authority"`
		RequiredGateSignals        []gateSignal `json:"requiredGateSignals"`
		RequiredReviewPerspectives []string     `json:"requiredReviewPerspectives"`
		MinimumHeadEpochs          int          `json:"minimumHeadEpochs"`
	}
	type registrationConfiguration struct {
		ReleaseEvidence    *releasePolicy `json:"releaseEvidence,omitempty"`
		GateTimeoutSeconds map[string]int `json:"gateTimeoutSeconds,omitempty"`
		MergeMethod        string         `json:"mergeMethod,omitempty"`
	}
	var value registrationConfiguration
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return false
	}
	if value.MergeMethod != "" && value.MergeMethod != "squash" &&
		value.MergeMethod != "merge" && value.MergeMethod != "rebase" {
		return false
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return false
	}
	for _, key := range []string{"mergeMethod", "releaseEvidence", "gateTimeoutSeconds"} {
		member, present := members[key]
		if present && bytes.Equal(bytes.TrimSpace(member), []byte("null")) {
			return false
		}
	}
	allowedGateKeys := map[string]struct{}{
		"default": {}, "planning": {}, "design": {},
		"repository-graders": {}, "review": {}, "merge": {},
		"lease-recovery": {},
	}
	for key, seconds := range value.GateTimeoutSeconds {
		if _, supported := allowedGateKeys[key]; !supported || seconds < 60 || seconds > 2_592_000 {
			return false
		}
	}
	policy := value.ReleaseEvidence
	if policy == nil {
		return value.MergeMethod != "" || len(value.GateTimeoutSeconds) > 0 ||
			bytes.Equal(bytes.TrimSpace(raw), []byte(`{}`))
	}
	if policy.Authority != "human-ready-allowed" &&
		policy.Authority != "ai-triage-required" {
		return false
	}
	if len(policy.RequiredGateSignals) < 1 || len(policy.RequiredGateSignals) > 64 ||
		len(policy.RequiredReviewPerspectives) < 2 ||
		len(policy.RequiredReviewPerspectives) > len(supportedReviewPerspectives) ||
		policy.MinimumHeadEpochs < 1 || policy.MinimumHeadEpochs > 32 {
		return false
	}
	signals := map[string]struct{}{}
	for _, signal := range policy.RequiredGateSignals {
		if (signal.Source != "repository-grader" && signal.Source != "github-check") ||
			len(signal.Name) < 1 || len(signal.Name) > 128 {
			return false
		}
		if signal.Source == "repository-grader" {
			if _, supported := supportedRepositoryGraderSignals[signal.Name]; !supported {
				return false
			}
		}
		key := signal.Source + ":" + signal.Name
		if _, duplicate := signals[key]; duplicate {
			return false
		}
		signals[key] = struct{}{}
	}
	perspectives := map[string]struct{}{}
	for _, perspective := range policy.RequiredReviewPerspectives {
		if _, supported := supportedReviewPerspectives[perspective]; !supported {
			return false
		}
		if _, duplicate := perspectives[perspective]; duplicate {
			return false
		}
		perspectives[perspective] = struct{}{}
	}
	return true
}

func (registration Registration) Desired(component string) bool {
	if !registration.Enabled {
		return false
	}
	switch component {
	case ComponentIssueMonitor:
		return registration.IssueMonitorEnabled
	case ComponentPRMonitor:
		return registration.PRMonitorEnabled
	case ComponentForwarder:
		return registration.Enabled
	case ComponentExecution, ComponentQueue:
		return registration.ExecutionEnabled
	default:
		return false
	}
}

type ActualState struct {
	Component           string     `json:"component"`
	RegistrationVersion int64      `json:"registrationVersion"`
	State               string     `json:"state"`
	SupervisorID        string     `json:"supervisorId"`
	ObservedAt          time.Time  `json:"observedAt"`
	LastHealthyAt       *time.Time `json:"lastHealthyAt"`
	LastError           *string    `json:"lastError"`
}

type ComponentProjection struct {
	Desired       bool       `json:"desired"`
	Actual        string     `json:"actual"`
	ObservedAt    *time.Time `json:"observedAt"`
	Freshness     string     `json:"freshness"`
	StaleReason   *string    `json:"staleReason"`
	LastGoodAt    *time.Time `json:"lastGoodAt"`
	LastError     *string    `json:"lastError"`
	RecoveryState string     `json:"recoveryState"`
}

type RegistrationProjection struct {
	Registration     Registration                   `json:"registration"`
	Mode             lifecycle.Mode                 `json:"mode"`
	Components       map[string]ComponentProjection `json:"components"`
	CursorObservedAt map[string]*time.Time          `json:"cursorObservedAt"`
	// LastPoll is the one-version deprecated wire alias. It is the cursor
	// advance time, not an attempted poll or a liveness signal.
	LastPoll                     map[string]*time.Time       `json:"lastPoll"`
	LastDelivery                 *time.Time                  `json:"lastDelivery"`
	QueueDepth                   int64                       `json:"queueDepth"`
	ActiveJobID                  *string                     `json:"activeJobId"`
	ActiveJobState               *string                     `json:"activeJobState"`
	ActiveJobRegistrationVersion *int64                      `json:"activeJobRegistrationVersion"`
	LastJobFailure               *JobFailureProjection       `json:"lastJobFailure"`
	RecentDeliveryFailures       []DeliveryFailureProjection `json:"recentDeliveryFailures"`
	DevelopmentProgress          []DevelopmentIssueProgress  `json:"developmentProgress"`
	// True when the registration has more Issues with durable progress than
	// the card shows, so the operator knows to reach for `agentopsctl progress`.
	DevelopmentProgressTruncated bool `json:"developmentProgressTruncated"`
}

// DevelopmentIssueProgress groups a current Issue state with history from the
// same Issue coordinate. LastActivity is computed from durable events and the
// active lease heartbeat so a quiet but healthy runner remains observable.
type DevelopmentIssueProgress struct {
	Repository   string                     `json:"repository"`
	IssueNumber  int64                      `json:"issueNumber"`
	Current      DevelopmentProgressEvent   `json:"current"`
	History      []DevelopmentProgressEvent `json:"history"`
	StartedAt    time.Time                  `json:"startedAt"`
	LastActivity time.Time                  `json:"lastActivity"`
}

// DevelopmentProgressEvent is the durable operator view of one Issue phase
// transition. Worktree/session coordinates are intentionally first-class: the
// operator can verify isolation without scraping runner logs.
type DevelopmentProgressEvent struct {
	ID                  int64      `json:"id"`
	RegistrationID      string     `json:"registrationId"`
	RegistrationVersion int64      `json:"registrationVersion"`
	JobID               string     `json:"jobId"`
	AttemptID           string     `json:"attemptId"`
	AttemptNumber       int        `json:"attemptNumber"`
	ReleaseID           *string    `json:"releaseId"`
	Repository          string     `json:"repository"`
	SubjectKind         string     `json:"subjectKind"`
	SubjectNumber       *int64     `json:"subjectNumber"`
	ParentIssueNumber   *int64     `json:"parentIssueNumber"`
	WorkerID            string     `json:"workerId"`
	EventKey            string     `json:"eventKey"`
	Phase               string     `json:"phase"`
	Step                string     `json:"step"`
	State               string     `json:"state"`
	Summary             *string    `json:"summary"`
	NextGate            *string    `json:"nextGate"`
	Blocker             *string    `json:"blocker"`
	SessionName         *string    `json:"sessionName"`
	WorktreePath        *string    `json:"worktreePath"`
	Branch              *string    `json:"branch"`
	PullRequestNumber   *int64     `json:"pullRequestNumber"`
	OccurredAt          time.Time  `json:"occurredAt"`
	JobStatus           string     `json:"jobStatus"`
	JobLastError        *string    `json:"jobLastError"`
	LeaseHeartbeatAt    *time.Time `json:"leaseHeartbeatAt"`

	// Canonical operator projection. These fields are derived from the durable
	// event plus job/attempt/lease/release facts; they are never accepted from a
	// runner. In particular, terminal facts override a stale running event.
	KanbanLane         string          `json:"kanbanLane"`
	HeadSHA            *string         `json:"headSha"`
	ReviewRound        *int            `json:"reviewRound"`
	ReviewOutcome      *string         `json:"reviewOutcome"`
	GateKey            *string         `json:"gateKey"`
	GateEnteredAt      *time.Time      `json:"gateEnteredAt"`
	GateWaitSeconds    int64           `json:"gateWaitSeconds"`
	HumanAction        *string         `json:"humanAction"`
	Terminal           bool            `json:"terminal"`
	EscalationID       *int64          `json:"escalationId"`
	EscalatedAt        *time.Time      `json:"escalatedAt"`
	EscalationEvidence json.RawMessage `json:"escalationEvidence,omitempty"`
	ReviewPerspectives json.RawMessage `json:"reviewPerspectives,omitempty"`
	BranchLineage      json.RawMessage `json:"branchLineage,omitempty"`

	// Supporting durable facts are intentionally not exposed as a second API
	// state model. canonicalDevelopmentProgress consumes them immediately after
	// the row is scanned.
	JobUpdatedAt          time.Time  `json:"-"`
	JobType               string     `json:"-"`
	JobAvailableAt        time.Time  `json:"-"`
	JobFinishedAt         *time.Time `json:"-"`
	AttemptStatus         string     `json:"-"`
	LeaseExpiresAt        *time.Time `json:"-"`
	ReleaseStatus         *string    `json:"-"`
	ReleaseFinalHead      *string    `json:"-"`
	JobResultOutcome      *string    `json:"-"`
	JobFailureCode        *string    `json:"-"`
	JobFailureRetryable   *bool      `json:"-"`
	EscalationReason      *string    `json:"-"`
	EscalationHumanAction *string    `json:"-"`
	EscalationGateEntered *time.Time `json:"-"`
	EscalationTargetSHA   *string    `json:"-"`
}

type CommandFence struct {
	RegistrationID      string `json:"registrationId"`
	RegistrationVersion int64  `json:"registrationVersion"`
	RouteAttempts       *int   `json:"routeAttempts,omitempty"`
}

type CommandOutcome struct {
	CommandID                      string        `json:"commandId"`
	CommandIdentityDigest          string        `json:"commandIdentityDigest"`
	ResourceIdentity               string        `json:"resourceIdentity"`
	Outcome                        string        `json:"outcome"`
	Reason                         *string       `json:"reason"`
	ObservedFence                  *CommandFence `json:"observedFence"`
	CurrentFence                   *CommandFence `json:"currentFence"`
	ResultVersionOrAttemptIdentity *string       `json:"resultVersionOrAttemptIdentity"`
	Recoverability                 string        `json:"recoverability"`
	RecordedAt                     time.Time     `json:"recordedAt"`
	Cancellable                    bool          `json:"cancellable"`
}

type RegistrationCommandResponse struct {
	Outcome      CommandOutcome `json:"outcome"`
	Registration Registration   `json:"registration"`
}

type RetryCommandResponse struct {
	Outcome CommandOutcome `json:"outcome"`
	Retry   RetryResult    `json:"retry"`
}

type JobFailureProjection struct {
	ID                  string    `json:"id"`
	RegistrationVersion int64     `json:"registrationVersion"`
	JobType             string    `json:"jobType"`
	Status              string    `json:"status"`
	LastError           *string   `json:"lastError"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type DeliveryFailureProjection struct {
	ID                  string    `json:"id"`
	DeliveryKey         string    `json:"deliveryKey"`
	Event               string    `json:"event"`
	Action              *string   `json:"action"`
	Status              string    `json:"status"`
	IgnoredReason       *string   `json:"ignoredReason"`
	LastError           *string   `json:"lastError"`
	RouteAttempts       int       `json:"routeAttempts"`
	RegistrationVersion *int64    `json:"registrationVersion"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type WorkItem struct {
	Repository  string         `json:"repository"`
	Kind        string         `json:"kind"`
	SourceEvent string         `json:"sourceEvent,omitempty"`
	Number      int64          `json:"number"`
	Identity    string         `json:"identity,omitempty"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	Payload     map[string]any `json:"-"`
}

const (
	WorkItemKindIssue          = "issue"
	WorkItemKindPullRequest    = "pull-request"
	WorkItemKindRepositoryHead = "repository-head"
)

func canonicalWorkItemKind(kind string) (string, bool) {
	switch kind {
	case WorkItemKindIssue:
		return WorkItemKindIssue, true
	case WorkItemKindPullRequest, "pull_request":
		return WorkItemKindPullRequest, true
	case WorkItemKindRepositoryHead, "push", "check_run", "check_suite":
		return WorkItemKindRepositoryHead, true
	default:
		return "", false
	}
}

// idempotencyKind preserves keys emitted before WorkItem separated entity kind
// from the GitHub source event. New producers use canonical kinds, while old
// values remain accepted so a replay cannot create a duplicate durable job.
func (item WorkItem) idempotencyKind() string {
	switch item.Kind {
	case "pull_request", "push", "check_run", "check_suite":
		return item.Kind
	case WorkItemKindPullRequest:
		return "pull_request"
	case WorkItemKindRepositoryHead:
		switch item.SourceEvent {
		case "push", "check_run", "check_suite":
			return item.SourceEvent
		default:
			return WorkItemKindRepositoryHead
		}
	default:
		return item.Kind
	}
}

func (item WorkItem) IdempotencyKey() string {
	kind := item.idempotencyKind()
	if item.Identity != "" {
		key := fmt.Sprintf(
			"github:%s:%s:%s",
			strings.ToLower(item.Repository),
			kind,
			item.Identity,
		)
		if !item.UpdatedAt.IsZero() {
			key += ":" + item.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		return key
	}
	return fmt.Sprintf(
		"github:%s:%s:%d:%s",
		strings.ToLower(item.Repository),
		kind,
		item.Number,
		item.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
}

func (item WorkItem) CanonicalPayload() map[string]any {
	payload := make(map[string]any, len(item.Payload)+6)
	for key, value := range item.Payload {
		payload[key] = value
	}
	payload["repository"] = strings.ToLower(item.Repository)
	kind, supported := canonicalWorkItemKind(item.Kind)
	if !supported {
		kind = item.Kind
	}
	payload["entityKind"] = kind
	if item.SourceEvent != "" {
		payload["sourceEvent"] = item.SourceEvent
	}
	if item.Number > 0 {
		payload["number"] = item.Number
	}
	if item.Identity != "" {
		payload["identity"] = item.Identity
	}
	if !item.UpdatedAt.IsZero() {
		payload["updatedAt"] = item.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return payload
}

// TriagePayload projects an Issue observation into the only payload accepted
// by the capability-limited triage runner. It intentionally carries no Issue
// body, labels, repository path, command, credential, or mutation request; the
// triage runner re-reads current GitHub state through typed operations.
func (item WorkItem) TriagePayload() (map[string]any, error) {
	canonicalRepository := strings.ToLower(strings.TrimSpace(item.Repository))
	repository := strings.Split(canonicalRepository, "/")
	if len(repository) != 2 || canonicalRepository != item.Repository ||
		!safeRepositoryIdentity(canonicalRepository) {
		return nil, fmt.Errorf("triage work repository must be canonical owner/name")
	}
	kind, supported := canonicalWorkItemKind(item.Kind)
	if !supported || kind != WorkItemKindIssue || item.Number < 1 || item.UpdatedAt.IsZero() {
		return nil, fmt.Errorf("triage work requires a positive Issue observation")
	}
	return map[string]any{
		"schemaVersion": 1,
		"repository": map[string]any{
			"owner": repository[0],
			"name":  repository[1],
		},
		"issue": map[string]any{
			"number":            item.Number,
			"observedUpdatedAt": item.UpdatedAt.UTC().Format(time.RFC3339Nano),
		},
	}, nil
}

// QueuedJob selects the capability boundary from the observed entity kind.
// Every Issue is triaged first. Only the triage runner's explicit ready-label
// promotion can create an agentops.runner development_turn for that Issue.
func (item WorkItem) QueuedJob(
	sourceKind, mergeMethod string,
) (string, map[string]any, error) {
	kind, supported := canonicalWorkItemKind(item.Kind)
	if !supported {
		return "", nil, fmt.Errorf("unsupported work item kind %q", item.Kind)
	}
	if kind == WorkItemKindIssue {
		payload, err := item.TriagePayload()
		return "agentops.triage", payload, err
	}
	payload, err := item.RunnerPayload(sourceKind, mergeMethod)
	return "agentops.runner", payload, err
}

// RunnerPayload projects control-plane observations into the only executable
// job contract. It never forwards the webhook body, a command, clone URL,
// credential, host path, or arbitrary environment to the runner.
func (item WorkItem) RunnerPayload(
	sourceKind, mergeMethod string,
) (map[string]any, error) {
	canonicalRepository := strings.ToLower(strings.TrimSpace(item.Repository))
	repository := strings.Split(canonicalRepository, "/")
	if len(repository) != 2 || canonicalRepository != item.Repository ||
		!safeRepositoryIdentity(canonicalRepository) {
		return nil, fmt.Errorf("runner work repository must be canonical owner/name")
	}
	event := map[string]any{}
	mode := "pr_reconciliation"
	kind, supported := canonicalWorkItemKind(item.Kind)
	if !supported {
		return nil, fmt.Errorf("unsupported executable work item kind %q", item.Kind)
	}
	if mergeMethod != "squash" && mergeMethod != "merge" && mergeMethod != "rebase" {
		return nil, fmt.Errorf("unsupported integration strategy %q", mergeMethod)
	}
	switch kind {
	case WorkItemKindIssue:
		if item.Number < 1 {
			return nil, fmt.Errorf("runner issue number must be positive")
		}
		mode = "development_turn"
		event = map[string]any{
			"kind": "issue", "number": item.Number, "action": "recovery",
		}
	case WorkItemKindPullRequest:
		if item.Number < 1 {
			return nil, fmt.Errorf("runner pull request number must be positive")
		}
		event = map[string]any{
			"kind": "pull_request", "number": item.Number, "action": "recovery",
		}
	case WorkItemKindRepositoryHead:
		identity := item.Identity
		if identity == "" {
			identity = item.IdempotencyKey()
		}
		trigger := item.SourceEvent
		if trigger == "" && item.Kind != WorkItemKindRepositoryHead {
			trigger = item.Kind
		}
		if trigger != "push" && trigger != "check_run" && trigger != "check_suite" {
			return nil, fmt.Errorf("repository-head work requires a supported source event")
		}
		event = map[string]any{
			"kind": "repository", "trigger": trigger, "identity": identity,
		}
		if ref, ok := item.Payload["ref"].(string); ok && ref != "" {
			event["ref"] = ref
		}
		if after, ok := item.Payload["after"].(string); ok && after != "" {
			event["after"] = after
		}
	}
	_ = sourceKind // Source identity remains in the outer durable job envelope.
	return map[string]any{
		"schemaVersion": 1,
		"repository": map[string]any{
			"owner": repository[0],
			"name":  repository[1],
		},
		"event": event,
		"target": map[string]any{
			"baseRef": "refs/heads/main",
		},
		"execution": map[string]any{
			"mode":           mode,
			"requiredChecks": []string{},
			"mergeMethod":    mergeMethod,
			"readyLabel":     "ready",
			"claimedLabel":   "agent-claimed",
		},
		"artifacts": []any{},
	}, nil
}

type WebhookReceipt struct {
	DeliveryID string `json:"deliveryId"`
	Duplicate  bool   `json:"duplicate"`
	Status     string `json:"status"`
}

type ClaimedDelivery struct {
	ID                  string
	DeliveryKey         string
	Repository          string
	Event               string
	Action              *string
	Payload             map[string]any
	Token               string
	RouteAttempts       int
	RegistrationID      *string
	RegistrationVersion *int64
}

type RetryResult struct {
	AttemptID   string    `json:"attemptId"`
	DeliveryID  string    `json:"deliveryId"`
	State       string    `json:"state"`
	Cancellable bool      `json:"cancellable"`
	RecordedAt  time.Time `json:"recordedAt"`
}

type DeliveryStatus struct {
	ID                  string                 `json:"id"`
	DeliveryKey         string                 `json:"deliveryKey"`
	Repository          string                 `json:"repository"`
	Event               string                 `json:"event"`
	Action              *string                `json:"action"`
	Status              string                 `json:"status"`
	IgnoredReason       *string                `json:"ignoredReason"`
	LastError           *string                `json:"lastError"`
	RouteAttempts       int                    `json:"routeAttempts"`
	RegistrationID      *string                `json:"registrationId"`
	RegistrationVersion *int64                 `json:"registrationVersion"`
	ReceivedAt          time.Time              `json:"receivedAt"`
	UpdatedAt           time.Time              `json:"updatedAt"`
	RetryAttempts       []DeliveryRetryAttempt `json:"retryAttempts"`
}

type DeliveryRetryAttempt struct {
	AttemptID             string    `json:"attemptId"`
	Status                string    `json:"status"`
	Reason                *string   `json:"reason"`
	ObservedRouteAttempts int       `json:"observedRouteAttempts"`
	CreatedAt             time.Time `json:"createdAt"`
}

type DeliveryRetryConflict struct {
	Reason              string    `json:"reason"`
	State               string    `json:"state"`
	RouteAttempts       int       `json:"routeAttempts"`
	AttemptID           string    `json:"attemptId"`
	RegistrationID      string    `json:"registrationId"`
	RegistrationVersion int64     `json:"registrationVersion"`
	RecordedAt          time.Time `json:"recordedAt"`
}

func (conflict *DeliveryRetryConflict) Error() string {
	return fmt.Sprintf(
		"delivery retry rejected: %s (state=%s, routeAttempts=%d)",
		conflict.Reason,
		conflict.State,
		conflict.RouteAttempts,
	)
}

func (conflict *DeliveryRetryConflict) Unwrap() error {
	switch conflict.Reason {
	case "registration_disabled", "registration_missing", "registration_stale":
		return ErrStaleRegistration
	default:
		return ErrConflict
	}
}
