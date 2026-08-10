package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

type fakeAPIStore struct {
	webhooks        int
	creates         int
	createResult    Registration
	createDuplicate bool
	createError     error
	created         CreateRegistration
	projections     []RegistrationProjection
	projectionError error
	currentMode     lifecycle.Mode
	lifecycleError  error
	retryError      error
	updateError     error
	updateDuplicate bool
	updated         Registration
	updatedPatch    RegistrationPatch
	deliveryStatus  DeliveryStatus
	deliveryError   error
	auditEvents     []string
	auditDetails    []map[string]any
	auditError      error
}

const (
	testRegistrationID = "11111111-1111-4111-8111-111111111111"
	testDeliveryID     = "22222222-2222-4222-8222-222222222222"
)

func (store *fakeAPIStore) Ping(context.Context) error { return nil }

func (store *fakeAPIStore) LifecycleMode(context.Context) (lifecycle.Mode, error) {
	if store.lifecycleError != nil {
		return "", store.lifecycleError
	}
	if store.currentMode == "" {
		return lifecycle.ModeMonitorOnly, nil
	}
	return store.currentMode, nil
}

func (store *fakeAPIStore) CreateRegistration(
	_ context.Context,
	input CreateRegistration,
	_, _ string,
) (Registration, bool, error) {
	store.creates++
	store.created = input
	if store.createError != nil {
		return store.createResult, store.createDuplicate, store.createError
	}
	validated, err := input.Validated()
	validated.ID = testRegistrationID
	validated.Version = 1
	return validated, false, err
}

func (store *fakeAPIStore) UpdateRegistrationCommand(
	_ context.Context,
	_ string,
	_ int64,
	patch RegistrationPatch,
	_ string,
	_ string,
	_ string,
) (Registration, bool, error) {
	store.updatedPatch = patch
	return store.updated, store.updateDuplicate, store.updateError
}

func (store *fakeAPIStore) Projections(
	context.Context,
	time.Duration,
) ([]RegistrationProjection, error) {
	return store.projections, store.projectionError
}

func (store *fakeAPIStore) ReceiveWebhook(
	context.Context,
	string,
	string,
	string,
	*string,
	map[string]string,
	map[string]any,
) (WebhookReceipt, error) {
	store.webhooks++
	return WebhookReceipt{DeliveryID: testDeliveryID, Status: "pending"}, nil
}

func (store *fakeAPIStore) RetryWebhook(
	context.Context,
	string,
	string,
	string,
	int,
	string,
	int64,
) (RetryResult, bool, error) {
	return RetryResult{}, false, store.retryError
}

func (store *fakeAPIStore) DeliveryStatus(
	context.Context,
	string,
) (DeliveryStatus, error) {
	return store.deliveryStatus, store.deliveryError
}

func (store *fakeAPIStore) AppendAudit(
	_ context.Context,
	_, eventType string,
	_, _ *string,
	details map[string]any,
) error {
	store.auditEvents = append(store.auditEvents, eventType)
	store.auditDetails = append(store.auditDetails, details)
	return store.auditError
}

