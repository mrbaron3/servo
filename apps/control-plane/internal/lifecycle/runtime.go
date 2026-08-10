package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var resourceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var credentialEnvironmentKeyPattern = regexp.MustCompile(
	`(?:^|_)(?:TOKEN|PASSWORD|SECRET|DATABASE_URL|API_KEY|CAPABILITY)$`,
)
var privateCopyHelperPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
var privateCopyOperationPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

type CommandResult struct {
	Status int      `json:"status"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
	Args   []string `json:"args"`
}

type RuntimeRunner interface {
	Run(context.Context, []string) CommandResult
}

type environmentRuntimeRunner interface {
	RunWithEnvironment(context.Context, []string, map[string]string) CommandResult
}

type stdinRuntimeRunner interface {
	RunWithStdin(context.Context, []string, io.Reader) CommandResult
}

type interactiveRuntimeRunner interface {
	RunInteractive(context.Context, []string) error
}

type execRuntimeRunner struct {
	binary string
}

func (runner execRuntimeRunner) Run(ctx context.Context, args []string) CommandResult {
	return runner.run(ctx, args, nil, nil)
}

func (runner execRuntimeRunner) RunWithEnvironment(
	ctx context.Context,
	args []string,
	environment map[string]string,
) CommandResult {
	return runner.run(ctx, args, environment, nil)
}

func (runner execRuntimeRunner) RunWithStdin(
	ctx context.Context,
	args []string,
	stdin io.Reader,
) CommandResult {
	return runner.run(ctx, args, nil, stdin)
}

func (runner execRuntimeRunner) RunInteractive(ctx context.Context, args []string) error {
	process := exec.CommandContext(ctx, runner.binary, args...)
	process.Stdin = os.Stdin
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	return process.Run()
}

func (runner execRuntimeRunner) run(
	ctx context.Context,
	args []string,
	environment map[string]string,
	stdin io.Reader,
) CommandResult {
	command := exec.CommandContext(ctx, runner.binary, args...)
	command.Stdin = stdin
	if len(environment) > 0 {
		values := make(map[string]string)
		for _, item := range os.Environ() {
			key, value, present := strings.Cut(item, "=")
			if present {
				values[key] = value
			}
		}
		for key, value := range environment {
			values[key] = value
		}
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		command.Env = make([]string, 0, len(keys))
		for _, key := range keys {
			command.Env = append(command.Env, key+"="+values[key])
		}
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	status := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			status = exit.ExitCode()
		} else {
			status = -1
			if stderr.Len() == 0 {
				stderr.WriteString(err.Error())
			}
		}
	}
	return CommandResult{
		Status: status, Stdout: stdout.String(), Stderr: stderr.String(),
		Args: append([]string(nil), args...),
	}
}

type AppleRuntime struct {
	runner RuntimeRunner
	binary string
}

func NewAppleRuntime() *AppleRuntime {
	return &AppleRuntime{
		runner: execRuntimeRunner{binary: "container"},
		binary: "container",
	}
}

func NewAppleRuntimeForTest(runner RuntimeRunner) *AppleRuntime {
	return &AppleRuntime{runner: runner, binary: "container"}
}

type Capability struct {
	Available      bool   `json:"available"`
	Version        string `json:"version"`
	ServiceRunning bool   `json:"serviceRunning"`
}

func (runtime *AppleRuntime) Capability(ctx context.Context) Capability {
	version := runtime.runner.Run(ctx, []string{"--version"})
	if version.Status != 0 {
		return Capability{}
	}
	status := runtime.runner.Run(ctx, []string{"system", "status"})
	return Capability{
		Available:      true,
		Version:        strings.TrimSpace(version.Stdout),
		ServiceRunning: status.Status == 0,
	}
}

func (runtime *AppleRuntime) StartSystem(ctx context.Context) error {
	return runtime.command(ctx, []string{"system", "start"}, nil)
}

type Resource struct {
	ID            string `json:"id"`
	Configuration struct {
		Name   string            `json:"name"`
		Mode   string            `json:"mode"`
		Labels map[string]string `json:"labels"`
	} `json:"configuration"`
}

// ContainerMount is one mount as Apple Container reports it. It is a named
// type because callers construct and compare mounts directly; an anonymous
// struct forces every construction site to restate the whole shape, which
// silently breaks the moment the runtime reports one more field.
type ContainerMount struct {
	Destination string `json:"destination"`
	Source      string `json:"source"`
	// Options carries Apple Container's per-mount flags. "ro" here is the only
	// record that a credential volume is mounted read-only, so a migration
	// that ignores it silently widens access to that credential.
	Options []string       `json:"options"`
	Type    map[string]any `json:"type"`
}

// ContainerNetworkAttachment is one network attachment as Apple Container
// reports it. Options carries the per-attachment settings (hostname and MTU on
// 1.1.0). The migration cannot restate them — the specification attaches by
// name — so they are decoded in order to be *compared*: a replacement that
// landed on different attachment options is drift the equivalence gate has to
// see rather than silently accept.
type ContainerNetworkAttachment struct {
	Network string         `json:"network"`
	Options map[string]any `json:"options"`
}

type ContainerActual struct {
	ID            string `json:"id"`
	Configuration struct {
		Labels   map[string]string `json:"labels"`
		ReadOnly bool              `json:"readOnly"`
		UseInit  bool              `json:"useInit"`
		CapAdd   []string          `json:"capAdd"`
		CapDrop  []string          `json:"capDrop"`
		Image    struct {
			Reference  string `json:"reference"`
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"descriptor"`
		} `json:"image"`
		InitProcess struct {
			Environment []string `json:"environment"`
			// Executable, Arguments, and WorkingDirectory are read back only to
			// prove a migrated replacement kept the image's entrypoint. They are
			// never rebuilt from: the observed executable is frequently a
			// relative image default ("node"), which no specification field can
			// express, so the migration leaves them to the image and verifies.
			Executable       string   `json:"executable"`
			Arguments        []string `json:"arguments"`
			WorkingDirectory string   `json:"workingDirectory"`
			User             struct {
				ID struct {
					UID int `json:"uid"`
					GID int `json:"gid"`
				} `json:"id"`
				Raw struct {
					UserString string `json:"userString"`
				} `json:"raw"`
			} `json:"user"`
		} `json:"initProcess"`
		PublishedPorts []map[string]any `json:"publishedPorts"`
		PublishedSock  []map[string]any `json:"publishedSockets"`
		Mounts         []ContainerMount `json:"mounts"`
		// Resources is defaulted by Apple Container rather than requested by any
		// specification. It is compared across a migration so a replacement that
		// landed on different defaults is caught instead of accepted.
		Resources struct {
			CPUs          int     `json:"cpus"`
			MemoryInBytes int64   `json:"memoryInBytes"`
			CPUOverhead   float64 `json:"cpuOverhead"`
		} `json:"resources"`
		Networks []ContainerNetworkAttachment `json:"networks"`
	} `json:"configuration"`
	Status struct {
		State    string `json:"state"`
		Networks []struct {
			Network     string `json:"network"`
			IPv4Address string `json:"ipv4Address"`
		} `json:"networks"`
	} `json:"status"`
}

