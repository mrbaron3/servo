package lifecycle

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Editing Apple Container's own metadata is only defensible against a runtime
// whose on-disk layout this code has actually examined. Everything in this file
// exists to prove that the host in front of the tool is that runtime, and to
// prove its services are down before a single byte is written.

// SupportedAppleContainerVersions is the exact allowlist this migration will
// run against. It is an allowlist rather than a minimum because the layout is
// not a published contract: a later release may move labels, add a third copy,
// or change the encoding, and the correct response to an unrecognised version is
// to stop and re-examine the layout by hand.
var SupportedAppleContainerVersions = []string{"1.1.0"}

// MetadataHost is the verified runtime this migration may operate on.
type MetadataHost struct {
	// AppRoot is an absolute path under the operator's home directory and is
	// deliberately not serialised: the evidence this migration writes is
	// committed to the repository.
	AppRoot          string `json:"-"`
	CLIVersion       string `json:"cliVersion"`
	APIServerVersion string `json:"apiServerVersion"`
}

var semanticVersion = regexp.MustCompile(`version (\d+\.\d+\.\d+)`)

// ResolveMetadataHost reads the runtime's own report of where it keeps its
// state and which build is serving it. The application root is taken from
// `container system status` rather than assumed, because a hardcoded path is
// exactly the kind of Mac-absolute assumption this project treats as a
// regression, and because the value has to come from the running service that
// owns those files.
//
// This must run while the services are up: a stopped apiserver reports nothing
// at all, so the host has to be identified before it is shut down.
func ResolveMetadataHost(
	ctx context.Context,
	runner RuntimeRunner,
) (*MetadataHost, error) {
	version := runner.Run(ctx, []string{"--version"})
	if version.Status != 0 {
		return nil, fmt.Errorf(
			"Apple Container CLI is not available: %s",
			strings.TrimSpace(version.Stderr),
		)
	}
	status := runner.Run(ctx, []string{"system", "status"})
	if status.Status != 0 {
		return nil, fmt.Errorf(
			"Apple Container services are not running; start them with " +
				"`container system start` so the migration can read the " +
				"application root and inventory before it stops them",
		)
	}
	host := &MetadataHost{
		CLIVersion: extractVersion(version.Stdout),
	}
	for _, line := range strings.Split(status.Stdout, "\n") {
		name, value, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}
		// The value is taken as the whole remainder rather than the next
		// whitespace-separated field: the default application root contains
		// "Application Support".
		value = strings.TrimSpace(value)
		switch name {
		case "appRoot":
			host.AppRoot = strings.TrimRight(value, "/")
		case "apiserver.version":
			host.APIServerVersion = extractVersion(value)
		}
	}
	if host.AppRoot == "" {
		return nil, fmt.Errorf(
			"`container system status` did not report an application root",
		)
	}
	if err := requireSupportedVersion(
		"container CLI", host.CLIVersion,
	); err != nil {
		return nil, err
	}
	if err := requireSupportedVersion(
		"container apiserver", host.APIServerVersion,
	); err != nil {
		return nil, err
	}
	return host, nil
}

func extractVersion(text string) string {
	if match := semanticVersion.FindStringSubmatch(text); len(match) == 2 {
		return match[1]
	}
	return ""
}

func requireSupportedVersion(subject, version string) error {
	for _, supported := range SupportedAppleContainerVersions {
		if version == supported {
			return nil
		}
	}
	return fmt.Errorf(
		"%s reports version %q; this migration edits Apple Container's own "+
			"metadata and only runs against %s",
		subject,
		version,
		strings.Join(SupportedAppleContainerVersions, ", "),
	)
}

// RequireServicesStopped proves the runtime is down before its files are
// rewritten. Two independent signals are required: the status command has to
// report the apiserver as absent, and a data-plane call has to fail. Asking only
// the status command would accept a half-stopped runtime that is still serving
// reads and still holding its own view of the labels in memory.
func RequireServicesStopped(
	ctx context.Context,
	runner RuntimeRunner,
) error {
	// The CLI itself has to work, or the two failures below prove nothing. A
	// missing binary, a broken install, or a permission problem makes every
	// command fail, and "everything failed" is not evidence that a service
	// stopped — it is evidence that nothing can be asked.
	version := runner.Run(ctx, []string{"--version"})
	if version.Status != 0 {
		return fmt.Errorf(
			"the Apple Container CLI is not usable, so a failing `system " +
				"status` proves nothing about whether the runtime is stopped",
		)
	}
	status := runner.Run(ctx, []string{"system", "status"})
	if status.Status == 0 {
		return fmt.Errorf(
			"Apple Container services are still running; the migration will " +
				"not edit metadata underneath a live apiserver",
		)
	}
	// A positive marker, not merely a nonzero exit: 1.1.0 reports the stopped
	// apiserver in words, and requiring them distinguishes "stopped" from
	// "the command failed for some other reason".
	reported := strings.ToLower(status.Stdout + " " + status.Stderr)
	if !strings.Contains(reported, "not running") &&
		!strings.Contains(reported, "not registered") {
		return fmt.Errorf(
			"`container system status` failed without reporting the apiserver " +
				"as stopped, so the runtime's state is unknown; resolve that " +
				"before editing its metadata",
		)
	}
	probe := runner.Run(ctx, []string{"volume", "list", "--format", "json"})
	if probe.Status == 0 {
		return fmt.Errorf(
			"Apple Container still answers volume queries after `system " +
				"stop`; refusing to edit metadata while the runtime is up",
		)
	}
	return nil
}

// StopSystem stops every Apple Container service.
func (runtime *AppleRuntime) StopSystem(ctx context.Context) error {
	return runtime.command(ctx, []string{"system", "stop"}, nil)
}

// ResolveMetadataHost identifies the runtime this migration may operate on.
func (runtime *AppleRuntime) ResolveMetadataHost(
	ctx context.Context,
) (*MetadataHost, error) {
	return ResolveMetadataHost(ctx, runtime.runner)
}

// RequireServicesStopped proves the runtime is down before its files change.
func (runtime *AppleRuntime) RequireServicesStopped(ctx context.Context) error {
	return RequireServicesStopped(ctx, runtime.runner)
}