func TestControlAPIRequiresAuthorizationAndOptimisticContractHeaders(t *testing.T) {
	store := &fakeAPIStore{}
	handler := testAPI(store).Handler()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/registrations",
		bytes.NewBufferString(`{"repository":"Owner/Repo"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || store.creates != 0 {
		t.Fatalf("unauthorized create status=%d creates=%d", response.Code, store.creates)
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"/v1/registrations",
		bytes.NewBufferString(`{"repository":"Owner/Repo"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer control-token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || store.creates != 0 {
		t.Fatalf("missing idempotency status=%d creates=%d", response.Code, store.creates)
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"/v1/registrations",
		bytes.NewBufferString(`{"repository":"Owner/Repo"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer control-token")
	request.Header.Set("Idempotency-Key", "registration-create-1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || store.creates != 1 {
		t.Fatalf("authorized create status=%d creates=%d body=%s", response.Code, store.creates, response.Body)
	}
	if response.Header().Get("ETag") != `"1"` {
		t.Fatalf("ETag = %q", response.Header().Get("ETag"))
	}
	for _, field := range []string{
		`"resourceIdentity":"` + testRegistrationID + `"`,
		`"resultVersionOrAttemptIdentity":"1"`,
		`"recoverability":"none"`,
	} {
		if !bytes.Contains(response.Body.Bytes(), []byte(field)) {
			t.Fatalf("create outcome missing %s: %s", field, response.Body)
		}
	}
}

func TestRegistrationCommandsCarryStrictReleaseEvidenceConfiguration(t *testing.T) {
	configuration := `{"releaseEvidence":{"authority":"ai-triage-required",` +
		`"requiredGateSignals":[{"source":"repository-grader","name":"api_tests"}],` +
		`"requiredReviewPerspectives":["functionality","security"],` +
		`"minimumHeadEpochs":1},"gateTimeoutSeconds":{"default":3600,"review":600},` +
		`"mergeMethod":"rebase"}`
	store := &fakeAPIStore{}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/registrations",
		bytes.NewBufferString(`{"repository":"owner/repo","configuration":`+configuration+`}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer control-token")
	request.Header.Set("Idempotency-Key", "registration-with-release-evidence")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated ||
		!bytes.Equal(store.created.Configuration, []byte(configuration)) {
		t.Fatalf("configured create status=%d configuration=%s body=%s", response.Code, store.created.Configuration, response.Body)
	}

	store.updated = Registration{ID: testRegistrationID, Version: 2}
	request = httptest.NewRequest(
		http.MethodPatch,
		"/v1/registrations/"+testRegistrationID,
		bytes.NewBufferString(`{"configuration":`+configuration+`}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer control-token")
	request.Header.Set("If-Match", `"1"`)
	request.Header.Set("Idempotency-Key", "registration-release-evidence-update")
	response = httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!bytes.Equal(store.updatedPatch.Configuration, []byte(configuration)) {
		t.Fatalf("configured patch status=%d configuration=%s body=%s", response.Code, store.updatedPatch.Configuration, response.Body)
	}

	unsupported := `{"releaseEvidence":{"authority":"human-ready-allowed",` +
		`"requiredGateSignals":[{"source":"github-check","name":"test"}],` +
		`"requiredReviewPerspectives":["security","performance"],` +
		`"minimumHeadEpochs":1}}`
	if validJSONObject(json.RawMessage(unsupported)) {
		t.Fatal("unsupported review perspective passed the control contract")
	}
	invalidRepositoryGrader := `{"releaseEvidence":{"authority":"human-ready-allowed",` +
		`"requiredGateSignals":[{"source":"repository-grader","name":"contracts"}],` +
		`"requiredReviewPerspectives":["security","codeQuality"],` +
		`"minimumHeadEpochs":1}}`
	if validJSONObject(json.RawMessage(invalidRepositoryGrader)) {
		t.Fatal("unknown repository grader signal passed the control contract")
	}
	if validJSONObject(json.RawMessage(`{"mergeMethod":"octopus"}`)) {
		t.Fatal("unsupported integration strategy passed the control contract")
	}
	customGitHubCheck := `{"releaseEvidence":{"authority":"human-ready-allowed",` +
		`"requiredGateSignals":[{"source":"github-check","name":"ci/custom"}],` +
		`"requiredReviewPerspectives":["security","codeQuality"],` +
		`"minimumHeadEpochs":1}}`
	if !validJSONObject(json.RawMessage(customGitHubCheck)) {
		t.Fatal("repository-defined GitHub check was rejected by the control contract")
	}
	if validJSONObject(json.RawMessage(`{"gateTimeoutSeconds":{"unknown":60}}`)) ||
		validJSONObject(json.RawMessage(`{"gateTimeoutSeconds":{"review":59}}`)) {
		t.Fatal("unsupported or unsafe gate timeout passed the control contract")
	}
}

func TestRegistrationCommandsRejectExplicitNullConfigurationMembers(t *testing.T) {
	for _, member := range []string{"releaseEvidence", "gateTimeoutSeconds"} {
		configuration := `{"` + member + `":null}`
		t.Run(member+"/create", func(t *testing.T) {
			store := &fakeAPIStore{}
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/registrations",
				bytes.NewBufferString(
					`{"repository":"owner/null-create","configuration":`+configuration+`}`,
				),
			)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer control-token")
			request.Header.Set("Idempotency-Key", "registration-null-create-"+member)
			response := httptest.NewRecorder()
			testAPI(store).Handler().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || store.creates != 0 ||
				!bytes.Contains(response.Body.Bytes(), []byte(`"invalid_registration_input"`)) {
				t.Fatalf(
					"explicit null create member %s status=%d creates=%d body=%s",
					member,
					response.Code,
					store.creates,
					response.Body,
				)
			}
		})

		t.Run(member+"/patch", func(t *testing.T) {
			store := &fakeAPIStore{}
			request := httptest.NewRequest(
				http.MethodPatch,
				"/v1/registrations/"+testRegistrationID,
				bytes.NewBufferString(`{"configuration":`+configuration+`}`),
			)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer control-token")
			request.Header.Set("If-Match", `"1"`)
			request.Header.Set("Idempotency-Key", "registration-null-patch-"+member)
			response := httptest.NewRecorder()
			testAPI(store).Handler().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest ||
				len(store.updatedPatch.Configuration) != 0 ||
				!bytes.Contains(response.Body.Bytes(), []byte(`"invalid_registration_patch"`)) {
				t.Fatalf(
					"explicit null patch member %s status=%d configuration=%s body=%s",
					member,
					response.Code,
					store.updatedPatch.Configuration,
					response.Body,
				)
			}
		})
	}
}

func TestWebhookPersistsOnlyAfterValidSignatureAndIdentity(t *testing.T) {
	store := &fakeAPIStore{}
	handler := testAPI(store).Handler()
	payload := []byte(
		`{"action":"opened","repository":{"full_name":"owner/repo"},` +
			`"issue":{"number":1,"updated_at":"2026-07-25T00:00:00Z"}}`,
	)
	request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", bytes.NewReader(payload))
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	request.Header.Set("X-GitHub-Event", "issues")
	request.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || store.webhooks != 0 {
		t.Fatalf("invalid signature status=%d persisted=%d", response.Code, store.webhooks)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", bytes.NewReader(payload))
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	request.Header.Set("X-GitHub-Event", "issues")
	request.Header.Set("X-Hub-Signature-256", sign(payload, "webhook-secret"))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.webhooks != 1 {
		t.Fatalf("valid signature status=%d persisted=%d body=%s", response.Code, store.webhooks, response.Body)
	}
	var receipt WebhookReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil ||
		receipt.DeliveryID != testDeliveryID {
		t.Fatalf("receipt = %#v error=%v", receipt, err)
	}

	trailing := append(append([]byte(nil), payload...), []byte(` {}`)...)
	request = httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", bytes.NewReader(trailing))
	request.Header.Set("X-GitHub-Delivery", "delivery-trailing")
	request.Header.Set("X-GitHub-Event", "issues")
	request.Header.Set("X-Hub-Signature-256", sign(trailing, "webhook-secret"))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || store.webhooks != 1 {
		t.Fatalf("trailing payload status=%d persisted=%d", response.Code, store.webhooks)
	}
}

func TestWebhookRejectsAlternateHostBeforeSignatureOrPersistence(t *testing.T) {
	store := &fakeAPIStore{}
	handler := browserTestAPI(store).Handler()
	payload := []byte(`{"repository":{"full_name":"owner/repo"}}`)
	request := httptest.NewRequest(
		http.MethodPost,
		"http://localhost:8080/v1/webhooks/github",
		bytes.NewReader(payload),
	)
	request.Host = "localhost:8080"
	request.Header.Set("X-GitHub-Delivery", "wrong-host-delivery")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", sign(payload, "webhook-secret"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || store.webhooks != 0 {
		t.Fatalf("wrong Host webhook status=%d persisted=%d body=%s", response.Code, store.webhooks, response.Body)
	}
}

func TestCreateConflictIdentifiesExistingRegistration(t *testing.T) {
	existing := Registration{
		ID:         testRegistrationID,
		Repository: "owner/repo",
		Version:    7,
	}
	store := &fakeAPIStore{createResult: existing, createError: ErrConflict}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/registrations",
		bytes.NewBufferString(`{"repository":"owner/repo"}`),
	)
	request.Header.Set("Authorization", "Bearer control-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-existing")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict ||
		response.Header().Get("ETag") != `"7"` ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"resourceIdentity":"`+testRegistrationID+`"`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"registrationVersion":7`)) {
		t.Fatalf("create conflict status=%d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
}

func TestExpectedVersionRequiresStrongQuotedVersion(t *testing.T) {
	for _, value := range []string{"1", `W/"1"`, `"0"`, `"invalid"`} {
		t.Run(value, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPatch,
				"/v1/registrations/"+testRegistrationID,
				bytes.NewBufferString(`{"enabled":false}`),
			)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer control-token")
			request.Header.Set("If-Match", value)
			response := httptest.NewRecorder()
			testAPI(&fakeAPIStore{}).Handler().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("If-Match %q status=%d", value, response.Code)
			}
		})
	}
}

func TestVersionConflictReturnsStructuredCurrentFence(t *testing.T) {
	recordedAt := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	store := &fakeAPIStore{
		updateError: &RegistrationCommandRejection{
			Cause:      ErrConflict,
			Reason:     "registration_version_mismatch",
			RecordedAt: recordedAt,
		},
		updateDuplicate: true,
		updated: Registration{
			ID:         testRegistrationID,
			Repository: "owner/repo",
			Version:    4,
		},
	}
	request := httptest.NewRequest(
		http.MethodPatch,
		"/v1/registrations/"+testRegistrationID,
		bytes.NewBufferString(`{"enabled":false}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer control-token")
	request.Header.Set("If-Match", `"3"`)
	request.Header.Set("Idempotency-Key", "update-version-conflict")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict ||
		response.Header().Get("ETag") != `"4"` ||
		response.Header().Get("Idempotent-Replay") != "true" ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"outcome":"version_conflict"`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"registrationVersion":4`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"recordedAt":"2026-07-26T02:00:00Z"`)) {
		t.Fatalf("version conflict = %d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
}

func TestHealthAndRegistrationPageExposeTheSameReleaseProvenance(t *testing.T) {
	api := testAPI(&fakeAPIStore{})
	api.ReleaseRepository = "mrbaron3/servo"
	api.ReleaseRevision = strings.Repeat("a", 40)

	healthRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(healthResponse, healthRequest)
	if healthResponse.Code != http.StatusOK ||
		!bytes.Contains(healthResponse.Body.Bytes(), []byte(`"repository":"mrbaron3/servo"`)) ||
		!bytes.Contains(healthResponse.Body.Bytes(), []byte(`"revision":"`+strings.Repeat("a", 40)+`"`)) {
		t.Fatalf("health provenance = %d: %s", healthResponse.Code, healthResponse.Body)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "/v1/registrations", nil)
	pageRequest.Header.Set("Authorization", "Bearer control-token")
	pageResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK ||
		!bytes.Contains(pageResponse.Body.Bytes(), []byte(`"provenance":{"repository":"mrbaron3/servo"`)) {
		t.Fatalf("registration provenance = %d: %s", pageResponse.Code, pageResponse.Body)
	}
}

func TestEmptyRegistrationPatchReturnsStructuredRejection(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPatch,
		"/v1/registrations/"+testRegistrationID,
		bytes.NewBufferString(`{}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer control-token")
	request.Header.Set("If-Match", `"3"`)
	request.Header.Set("Idempotency-Key", "empty-update")
	response := httptest.NewRecorder()
	testAPI(&fakeAPIStore{}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"outcome":"rejected"`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"reason":"invalid_registration_patch"`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"resourceIdentity":"`+testRegistrationID+`"`)) {
		t.Fatalf("empty patch = %d: %s", response.Code, response.Body)
	}
}

func TestStatusQueryFiltersAndFailsClosedWithLastSuccessfulTime(t *testing.T) {
	store := &fakeAPIStore{projections: []RegistrationProjection{
		{Registration: Registration{Repository: "owner/one"}, Mode: lifecycle.ModeMonitorOnly},
		{Registration: Registration{Repository: "owner/two"}, Mode: lifecycle.ModeMonitorOnly},
	}}
	api := testAPI(store)
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/registrations?repository=OWNER/TWO",
		nil,
	)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status query = %d: %s", response.Code, response.Body)
	}
	var page struct {
		Items []RegistrationProjection `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Registration.Repository != "owner/two" {
		t.Fatalf("filtered items = %#v", page.Items)
	}

	store.projectionError = ErrStoreUnavailable
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable ||
		response.Header().Get("Retry-After") != "2" ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"lastSuccessfulAt":"`)) {
		t.Fatalf("failed status query = %d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
}

func TestStatusProjectionPreservesHistoricalRegistrationConfiguration(t *testing.T) {
	configuration := json.RawMessage(`{"releaseEvidence":{` +
		`"authority":"human-ready-allowed",` +
		`"requiredGateSignals":[{"source":"repository-grader","name":"contracts"},` +
		`{"source":"repository-grader","name":"contracts"}],` +
		`"requiredReviewPerspectives":["security","security"],` +
		`"minimumHeadEpochs":1}}`)
	store := &fakeAPIStore{projections: []RegistrationProjection{{
		Registration: Registration{
			ID:            testRegistrationID,
			Repository:    "owner/historical",
			Configuration: configuration,
			Version:       1,
		},
		Mode:       lifecycle.ModeMonitorOnly,
		Components: map[string]ComponentProjection{},
	}}}
	request := httptest.NewRequest(http.MethodGet, "/v1/registrations", nil)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"name":"contracts"`)) {
		t.Fatalf("historical Registration response = %d: %s", response.Code, response.Body)
	}
}

func TestMonitorOnlyModeDoesNotMaskStaleExecutionEvidence(t *testing.T) {
	store := &fakeAPIStore{projections: []RegistrationProjection{{
		Registration: Registration{
			Repository:       "owner/stale",
			ExecutionEnabled: true,
			Enabled:          true,
		},
		Mode: lifecycle.ModeMonitorOnly,
		Components: map[string]ComponentProjection{
			ComponentExecution: {
				Desired: true, Actual: "stale", Freshness: "stale",
				RecoveryState: "blocked",
			},
			ComponentQueue: {
				Desired: true, Actual: "failed", Freshness: "fresh",
				RecoveryState: "scheduled",
			},
		},
	}}}
	request := httptest.NewRequest(http.MethodGet, "/v1/registrations", nil)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"actual":"stale"`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"actual":"failed"`)) {
		t.Fatalf("MONITOR_ONLY masked anomaly = %d: %s", response.Code, response.Body)
	}
}

func TestStatusProjectionPreservesAuthoritativeLifecycleMode(t *testing.T) {
	observedAt := time.Date(2026, 8, 6, 1, 2, 3, 0, time.UTC)
	cursorObservedAt := map[string]*time.Time{
		"issue": &observedAt, "pull_request": nil,
	}
	store := &fakeAPIStore{projections: []RegistrationProjection{{
		Registration:     Registration{Repository: "owner/draining"},
		Mode:             lifecycle.ModeDraining,
		Components:       map[string]ComponentProjection{},
		CursorObservedAt: cursorObservedAt,
		LastPoll:         cursorObservedAt,
	}}}
	request := httptest.NewRequest(http.MethodGet, "/v1/registrations", nil)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status query = %d: %s", response.Code, response.Body)
	}
	var page struct {
		Items []RegistrationProjection `json:"items"`
		Mode  lifecycle.Mode           `json:"mode"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Mode != lifecycle.ModeDraining ||
		page.Mode != lifecycle.ModeDraining ||
		page.Items[0].CursorObservedAt["issue"] == nil ||
		page.Items[0].LastPoll["issue"] == nil ||
		!page.Items[0].CursorObservedAt["issue"].Equal(
			*page.Items[0].LastPoll["issue"],
		) {
		t.Fatalf("lifecycle mode was overwritten: %#v", page.Items)
	}
}

func TestEmptyStatusProjectionStillUsesAuthoritativeLifecycleMode(t *testing.T) {
	store := &fakeAPIStore{currentMode: lifecycle.ModeDraining}
	request := httptest.NewRequest(http.MethodGet, "/v1/registrations", nil)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"mode":"DRAINING"`)) {
		t.Fatalf("empty lifecycle projection = %d: %s", response.Code, response.Body)
	}
}

func TestEmptyStatusProjectionFailsClosedWhenLifecycleAuthorityIsUnavailable(t *testing.T) {
	store := &fakeAPIStore{lifecycleError: ErrStoreUnavailable}
	request := httptest.NewRequest(http.MethodGet, "/v1/registrations", nil)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable ||
		response.Header().Get("Retry-After") != "2" ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"code":"control_store_unavailable"`)) {
		t.Fatalf("unavailable lifecycle authority = %d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
}

func TestStatusProjectionRejectsAnItemWithoutLifecycleMode(t *testing.T) {
	store := &fakeAPIStore{projections: []RegistrationProjection{{
		Registration: Registration{Repository: "owner/missing-mode"},
		Components:   map[string]ComponentProjection{},
	}}}
	request := httptest.NewRequest(http.MethodGet, "/v1/registrations", nil)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest ||
		!bytes.Contains(response.Body.Bytes(), []byte("status projection omitted lifecycle mode")) {
		t.Fatalf("missing lifecycle mode = %d: %s", response.Code, response.Body)
	}
}

func TestOffModeProjectsExecutionAndQueueAsBlockedByMode(t *testing.T) {
	store := &fakeAPIStore{projections: []RegistrationProjection{{
		Registration: Registration{
			Repository:       "owner/off",
			Enabled:          true,
			ExecutionEnabled: true,
		},
		Mode: lifecycle.ModeOff,
		Components: map[string]ComponentProjection{
			ComponentExecution: {Desired: true, Actual: "unknown", Freshness: "unknown"},
			ComponentQueue:     {Desired: true, Actual: "unknown", Freshness: "unknown"},
		},
	}}}
	request := httptest.NewRequest(http.MethodGet, "/v1/registrations", nil)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"actual":"paused_by_mode"`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"actual":"blocked_by_mode"`)) {
		t.Fatalf("OFF lifecycle projection = %d: %s", response.Code, response.Body)
	}
}

func TestOpaqueContinuationRemainsBoundToFirstSnapshot(t *testing.T) {
	store := &fakeAPIStore{projections: []RegistrationProjection{
		{Registration: Registration{Repository: "owner/one"}, Mode: lifecycle.ModeDraining},
		{Registration: Registration{Repository: "owner/two"}, Mode: lifecycle.ModeDraining},
	}}
	api := testAPI(store)
	request := httptest.NewRequest(http.MethodGet, "/v1/registrations?limit=1", nil)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	var first struct {
		Items         []RegistrationProjection `json:"items"`
		NextPageToken string                   `json:"nextPageToken"`
		ObservedAt    time.Time                `json:"observedAt"`
		Mode          lifecycle.Mode           `json:"mode"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil ||
		len(first.Items) != 1 ||
		first.NextPageToken == "" {
		t.Fatalf("first page = %#v error=%v body=%s", first, err, response.Body)
	}

	store.projections = []RegistrationProjection{
		{Registration: Registration{Repository: "owner/one"}, Mode: lifecycle.ModeActive},
		{Registration: Registration{Repository: "owner/replaced"}, Mode: lifecycle.ModeActive},
	}
	request = httptest.NewRequest(
		http.MethodGet,
		"/v1/registrations?limit=1&pageToken="+first.NextPageToken,
		nil,
	)
	request.Header.Set("Authorization", "Bearer control-token")
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	var second struct {
		Items      []RegistrationProjection `json:"items"`
		ObservedAt time.Time                `json:"observedAt"`
		Mode       lifecycle.Mode           `json:"mode"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil ||
		len(second.Items) != 1 ||
		second.Items[0].Registration.Repository != "owner/two" ||
		second.Items[0].Mode != lifecycle.ModeDraining ||
		second.Mode != lifecycle.ModeDraining ||
		first.Mode != lifecycle.ModeDraining ||
		!second.ObservedAt.Equal(first.ObservedAt) {
		t.Fatalf("second page escaped snapshot = %#v error=%v body=%s", second, err, response.Body)
	}
}

func TestRetryConflictReturnsCurrentStateAndReason(t *testing.T) {
	store := &fakeAPIStore{retryError: &DeliveryRetryConflict{
		Reason: "delivery_not_retryable", State: "processing", RouteAttempts: 2,
	}}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/deliveries/"+testDeliveryID+"/retry",
		bytes.NewBufferString(
			`{"observedAttempts":1,"expectedRegistrationId":"`+testRegistrationID+`",`+
				`"expectedRegistrationVersion":1}`,
		),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer control-token")
	request.Header.Set("Idempotency-Key", "retry-1")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"state":"processing"`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"routeAttempts":2`)) {
		t.Fatalf("retry conflict = %d: %s", response.Code, response.Body)
	}
}

func TestDeliveryStatusQueryReturnsCurrentDurableState(t *testing.T) {
	store := &fakeAPIStore{deliveryStatus: DeliveryStatus{
		ID: testDeliveryID, Status: "processed", RouteAttempts: 2,
		RetryAttempts: []DeliveryRetryAttempt{{
			AttemptID: "attempt-1", Status: "accepted",
		}},
	}}
	request := httptest.NewRequest(http.MethodGet, "/v1/deliveries/"+testDeliveryID, nil)
	request.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	testAPI(store).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"status":"processed"`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"attemptId":"attempt-1"`)) {
		t.Fatalf("delivery status = %d: %s", response.Code, response.Body)
	}
}

func testAPI(store APIStore) *API {
	return &API{
		Store:         store,
		ControlToken:  "control-token",
		WebhookSecret: "webhook-secret",
		StaleAfter:    time.Minute,
		RouterWake:    func() {},
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
