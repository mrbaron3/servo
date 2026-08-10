package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mrbaron3/servo/apps/control-plane/internal/control"
)

func TestStandardRuntimeTopologyExcludesLegacyForwarder(t *testing.T) {
	if standardRuntimeTopology.ManagesComponent(control.ComponentForwarder) {
		t.Fatal("standard signed-ingress topology still manages the legacy forwarder")
	}
}

func TestOpenStandardControlStoreWiresSignedIngressTopology(t *testing.T) {
	original := openControlStoreWithTopology
	t.Cleanup(func() { openControlStoreWithTopology = original })
	wantError := errors.New("constructor observed")
	var observedDatabaseURL, observedRoot string
	var observedTopology control.RuntimeTopology
	openControlStoreWithTopology = func(
		_ context.Context,
		databaseURL, root string,
		topology control.RuntimeTopology,
	) (*control.Store, error) {
		observedDatabaseURL = databaseURL
		observedRoot = root
		observedTopology = topology
		return nil, wantError
	}

	_, err := openStandardControlStore(
		context.Background(),
		"postgresql://control.example/servo",
		"/application/root",
	)
	if !errors.Is(err, wantError) ||
		observedDatabaseURL != "postgresql://control.example/servo" ||
		observedRoot != "/application/root" ||
		observedTopology != control.RuntimeTopologySignedWebhookIngress {
		t.Fatalf(
			"standard store wiring error=%v databaseURL=%q root=%q topology=%q",
			err,
			observedDatabaseURL,
			observedRoot,
			observedTopology,
		)
	}
}

func TestRunCallsStandardControlStoreBoundary(t *testing.T) {
	original := openStandardControlStore
	t.Cleanup(func() { openStandardControlStore = original })
	wantError := errors.New("run standard store boundary observed")
	databaseURL := "http://invalid.example"
	root, err := applicationRoot()
	if err != nil {
		t.Fatal(err)
	}
	called := false
	openStandardControlStore = func(
		_ context.Context,
		observedDatabaseURL, observedRoot string,
	) (*control.Store, error) {
		called = true
		if observedDatabaseURL != databaseURL || observedRoot != root {
			t.Fatalf(
				"run standard store coordinates databaseURL=%q root=%q",
				observedDatabaseURL,
				observedRoot,
			)
		}
		return nil, wantError
	}
	t.Setenv("AGENTOPS_APP_ROOT", root)
	t.Setenv("AGENTOPS_DATABASE_URL", databaseURL)
	t.Setenv("AGENTOPS_CONTROL_TOKEN", strings.Repeat("c", 32))
	t.Setenv("AGENTOPS_DASHBOARD_BOOTSTRAP_TOKEN", strings.Repeat("d", 32))

	if err := run(); !errors.Is(err, wantError) {
		t.Fatalf("run did not propagate the standard store boundary: %v", err)
	}
	if !called {
		t.Fatal("run bypassed the standard store boundary")
	}
}

func TestProgressCommandCallsStandardControlStoreBoundary(t *testing.T) {
	original := openStandardControlStore
	t.Cleanup(func() { openStandardControlStore = original })
	wantError := errors.New("admin standard store boundary observed")
	databaseURL := "http://invalid.example"
	root, err := applicationRoot()
	if err != nil {
		t.Fatal(err)
	}
	called := false
	openStandardControlStore = func(
		_ context.Context,
		observedDatabaseURL, observedRoot string,
	) (*control.Store, error) {
		called = true
		if observedDatabaseURL != databaseURL || observedRoot != root {
			t.Fatalf(
				"admin standard store coordinates databaseURL=%q root=%q",
				observedDatabaseURL,
				observedRoot,
			)
		}
		return nil, wantError
	}
	t.Setenv("AGENTOPS_APP_ROOT", root)
	t.Setenv("AGENTOPS_DATABASE_URL", databaseURL)

	err = runAdministrativeCommand([]string{
		"progress",
		"--repository",
		"sample/repository",
	})
	if !errors.Is(err, wantError) {
		t.Fatalf("progress did not propagate the standard store boundary: %v", err)
	}
	if !called {
		t.Fatal("progress bypassed the standard store boundary")
	}
}

