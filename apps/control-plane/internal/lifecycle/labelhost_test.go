package lifecycle

import (
	"context"
	"strings"
	"testing"
)

const runningStatusOutput = `FIELD              VALUE
status             running
appRoot            /Users/example/Library/Application Support/com.apple.container/
installRoot        /opt/homebrew/Cellar/container/1.1.0/
logRoot
apiserver.version  container-apiserver version 1.1.0 (build: release, commit: unspeci)
apiserver.commit   unspecified
`

const cliVersionOutput = "container CLI version 1.1.0 (build: release, commit: unspeci)"

func TestResolveMetadataHostReadsAppRootWithSpaces(t *testing.T) {
	runner := &fakeRuntimeRunner{results: []CommandResult{
		{Status: 0, Stdout: cliVersionOutput},
		{Status: 0, Stdout: runningStatusOutput},
	}}
	host, err := ResolveMetadataHost(context.Background(), runner)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The default application root contains "Application Support"; splitting the
	// status table on whitespace would truncate it.
	want := "/Users/example/Library/Application Support/com.apple.container"
	if host.AppRoot != want {
		t.Fatalf("appRoot = %q, want %q", host.AppRoot, want)
	}
	if host.CLIVersion != "1.1.0" || host.APIServerVersion != "1.1.0" {
		t.Fatalf("unexpected versions: %#v", host)
	}
}

func TestResolveMetadataHostRejectsUnsupportedVersions(t *testing.T) {
	for name, results := range map[string][]CommandResult{
		"cli drift": {
			{Status: 0, Stdout: "container CLI version 1.2.0 (build: release)"},
			{Status: 0, Stdout: runningStatusOutput},
		},
		"apiserver drift": {
			{Status: 0, Stdout: cliVersionOutput},
			{Status: 0, Stdout: strings.Replace(
				runningStatusOutput,
				"container-apiserver version 1.1.0",
				"container-apiserver version 1.0.9",
				1,
			)},
		},
		"unparseable version": {
			{Status: 0, Stdout: "container CLI"},
			{Status: 0, Stdout: runningStatusOutput},
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRuntimeRunner{results: results}
			if _, err := ResolveMetadataHost(
				context.Background(), runner,
			); err == nil {
				t.Fatal("expected a version refusal")
			}
		})
	}
}

func TestResolveMetadataHostRequiresRunningServices(t *testing.T) {
	runner := &fakeRuntimeRunner{results: []CommandResult{
		{Status: 0, Stdout: cliVersionOutput},
		{Status: 1, Stdout: "apiserver is not running and not registered with launchd"},
	}}
	if _, err := ResolveMetadataHost(context.Background(), runner); err == nil {
		t.Fatal("expected the resolver to require a running runtime")
	}
}

func TestRequireServicesStoppedNeedsTwoSignals(t *testing.T) {
	stopped := []CommandResult{
		{Status: 1, Stdout: "apiserver is not running and not registered with launchd"},
		{Status: 1, Stderr: "cannot connect"},
	}
	if err := RequireServicesStopped(
		context.Background(), &fakeRuntimeRunner{results: stopped},
	); err != nil {
		t.Fatalf("expected a stopped runtime to be accepted: %v", err)
	}

	for name, results := range map[string][]CommandResult{
		"status still succeeds": {
			{Status: 0, Stdout: runningStatusOutput},
			{Status: 1},
		},
		"data plane still answers": {
			{Status: 1, Stdout: "apiserver is not running"},
			{Status: 0, Stdout: `[]`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := RequireServicesStopped(
				context.Background(), &fakeRuntimeRunner{results: results},
			); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
}