func (runtime *AppleRuntime) Containers(ctx context.Context) ([]ContainerActual, error) {
	result := runtime.runner.Run(ctx, []string{"list", "--all", "--format", "json"})
	if result.Status != 0 {
		return nil, runtimeError(result, nil)
	}
	var containers []ContainerActual
	if err := json.Unmarshal([]byte(result.Stdout), &containers); err != nil {
		return nil, fmt.Errorf("parse Apple Container list: %w", err)
	}
	return containers, nil
}

func (runtime *AppleRuntime) Container(
	ctx context.Context,
	name string,
) (*ContainerActual, error) {
	containers, err := runtime.Containers(ctx)
	if err != nil {
		return nil, err
	}
	for index := range containers {
		if containers[index].ID == name {
			return &containers[index], nil
		}
	}
	return nil, nil
}

func (runtime *AppleRuntime) EnsureNetwork(
	ctx context.Context,
	name string,
) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	result := runtime.runner.Run(ctx, []string{"network", "list", "--format", "json"})
	if result.Status != 0 {
		return runtimeError(result, nil)
	}
	var resources []Resource
	if err := json.Unmarshal([]byte(result.Stdout), &resources); err != nil {
		return fmt.Errorf("parse Apple Container network list: %w", err)
	}
	for _, resource := range resources {
		resourceName := resource.ID
		if resource.Configuration.Name != "" {
			resourceName = resource.Configuration.Name
		}
		if resourceName != name {
			continue
		}
		if err := RequireOwned(
			"network "+name,
			resource.Configuration.Labels,
		); err != nil {
			return err
		}
		if resource.Configuration.Mode != "" &&
			resource.Configuration.Mode != "host" &&
			resource.Configuration.Mode != "hostOnly" {
			return fmt.Errorf(
				"network %s exists with non-internal mode %q",
				name,
				resource.Configuration.Mode,
			)
		}
		return nil
	}
	args := []string{"network", "create", "--internal"}
	args = append(args, managedOwnershipLabelArgs()...)
	return runtime.command(ctx, append(args, name), nil)
}

