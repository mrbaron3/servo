package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/control"
	"github.com/mrbaron3/servo/apps/control-plane/internal/designgate"
	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

// The standard OCI deployment has one topology authority shared by Store,
// Supervisor, and ProductionRunner. GitHub credentials and gh remain runner-only.
const standardRuntimeTopology = control.RuntimeTopologySignedWebhookIngress

var openControlStoreWithTopology = control.OpenStoreWithTopology

var openStandardControlStore = func(
	ctx context.Context,
	databaseURL, root string,
) (*control.Store, error) {
	return openControlStoreWithTopology(
		ctx,
		databaseURL,
		root,
		standardRuntimeTopology,
	)
}

func main() {
	if err := runCommand(os.Args[1:]); err != nil {
		slog.Error("agentops-control failed closed", "error", err)
		os.Exit(1)
	}
}

func runCommand(args []string) error {
	if len(args) > 0 {
		return runAdministrativeCommand(args)
	}
	return run()
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	root, err := applicationRoot()
	if err != nil {
		return err
	}
	bundleRoot := environment(
		"AGENTOPS_DESIGN_BUNDLE_ROOT",
		root+"/contracts/designflow/contract-v1.0.0-rc.1",
	)
	coveragePath := environment(
		"AGENTOPS_DESIGN_COVERAGE_PATH",
		root+"/evidence/ciso-03/design-capability-trace.json",
	)
	gate, err := designgate.Validate(bundleRoot, coveragePath)
	if err != nil {
		return fmt.Errorf("Experience design gate rejected Control API startup: %w", err)
	}
	log.Info(
		"Experience design gate accepted",
		"provider", designgate.ProviderRef,
		"providerCommit", designgate.ProviderCommit,
		"revisionId", gate.RevisionID,
		"bundleDigest", gate.BundleDigest,
		"capabilities", gate.CapabilityIDs,
	)
	dashboardGate, err := designgate.ValidateDashboard(root)
	if err != nil {
		return fmt.Errorf("approved #15 dashboard gate rejected Control API startup: %w", err)
	}
	log.Info(
		"approved #15 dashboard gate accepted",
		"revisionId", dashboardGate.RevisionID,
		"bundleDigest", dashboardGate.BundleDigest,
		"decisionId", dashboardGate.DecisionID,
		"capabilities", dashboardGate.CapabilityIDs,
	)

	databaseURL := strings.TrimSpace(os.Getenv("AGENTOPS_DATABASE_URL"))
	if databaseURL == "" {
		return fmt.Errorf("AGENTOPS_DATABASE_URL is required")
	}
	controlToken := strings.TrimSpace(os.Getenv("AGENTOPS_CONTROL_TOKEN"))
	if controlToken == "" {
		return fmt.Errorf("AGENTOPS_CONTROL_TOKEN is required")
	}
	if len(controlToken) < 32 || len(controlToken) > 512 {
		return fmt.Errorf("AGENTOPS_CONTROL_TOKEN must be 32..512 bytes")
	}
	startupMode, err := lifecycle.ParseMode(environment(
		"AGENTOPS_OPERATING_MODE",
		string(lifecycle.ModeMonitorOnly),
	))
	if err != nil {
		return err
	}
	log.Info("control startup mode observed", "startupMode", startupMode)
	canonicalOrigin := environment("AGENTOPS_DASHBOARD_ORIGIN", "http://127.0.0.1:8080")
	bootstrapToken := strings.TrimSpace(os.Getenv("AGENTOPS_DASHBOARD_BOOTSTRAP_TOKEN"))
	if bootstrapToken == "" {
		bootstrapToken, err = secureToken()
		if err != nil {
			return fmt.Errorf("generate one-time dashboard bootstrap token: %w", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	startupContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	store, err := openStandardControlStore(
		startupContext,
		databaseURL,
		root,
	)
	if err != nil {
		return err
	}
	defer store.Close()
	supervisorID := environment("AGENTOPS_SUPERVISOR_ID", hostname())
	reconciliationInterval := durationEnvironment(
		"AGENTOPS_RECONCILIATION_INTERVAL",
		15*time.Second,
	)
	pollInterval := durationEnvironment("AGENTOPS_GITHUB_POLL_INTERVAL", time.Minute)
	httpClient := &http.Client{Timeout: 30 * time.Second}
	var monitorSource control.MonitorSource = control.GitHubSource{
		Client:  httpClient,
		BaseURL: environment("AGENTOPS_GITHUB_API_URL", "https://api.github.com"),
		Token:   firstNonEmpty(os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN")),
	}
	if strings.EqualFold(
		strings.TrimSpace(os.Getenv("AGENTOPS_GITHUB_MONITOR_BROKER_ENABLED")),
		"true",
	) {
		if strings.TrimSpace(os.Getenv("GH_TOKEN")) != "" ||
			strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) != "" {
			return fmt.Errorf(
				"private monitor broker control must not receive a GitHub credential",
			)
		}
		monitorSource = control.BrokeredGitHubSource{
			Store:   store,
			Timeout: control.DefaultMonitorBrokerTimeout,
		}
	}
	runner := &control.ProductionRunner{
		Store:          store,
		Source:         monitorSource,
		SupervisorID:   supervisorID,
		PollInterval:   pollInterval,
		TransientRetry: 2 * time.Second,
		ForwarderRetry: 2 * time.Second,
		HealthInterval: reconciliationInterval,
		Log:            log,
	}
	supervisor := control.NewSupervisor(
		store,
		runner,
		supervisorID,
		reconciliationInterval,
		log,
	)
	router := &control.Router{
		Store:    store,
		Interval: reconciliationInterval,
		Lease:    30 * time.Second,
		Wake:     make(chan struct{}, 1),
		Log:      log,
	}
	api := &control.API{
		Store:           store,
		ControlToken:    controlToken,
		WebhookSecret:   strings.TrimSpace(os.Getenv("AGENTOPS_GITHUB_WEBHOOK_SECRET")),
		StaleAfter:      max(3*reconciliationInterval, 3*pollInterval, time.Minute),
		CanonicalOrigin: canonicalOrigin,
		BootstrapToken:  bootstrapToken,
		ReleaseRepository: strings.TrimSpace(
			os.Getenv("AGENTOPS_RELEASE_CONSUMER_REPOSITORY"),
		),
		ReleaseRevision: strings.TrimSpace(
			os.Getenv("AGENTOPS_RELEASE_CONSUMER_REVISION"),
		),
		SessionTTL: durationEnvironment("AGENTOPS_DASHBOARD_SESSION_TTL", 8*time.Hour),
		RouterWake: router.Signal,
		Log:        log,
	}
	if err := api.Initialize(); err != nil {
		return fmt.Errorf("initialize dashboard security boundary: %w", err)
	}
	server := &http.Server{
		Addr:              environment("AGENTOPS_CONTROL_LISTEN", "127.0.0.1:8080"),
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
	}
	proxyServer, err := loopbackPublishProxy(
		strings.TrimSpace(os.Getenv("AGENTOPS_CONTROL_PROXY_LISTEN")),
		server.Addr,
		canonicalOrigin,
	)
	if err != nil {
		return err
	}
	egressProxy, err := runnerEgressProxy(
		strings.TrimSpace(os.Getenv("AGENTOPS_RUNNER_EGRESS_PROXY_LISTEN")),
		environment("AGENTOPS_RUNNER_PROVIDER", "codex"),
		environment("AGENTOPS_RUNNER_PROVIDER_AUTH", "none"),
	)
	if err != nil {
		return err
	}

	go func() {
		if err := supervisor.Run(ctx); err != nil {
			log.Error("supervisor stopped", "error", err)
			stop()
		}
	}()
	go func() {
		if err := router.Run(ctx); err != nil {
			log.Error("router stopped", "error", err)
			stop()
		}
	}()
	go listenLoop(ctx, store, "agentops_registration_wake", supervisor.Wake, log)
	go listenLoop(ctx, store, "agentops_webhook_wake", router.Signal, log)

	serverError := make(chan error, 3)
	go func() {
		log.Info(
			"agentops-control listening",
			"address", server.Addr,
			"startupMode", startupMode,
			"dashboardBootstrapUrl",
			canonicalOrigin+"/dashboard/bootstrap?token="+url.QueryEscape(bootstrapToken),
		)
		serverError <- server.ListenAndServe()
	}()
	if proxyServer != nil {
		go func() {
			log.Info(
				"agentops-control loopback publication proxy listening",
				"address", proxyServer.Addr,
				"backend", server.Addr,
			)
			serverError <- proxyServer.ListenAndServe()
		}()
	}
	if egressProxy != nil {
		go func() {
			log.Info(
				"agentops-control runner egress proxy listening",
				"address", egressProxy.Addr,
				"provider", environment("AGENTOPS_RUNNER_PROVIDER", "codex"),
			)
			serverError <- egressProxy.ListenAndServe()
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-serverError:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if proxyServer != nil {
		if err := proxyServer.Shutdown(shutdownContext); err != nil {
			return err
		}
	}
	if egressProxy != nil {
		if err := egressProxy.Shutdown(shutdownContext); err != nil {
			return err
		}
	}
	return server.Shutdown(shutdownContext)
}

func loopbackPublishProxy(
	listenAddress, backendAddress, canonicalOrigin string,
) (*http.Server, error) {
	if listenAddress == "" {
		return nil, nil
	}
	backendHost, _, err := net.SplitHostPort(backendAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid AGENTOPS_CONTROL_LISTEN for publication proxy: %w", err)
	}
	backendIP := net.ParseIP(strings.Trim(backendHost, "[]"))
	if backendIP == nil || !backendIP.IsLoopback() {
		return nil, fmt.Errorf("publication proxy backend must be a loopback listener")
	}
	origin, err := url.Parse(canonicalOrigin)
	if err != nil || origin.Host == "" {
		return nil, fmt.Errorf("invalid AGENTOPS_DASHBOARD_ORIGIN for publication proxy")
	}
	target, err := url.Parse("http://" + backendAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid publication proxy backend: %w", err)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = request.In.Host
			request.Out.Header.Del("Forwarded")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
			request.Out.Header["X-Forwarded-For"] = nil
		},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(writer, "loopback Control API unavailable", http.StatusBadGateway)
		},
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Host != origin.Host {
			http.Error(writer, "invalid Control API Host", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(writer, request)
	})
	return &http.Server{
		Addr:              listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
	}, nil
}

func secureToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func listenLoop(
	ctx context.Context,
	store *control.Store,
	channel string,
	wake func(),
	log *slog.Logger,
) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := store.Listen(ctx, channel, wake); err != nil && ctx.Err() == nil {
			log.Error("LISTEN disconnected; periodic reconciliation remains authoritative",
				"channel", channel,
				"error", err,
			)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func environment(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnvironment(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		panic(fmt.Sprintf("%s must be a positive Go duration", name))
	}
	return duration
}

func hostname() string {
	value, err := os.Hostname()
	if err != nil {
		return "agentops-control"
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