func TestLoopbackPublishProxyRequiresLoopbackBackendAndExactHost(t *testing.T) {
	if _, err := loopbackPublishProxy(
		"0.0.0.0:8080",
		"0.0.0.0:8081",
		"http://127.0.0.1:8080",
	); err == nil {
		t.Fatal("non-loopback proxy backend was accepted")
	}

	observedPeer := ""
	backend := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		observedPeer = request.RemoteAddr
		if request.Header.Get("Forwarded") != "" ||
			request.Header.Get("X-Forwarded-For") != "" {
			t.Error("forwarding provenance headers reached loopback backend")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	backendAddress := strings.TrimPrefix(backend.URL, "http://")
	proxy, err := loopbackPublishProxy(
		"0.0.0.0:8080",
		backendAddress,
		"http://127.0.0.1:8080",
	)
	if err != nil {
		t.Fatal(err)
	}

	invalid := httptest.NewRequest(http.MethodGet, "http://localhost:8080/healthz", nil)
	invalid.Host = "localhost:8080"
	invalidResponse := httptest.NewRecorder()
	proxy.Handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusForbidden {
		t.Fatalf("invalid Host status = %d", invalidResponse.Code)
	}

	valid := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/healthz", nil)
	valid.Host = "127.0.0.1:8080"
	valid.Header.Set("Forwarded", "for=203.0.113.1")
	valid.Header.Set("X-Forwarded-For", "203.0.113.1")
	validResponse := httptest.NewRecorder()
	proxy.Handler.ServeHTTP(validResponse, valid)
	if validResponse.Code != http.StatusNoContent {
		t.Fatalf("exact Host status = %d body=%s", validResponse.Code, validResponse.Body)
	}
	if !strings.HasPrefix(observedPeer, "127.0.0.1:") {
		t.Fatalf("backend peer = %q", observedPeer)
	}
}

func TestPRIntentAdministrativeRotationBoundary(t *testing.T) {
	original := rotatePostgresAdmin
	t.Cleanup(func() { rotatePostgresAdmin = original })
	t.Setenv(
		"AGENTOPS_DATABASE_URL",
		"postgresql://postgres:current-password-value-0000001@postgres/agentops",
	)
	next := "next-password-value-00000000000002"
	t.Setenv("AGENTOPS_NEXT_POSTGRES_PASSWORD", next)
	var observedURL, observedPassword, observedRequest string
	rotatePostgresAdmin = func(
		_ context.Context,
		databaseURL, nextPassword, requestID string,
	) error {
		observedURL = databaseURL
		observedPassword = nextPassword
		observedRequest = requestID
		return nil
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = write
	commandErr := runAdministrativeCommand([]string{
		"rotate-postgres-admin",
		"--request-id",
		"admin-rotation-001",
	})
	_ = write.Close()
	os.Stdout = originalStdout
	output, readErr := io.ReadAll(read)
	_ = read.Close()
	if commandErr != nil || readErr != nil {
		t.Fatalf("rotation command=%v output=%v", commandErr, readErr)
	}
	if observedURL == "" || observedPassword != next ||
		observedRequest != "admin-rotation-001" {
		t.Fatalf(
			"rotation forwarding url=%q passwordMatch=%t request=%q",
			observedURL,
			observedPassword == next,
			observedRequest,
		)
	}
	if strings.Contains(string(output), next) ||
		!strings.Contains(string(output), `"rotated":true`) ||
		!strings.Contains(string(output), `"requestId":"admin-rotation-001"`) {
		t.Fatalf("unsafe or incorrect success output: %s", output)
	}

	for _, args := range [][]string{
		{"rotate-postgres-admin"},
		{"rotate-postgres-admin", "--request-id", "id", "extra"},
	} {
		if err := runAdministrativeCommand(args); err == nil {
			t.Fatalf("invalid arguments were accepted: %#v", args)
		}
	}
	rotatePostgresAdmin = func(
		context.Context,
		string, string, string,
	) error {
		return errors.New("injected rotation failure")
	}
	if err := runAdministrativeCommand([]string{
		"rotate-postgres-admin",
		"--request-id",
		"admin-rotation-002",
	}); err == nil || !strings.Contains(err.Error(), "injected rotation failure") {
		t.Fatalf("runtime failure was not propagated: %v", err)
	}
}