// Volumes lists every named volume Apple Container knows about. It is
// read-only, and the Phase 2 label sweep depends on it: after deleting a
// container that held a volume exclusively, the sweep proves the volume still
// exists before attaching it to the replacement.
func (runtime *AppleRuntime) Volumes(ctx context.Context) ([]Resource, error) {
	result := runtime.runner.Run(ctx, []string{"volume", "list", "--format", "json"})
	if result.Status != 0 {
		return nil, runtimeError(result, nil)
	}
	var resources []Resource
	if err := json.Unmarshal([]byte(result.Stdout), &resources); err != nil {
		return nil, fmt.Errorf("parse Apple Container volume list: %w", err)
	}
	return resources, nil
}

func (runtime *AppleRuntime) EnsureVolume(ctx context.Context, name string) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	resources, err := runtime.Volumes(ctx)
	if err != nil {
		return err
	}
	for _, resource := range resources {
		resourceName := resource.ID
		if resource.Configuration.Name != "" {
			resourceName = resource.Configuration.Name
		}
		if resourceName != name {
			continue
		}
		return RequireOwned("volume "+name, resource.Configuration.Labels)
	}
	args := []string{"volume", "create"}
	args = append(args, managedOwnershipLabelArgs()...)
	return runtime.command(ctx, append(args, name), nil)
}

func (runtime *AppleRuntime) ImageExists(ctx context.Context, image string) bool {
	result := runtime.runner.Run(ctx, []string{"image", "inspect", image})
	return result.Status == 0
}

func (runtime *AppleRuntime) ImageDigest(
	ctx context.Context,
	image string,
) (string, error) {
	result := runtime.runner.Run(ctx, []string{"image", "inspect", image})
	if result.Status != 0 {
		return "", runtimeError(result, nil)
	}
	type inspection struct {
		Configuration struct {
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"descriptor"`
		} `json:"configuration"`
	}
	var item inspection
	body := []byte(result.Stdout)
	if len(bytes.TrimSpace(body)) > 0 && bytes.TrimSpace(body)[0] == '[' {
		var items []inspection
		if err := json.Unmarshal(body, &items); err != nil {
			return "", fmt.Errorf("parse Apple Container image inspect: %w", err)
		}
		if len(items) != 1 {
			return "", fmt.Errorf("image inspect returned %d records for %s", len(items), image)
		}
		item = items[0]
	} else if err := json.Unmarshal(body, &item); err != nil {
		return "", fmt.Errorf("parse Apple Container image inspect: %w", err)
	}
	digest := strings.TrimSpace(item.Configuration.Descriptor.Digest)
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", fmt.Errorf("image %s has no immutable sha256 descriptor", image)
	}
	return digest, nil
}

// ImageEnvironment returns the environment an image declares for itself. The
// Phase 2 label migration subtracts it from a container's observed environment
// to recover the values the original specification actually supplied: Apple
// Container reports only the merged result, and carrying an image's own PATH or
// HOME onto the replacement would both over-specify it and displace the host's
// values in the `container` CLI process that creates it.
//
// Variants that disagree are refused rather than guessed between, because
// picking the wrong one would silently change the replacement's environment.
func (runtime *AppleRuntime) ImageEnvironment(
	ctx context.Context,
	image string,
) ([]string, error) {
	result := runtime.runner.Run(ctx, []string{"image", "inspect", image})
	if result.Status != 0 {
		return nil, runtimeError(result, nil)
	}
	type inspection struct {
		Variants []struct {
			Config struct {
				Config struct {
					Env []string `json:"Env"`
				} `json:"config"`
			} `json:"config"`
		} `json:"variants"`
	}
	body := bytes.TrimSpace([]byte(result.Stdout))
	var items []inspection
	if len(body) > 0 && body[0] == '[' {
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("parse Apple Container image inspect: %w", err)
		}
	} else {
		var item inspection
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, fmt.Errorf("parse Apple Container image inspect: %w", err)
		}
		items = []inspection{item}
	}
	var declared []string
	seen := false
	for _, item := range items {
		for _, variant := range item.Variants {
			environment := variant.Config.Config.Env
			if !seen {
				declared = append([]string(nil), environment...)
				seen = true
				continue
			}
			if !equalStrings(declared, environment) {
				return nil, fmt.Errorf(
					"image %s declares different environments per variant",
					image,
				)
			}
		}
	}
	return declared, nil
}

func equalStrings(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func (runtime *AppleRuntime) BuildImage(
	ctx context.Context,
	image, target, containerfile, root string,
) error {
	args := []string{
		"build", "--tag", image, "--file", containerfile,
		"--target", target, root,
	}
	return runtime.command(ctx, args, nil)
}

type Mount struct {
	Volume   string `json:"volume"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly"`
}

type Publication struct {
	HostIP        string `json:"hostIp"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
}

type ContainerSpec struct {
	Name        string
	Role        string
	Image       string
	SpecDigest  string
	Networks    []string
	Environment map[string]string
	Mounts      []Mount
	Publish     []Publication
	Tmpfs       []string
	ReadOnly    bool
	CapDropAll  bool
	CapAdd      []string
	Init        bool
	User        string
	Entrypoint  string
	Command     []string
	Detach      bool
	Remove      bool
}

func SpecDigest(spec ContainerSpec, imageDigest string) (string, error) {
	spec.SpecDigest = ""
	environment := make(map[string]string, len(spec.Environment))
	for key, value := range spec.Environment {
		if CredentialEnvironmentKey(key) {
			environment[key] = "<credential-redacted>"
		} else {
			environment[key] = value
		}
	}
	spec.Environment = environment
	payload := struct {
		Spec        ContainerSpec `json:"spec"`
		ImageDigest string        `json:"imageDigest"`
	}{
		Spec:        spec,
		ImageDigest: imageDigest,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode container specification: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// CredentialEnvironmentKey identifies values which must never be reduced to a
// durable digest. Running actual values are compared only in memory.
func CredentialEnvironmentKey(key string) bool {
	return credentialEnvironmentKeyPattern.MatchString(key)
}

func (runtime *AppleRuntime) RunContainer(
	ctx context.Context,
	spec ContainerSpec,
) (string, error) {
	return runtime.materializeContainer(ctx, "run", spec)
}

// CreateContainer materializes a container without starting it. The Phase 2
// label migration uses this so a container an operator deliberately left
// stopped is replaced in the same stopped state: relabelling is not a reason to
// start a topology, and starting one would begin real work.
func (runtime *AppleRuntime) CreateContainer(
	ctx context.Context,
	spec ContainerSpec,
) (string, error) {
	return runtime.materializeContainer(ctx, "create", spec)
}

func (runtime *AppleRuntime) materializeContainer(
	ctx context.Context,
	verb string,
	spec ContainerSpec,
) (string, error) {
	args, secrets, err := containerArgs(verb, spec)
	if err != nil {
		return "", err
	}
	var result CommandResult
	if runner, ok := runtime.runner.(environmentRuntimeRunner); ok {
		result = runner.RunWithEnvironment(ctx, args, spec.Environment)
	} else {
		result = runtime.runner.Run(ctx, args)
	}
	if result.Status != 0 {
		return "", runtimeError(result, secrets)
	}
	return strings.TrimSpace(result.Stdout), nil
}

func buildContainerArgs(spec ContainerSpec) ([]string, []string, error) {
	return containerArgs("run", spec)
}

// containerArgs renders the argv for one managed container. Apple Container
// 1.1.0 accepts the identical flag surface for `run` and `create`, and the verb
// decides only whether the container starts. Keeping both on one builder is
// what lets the Phase 2 label migration recreate a stopped container without
// starting it: every hardening flag, mount, and label is rendered by the same
// code that produced the original.
func containerArgs(
	verb string,
	spec ContainerSpec,
) ([]string, []string, error) {
	if err := validateResourceName(spec.Name); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(spec.Role) == "" || strings.TrimSpace(spec.Image) == "" {
		return nil, nil, fmt.Errorf("container role and image are required")
	}
	if len(spec.Networks) == 0 && !spec.Remove {
		return nil, nil, fmt.Errorf("long-running container requires an explicit network")
	}
	if spec.Role != "control" && len(spec.Publish) > 0 {
		return nil, nil, fmt.Errorf("%s container must not publish host ports", spec.Role)
	}
	if spec.Role == "control" && spec.Detach {
		if len(spec.Publish) != 1 ||
			spec.Publish[0].HostIP != "127.0.0.1" {
			return nil, nil, fmt.Errorf(
				"control must publish exactly one 127.0.0.1 port",
			)
		}
	}
	if len(spec.CapAdd) > 0 && (spec.Role != "volume-init" || !spec.Remove) {
		return nil, nil, fmt.Errorf(
			"added capabilities are restricted to removable volume initialization",
		)
	}
	args := []string{verb}
	if spec.Detach {
		args = append(args, "--detach")
	}
	if spec.Remove {
		args = append(args, "--rm")
	}
	args = append(args, "--name", spec.Name)
	args = append(args, managedOwnershipLabelArgs()...)
	args = append(args, dualLabelArgs(
		LegacyRoleLabelKey,
		CurrentRoleLabelKey,
		spec.Role,
	)...)
	if spec.SpecDigest != "" {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(spec.SpecDigest) {
			return nil, nil, fmt.Errorf("invalid container specification digest")
		}
		args = append(args, dualLabelArgs(
			LegacySpecLabelKey,
			CurrentSpecLabelKey,
			spec.SpecDigest,
		)...)
	}
	for _, network := range spec.Networks {
		if err := validateResourceName(network); err != nil {
			return nil, nil, err
		}
		args = append(args, "--network", network)
	}
	if spec.Init {
		args = append(args, "--init")
	}
	if spec.ReadOnly {
		args = append(args, "--read-only")
	}
	if spec.CapDropAll {
		args = append(args, "--cap-drop", "ALL")
	}
	for _, capability := range spec.CapAdd {
		if capability != "CAP_CHOWN" {
			return nil, nil, fmt.Errorf("unsupported added capability %q", capability)
		}
		args = append(args, "--cap-add", capability)
	}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	for _, path := range spec.Tmpfs {
		if !strings.HasPrefix(path, "/") || strings.Contains(path, ":") {
			return nil, nil, fmt.Errorf("tmpfs target must be a container-absolute path")
		}
		args = append(args, "--tmpfs", path)
	}
	for _, mount := range spec.Mounts {
		if err := validateResourceName(mount.Volume); err != nil {
			return nil, nil, fmt.Errorf("invalid named volume: %w", err)
		}
		if !strings.HasPrefix(mount.Target, "/") ||
			strings.Contains(mount.Target, ":") ||
			strings.Contains(mount.Target, "..") {
			return nil, nil, fmt.Errorf("mount target must be a safe container-absolute path")
		}
		value := mount.Volume + ":" + mount.Target
		if mount.ReadOnly {
			value += ":ro"
		}
		args = append(args, "--volume", value)
	}
	for _, publication := range spec.Publish {
		if publication.HostIP != "127.0.0.1" ||
			publication.HostPort < 1 || publication.HostPort > 65535 ||
			publication.ContainerPort < 1 || publication.ContainerPort > 65535 {
			return nil, nil, fmt.Errorf("invalid loopback-only publication")
		}
		args = append(args, "--publish", fmt.Sprintf(
			"%s:%d:%d",
			publication.HostIP,
			publication.HostPort,
			publication.ContainerPort,
		))
	}
	keys := make([]string, 0, len(spec.Environment))
	for key := range spec.Environment {
		if !regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`).MatchString(key) {
			return nil, nil, fmt.Errorf("invalid environment key %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	secrets := make([]string, 0, len(keys))
	for _, key := range keys {
		value := spec.Environment[key]
		// Apple Container inherits the value from the CLI process. Keep secret
		// values out of argv/process listings by passing only the key here.
		args = append(args, "--env", key)
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	if spec.Entrypoint != "" {
		if !strings.HasPrefix(spec.Entrypoint, "/") {
			return nil, nil, fmt.Errorf("entrypoint must be container-absolute")
		}
		args = append(args, "--entrypoint", spec.Entrypoint)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Command...)
	return args, secrets, nil
}

func (runtime *AppleRuntime) SignalTerm(ctx context.Context, name string) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	return runtime.command(ctx, []string{"kill", "--signal", "TERM", name}, nil)
}

func (runtime *AppleRuntime) Stop(ctx context.Context, name string, seconds int) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	return runtime.command(ctx, []string{
		"stop", "--time", strconv.Itoa(seconds), name,
	}, nil)
}

func (runtime *AppleRuntime) Delete(ctx context.Context, name string) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	actual, err := runtime.Container(ctx, name)
	if err != nil || actual == nil {
		return err
	}
	if err := RequireManaged(
		"container "+name,
		actual.Configuration.Labels,
	); err != nil {
		return err
	}
	return runtime.command(ctx, []string{"delete", name}, nil)
}

func (runtime *AppleRuntime) Exec(
	ctx context.Context,
	name string,
	command ...string,
) CommandResult {
	args := append([]string{"exec", name}, command...)
	return runtime.runner.Run(ctx, args)
}

// InteractiveExec attaches the caller's terminal to a bounded command in a
// managed container. Callers must validate container/workdir identities before
// crossing this UI boundary.
func (runtime *AppleRuntime) InteractiveExec(
	ctx context.Context,
	name string,
	workdir string,
	command ...string,
) error {
	args := []string{"exec", "--interactive", "--tty", "--workdir", workdir, name}
	args = append(args, command...)
	if runner, ok := runtime.runner.(interactiveRuntimeRunner); ok {
		return runner.RunInteractive(ctx, args)
	}
	return fmt.Errorf("interactive execution is unavailable")
}

// CopyFileToContainer copies one validated regular file into a running managed
// container. The host source path is intentionally omitted from returned
// errors: it can disclose operator directory structure and is never useful to
// durable evidence.
func (runtime *AppleRuntime) CopyFileToContainer(
	ctx context.Context,
	name, source, destination string,
) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("credential source is unavailable")
	}
	if !filepath.IsAbs(source) || !info.Mode().IsRegular() {
		return fmt.Errorf("credential source must be an absolute regular file")
	}
	if !regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`).MatchString(destination) ||
		strings.Contains(destination, "..") ||
		strings.Contains(destination, "//") {
		return fmt.Errorf("copy destination must be a safe container-absolute path")
	}
	sourceFile, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("credential source is unavailable")
	}
	defer sourceFile.Close()
	stdinRunner, ok := runtime.runner.(stdinRuntimeRunner)
	if !ok {
		return fmt.Errorf("container runtime does not support private stdin copy")
	}
	result := stdinRunner.RunWithStdin(ctx, []string{
		"exec", "--interactive", "--user", "65532:65532", name,
		"/bin/sh", "-c",
		"rm -f " + destination + "; umask 077; cat > " + destination +
			" && chmod 0400 " + destination,
	}, sourceFile)
	if result.Status != 0 {
		result.Args = []string{
			"exec", "--interactive", "--user", "65532:65532", name,
			"/bin/sh", "-c", "***private-stdin-copy***",
		}
		return runtimeError(result, nil)
	}
	return nil
}

// CopyPrivateFileWithHelper streams one validated host file to a fixed-purpose
// helper inside a running managed container. Unlike CopyFileToContainer, this
// works with shell-free distroless images. Neither the host path nor stdin is
// retained in argv, logs, or returned errors.
func (runtime *AppleRuntime) CopyPrivateFileWithHelper(
	ctx context.Context,
	name, source, helper, operation string,
) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("credential source is unavailable")
	}
	if !filepath.IsAbs(source) || !info.Mode().IsRegular() {
		return fmt.Errorf("credential source must be an absolute regular file")
	}
	if !privateCopyHelperPattern.MatchString(helper) ||
		strings.Contains(helper, "..") ||
		!privateCopyOperationPattern.MatchString(operation) {
		return fmt.Errorf("private copy helper invocation is invalid")
	}
	sourceFile, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("credential source is unavailable")
	}
	defer sourceFile.Close()
	stdinRunner, ok := runtime.runner.(stdinRuntimeRunner)
	if !ok {
		return fmt.Errorf("container runtime does not support private stdin copy")
	}
	result := stdinRunner.RunWithStdin(ctx, []string{
		"exec", "--interactive", "--user", "65532:65532", name,
		helper, operation,
	}, sourceFile)
	if result.Status != 0 {
		result.Args = []string{
			"exec", "--interactive", "--user", "65532:65532", name,
			helper, "***private-stdin-copy***",
		}
		return runtimeError(result, nil)
	}
	return nil
}

func (runtime *AppleRuntime) WaitState(
	ctx context.Context,
	name, expected string,
	interval time.Duration,
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		actual, err := runtime.Container(ctx, name)
		if err != nil {
			return err
		}
		state := "absent"
		if actual != nil {
			state = actual.Status.State
		}
		if state == expected {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for container %s=%s: %w", name, expected, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (runtime *AppleRuntime) FollowLogs(
	ctx context.Context,
	name string,
	lines int,
	follow bool,
) error {
	args := []string{"logs", "-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, name)
	command := exec.CommandContext(ctx, runtime.binary, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func (runtime *AppleRuntime) RecentLogs(
	ctx context.Context,
	name string,
	lines int,
) CommandResult {
	if err := validateResourceName(name); err != nil {
		return CommandResult{Status: -1, Stderr: err.Error()}
	}
	if lines < 1 || lines > 500 {
		return CommandResult{Status: -1, Stderr: "log line count must be 1..500"}
	}
	return runtime.runner.Run(ctx, []string{
		"logs", "-n", strconv.Itoa(lines), name,
	})
}

func (runtime *AppleRuntime) command(
	ctx context.Context,
	args []string,
	secrets []string,
) error {
	result := runtime.runner.Run(ctx, args)
	if result.Status != 0 {
		return runtimeError(result, secrets)
	}
	return nil
}

func runtimeError(result CommandResult, secrets []string) error {
	args := redactArgs(result.Args)
	output := redactText(result.Stdout+result.Stderr, secrets)
	return fmt.Errorf(
		"container %s failed (status=%d): %s",
		strings.Join(args, " "),
		result.Status,
		strings.TrimSpace(output),
	)
}

func redactArgs(args []string) []string {
	redacted := append([]string(nil), args...)
	for index := 0; index < len(redacted)-1; index++ {
		if redacted[index] == "--env" {
			key, _, _ := strings.Cut(redacted[index+1], "=")
			redacted[index+1] = key + "=***"
			index++
		}
	}
	return redacted
}

func redactText(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	return value
}

func validateResourceName(name string) error {
	if !resourceNamePattern.MatchString(name) {
		return fmt.Errorf(
			"resource name %q is invalid; host paths and bind sources are forbidden",
			name,
		)
	}
	return nil
}
