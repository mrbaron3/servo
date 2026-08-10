package lifecycle

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Phase 2 of Issue #123 sweeps containers that predate the dual-label writer.
// Apple Container 1.1.0 cannot relabel an existing container — `container` has
// no update/relabel verb — so the only way to add the current namespace is to
// recreate the container. That makes this file a migration of identity, not of
// configuration, and every decision below exists to keep it that way:
//
//   - The replacement is rebuilt from the container's own observed record, so
//     nothing but the labels changes. Rebuilding from the topology's
//     specification builders would rebuild images and reseal digests, which is
//     a redeployment wearing a migration's name.
//   - Anything the specification cannot express blocks the container instead of
//     migrating it approximately. A replacement that quietly drops a read-only
//     credential mount or a published socket is a downgrade.
//   - The observed lifecycle state is preserved. A container an operator left
//     stopped is replaced stopped.
//   - Named volumes are never deleted. Apple Container attaches them
//     exclusively, so the previous container must be proven gone before the
//     replacement may attach the same volume.

// MigrationDisposition is the sweep's verdict for one container. Issue #123
// requires migrated, skipped, conflicting, and blocked to stay distinguishable
// in the audit; pending is the same verdict set observed before mutation.
type MigrationDisposition string

const (
	// MigrationPending is an owned legacy-only container that can be rebuilt
	// faithfully and will be migrated when the sweep is applied.
	MigrationPending MigrationDisposition = "pending"
	// MigrationMigrated is a container the sweep replaced with a dual-labeled
	// one and then proved equivalent.
	MigrationMigrated MigrationDisposition = "migrated"
	// MigrationSkipped is a container with nothing to do: already dual or
	// current-only, or not owned by this binary at all.
	MigrationSkipped MigrationDisposition = "skipped"
	// MigrationConflicting is a partially migrated container. It is a hard stop
	// and never a deletion candidate.
	MigrationConflicting MigrationDisposition = "conflicting"
	// MigrationBlocked is an owned legacy-only container the sweep refuses to
	// touch because it cannot prove the replacement would be identical.
	MigrationBlocked MigrationDisposition = "blocked"
)

// VolumeAttachment names one named volume a container has mounted. The host
// path backing the volume is deliberately absent: evidence is durable and
// reviewed away from the machine that produced it, and the volume's name is the
// only part a later phase can act on.
type VolumeAttachment struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"readOnly"`
}

// ContainerInventoryRecord is one bounded, redacted row of the pre-mutation
// snapshot Issue #123 requires. It carries the exact identity the sweep will
// act on, the ownership evidence behind the verdict, and the volume
// attachments a replacement has to reproduce.
type ContainerInventoryRecord struct {
	ID              string               `json:"id"`
	State           string               `json:"state"`
	Image           string               `json:"image"`
	ImageDigest     string               `json:"imageDigest"`
	Ownership       OwnershipClass       `json:"ownership"`
	Role            string               `json:"role"`
	RoleAgreement   LabelAgreement       `json:"roleAgreement"`
	SpecDigest      string               `json:"specDigest"`
	SpecAgreement   LabelAgreement       `json:"specAgreement"`
	OwnershipLabels map[string]string    `json:"ownershipLabels"`
	NamedVolumes    []VolumeAttachment   `json:"namedVolumes"`
	TmpfsMounts     []string             `json:"tmpfsMounts"`
	Networks        []string             `json:"networks"`
	Disposition     MigrationDisposition `json:"disposition"`
	Reason          string               `json:"reason"`
}

// MigrationAudit is the bounded record of one sweep observation.
type MigrationAudit struct {
	Phase   string                       `json:"phase"`
	Totals  map[MigrationDisposition]int `json:"totals"`
	Records []ContainerInventoryRecord   `json:"records"`
}

// HasConflicts reports whether any container is partially migrated. A sweep
// must not mutate anything while this is true: the conflict has to be resolved
// by an operator who can see which namespace holds the stale value.
func (audit MigrationAudit) HasConflicts() bool {
	return audit.Totals[MigrationConflicting] > 0
}

// MigrationTargets returns the records the sweep would act on, in listing
// order. Callers act on these exact identities and re-inspect each one
// immediately before mutating it.
func (audit MigrationAudit) MigrationTargets() []ContainerInventoryRecord {
	targets := make([]ContainerInventoryRecord, 0, len(audit.Records))
	for _, record := range audit.Records {
		if record.Disposition == MigrationPending {
			targets = append(targets, record)
		}
	}
	return targets
}

// ownershipLabelKeys is every label key this migration reads or writes. The
// snapshot records these and nothing else: a foreign label's value is
// arbitrary third-party text that has no business in durable evidence.
var ownershipLabelKeys = []string{
	LegacyManagedLabelKey, CurrentManagedLabelKey,
	LegacyRoleLabelKey, CurrentRoleLabelKey,
	LegacySpecLabelKey, CurrentSpecLabelKey,
}

// BuildMigrationAudit classifies a whole listing into one bounded audit.
func BuildMigrationAudit(
	phase string,
	containers []ContainerActual,
) MigrationAudit {
	audit := MigrationAudit{
		Phase:   phase,
		Totals:  make(map[MigrationDisposition]int),
		Records: make([]ContainerInventoryRecord, 0, len(containers)),
	}
	for _, container := range containers {
		record := InventoryContainer(container)
		audit.Records = append(audit.Records, record)
		audit.Totals[record.Disposition]++
	}
	return audit
}

// InventoryContainer reaches exactly one verdict for one container.
func InventoryContainer(actual ContainerActual) ContainerInventoryRecord {
	labels := actual.Configuration.Labels
	role, roleAgreement := ReadRoleLabel(labels)
	specDigest, specAgreement := ReadSpecLabel(labels)
	record := ContainerInventoryRecord{
		ID:              actual.ID,
		State:           actual.Status.State,
		Image:           actual.Configuration.Image.Reference,
		ImageDigest:     actual.Configuration.Image.Descriptor.Digest,
		Ownership:       ClassifyOwnership(labels),
		Role:            role,
		RoleAgreement:   roleAgreement,
		SpecDigest:      specDigest,
		SpecAgreement:   specAgreement,
		OwnershipLabels: ownershipLabelSubset(labels),
		NamedVolumes:    NamedVolumeAttachments(actual),
		TmpfsMounts:     tmpfsDestinations(actual),
		Networks:        containerNetworks(actual),
	}
	record.Disposition, record.Reason = disposition(actual, record.Ownership)
	return record
}

// disposition maps an ownership class onto the sweep's verdict. Every branch
// returns a reason, because an audit row without one cannot be acted on by the
// operator who reads it weeks later.
func disposition(
	actual ContainerActual,
	class OwnershipClass,
) (MigrationDisposition, string) {
	switch class {
	case OwnershipConflicting:
		return MigrationConflicting, "ownership namespaces disagree; " +
			"resolve the partial migration before sweeping"
	case OwnershipMissingLabel:
		return MigrationSkipped, "no ownership label; not managed by agentopsctl"
	case OwnershipUnmanaged:
		return MigrationSkipped, "ownership label names another deployment"
	case OwnershipDual:
		return MigrationSkipped, "already carries both ownership namespaces"
	case OwnershipCurrentOnly:
		return MigrationSkipped, "already past the legacy namespace"
	case OwnershipLegacyOnly:
		if _, err := RebuildMigratedSpec(actual); err != nil {
			return MigrationBlocked, "cannot rebuild an identical replacement: " +
				err.Error()
		}
		return MigrationPending, "legacy-only; a faithful dual-labeled " +
			"replacement can be rebuilt"
	default:
		// Unreachable while OwnershipClass stays closed, but a new class must
		// fail closed rather than inherit "skip".
		return MigrationBlocked, "unrecognized ownership class"
	}
}

// RebuildMigratedSpec reconstructs the specification that recreates one
// container unchanged except for its ownership labels. It refuses anything it
// cannot express, because the caller's next step deletes the original.
func RebuildMigratedSpec(actual ContainerActual) (ContainerSpec, error) {
	labels := actual.Configuration.Labels
	if err := RequireManaged("container "+actual.ID, labels); err != nil {
		return ContainerSpec{}, err
	}
	role, roleAgreement := ReadRoleLabel(labels)
	if !roleAgreement.Agreed() || strings.TrimSpace(role) == "" {
		return ContainerSpec{}, fmt.Errorf(
			"container %s has no agreed role label", actual.ID,
		)
	}
	specDigest, specAgreement := ReadSpecLabel(labels)
	if specAgreement == LabelConflicting {
		return ContainerSpec{}, fmt.Errorf(
			"container %s has conflicting specification digest labels", actual.ID,
		)
	}
	if len(actual.Configuration.PublishedSock) > 0 {
		return ContainerSpec{}, fmt.Errorf(
			"container %s publishes a socket, which no managed specification "+
				"expresses", actual.ID,
		)
	}
	if len(actual.Configuration.CapAdd) > 0 {
		return ContainerSpec{}, fmt.Errorf(
			"container %s adds Linux capabilities, which are restricted to "+
				"removable volume initialization", actual.ID,
		)
	}
	environment, err := environmentMap(actual)
	if err != nil {
		return ContainerSpec{}, err
	}
	publications, err := loopbackPublications(actual)
	if err != nil {
		return ContainerSpec{}, err
	}
	spec := ContainerSpec{
		Name:        actual.ID,
		Role:        role,
		Image:       strings.TrimSpace(actual.Configuration.Image.Reference),
		SpecDigest:  specDigest,
		Networks:    containerNetworks(actual),
		Environment: environment,
		Publish:     publications,
		Tmpfs:       tmpfsDestinations(actual),
		ReadOnly:    actual.Configuration.ReadOnly,
		CapDropAll:  containsString(actual.Configuration.CapDrop, "ALL"),
		Init:        actual.Configuration.UseInit,
		User:        strings.TrimSpace(actual.Configuration.InitProcess.User.Raw.UserString),
		// Entrypoint and Command are intentionally left to the image. The
		// observed executable is frequently a relative image default, which the
		// specification cannot express; VerifyMigrationEquivalence proves the
		// replacement landed on the same one.
		Detach: true,
	}
	if spec.Image == "" {
		return ContainerSpec{}, fmt.Errorf(
			"container %s has no image reference", actual.ID,
		)
	}
	// Apple Container reports the *effective* entrypoint without saying whether
	// it came from the image or from an override at creation. An absolute one
	// is reproduced explicitly, which is behaviourally identical in both cases.
	// A relative executable ("node") is necessarily an image default, because
	// the specification only accepts an absolute entrypoint, so it is left to
	// the image rather than guessed at. Either way the replacement's effective
	// entrypoint is proven by VerifyMigrationEquivalence, so a container whose
	// entrypoint could not be reproduced is caught rather than accepted.
	executable := strings.TrimSpace(
		actual.Configuration.InitProcess.Executable,
	)
	if strings.HasPrefix(executable, "/") {
		spec.Entrypoint = executable
		spec.Command = append(
			[]string(nil), actual.Configuration.InitProcess.Arguments...,
		)
	}
	for _, attachment := range NamedVolumeAttachments(actual) {
		spec.Mounts = append(spec.Mounts, Mount{
			Volume:   attachment.Name,
			Target:   attachment.Destination,
			ReadOnly: attachment.ReadOnly,
		})
	}
	// Rendering the argv now means an inexpressible container is blocked while
	// the original is still alive, rather than after it has been deleted.
	if _, _, err := containerArgs("create", spec); err != nil {
		return ContainerSpec{}, fmt.Errorf(
			"container %s cannot be recreated identically: %w", actual.ID, err,
		)
	}
	return spec, nil
}

// VerifyMigrationEquivalence proves a replacement differs from the original
// only by gaining the current ownership namespace. It runs after the
// replacement exists and is the gate that turns a delete-and-recreate into a
// migration the operator can trust.
func VerifyMigrationEquivalence(before, after ContainerActual) error {
	if before.ID != after.ID {
		return fmt.Errorf(
			"replacement identity changed from %s to %s", before.ID, after.ID,
		)
	}
	if err := verifyLabelsOnlyGained(before, after); err != nil {
		return err
	}
	for _, comparison := range []struct {
		field  string
		before any
		after  any
	}{
		{"image reference", before.Configuration.Image.Reference,
			after.Configuration.Image.Reference},
		{"image digest", before.Configuration.Image.Descriptor.Digest,
			after.Configuration.Image.Descriptor.Digest},
		{"read-only root", before.Configuration.ReadOnly,
			after.Configuration.ReadOnly},
		{"init process", before.Configuration.UseInit,
			after.Configuration.UseInit},
		{"dropped capabilities", before.Configuration.CapDrop,
			after.Configuration.CapDrop},
		{"added capabilities", before.Configuration.CapAdd,
			after.Configuration.CapAdd},
		{"user", before.Configuration.InitProcess.User,
			after.Configuration.InitProcess.User},
		{"entrypoint", before.Configuration.InitProcess.Executable,
			after.Configuration.InitProcess.Executable},
		{"entrypoint arguments", before.Configuration.InitProcess.Arguments,
			after.Configuration.InitProcess.Arguments},
		{"working directory",
			before.Configuration.InitProcess.WorkingDirectory,
			after.Configuration.InitProcess.WorkingDirectory},
		{"environment", sortedStrings(
			before.Configuration.InitProcess.Environment),
			sortedStrings(after.Configuration.InitProcess.Environment)},
		{"published ports", before.Configuration.PublishedPorts,
			after.Configuration.PublishedPorts},
		{"published sockets", before.Configuration.PublishedSock,
			after.Configuration.PublishedSock},
		{"networks", containerNetworks(before), containerNetworks(after)},
		{"named volumes", NamedVolumeAttachments(before),
			NamedVolumeAttachments(after)},
		{"tmpfs mounts", tmpfsDestinations(before), tmpfsDestinations(after)},
		{"resources", before.Configuration.Resources,
			after.Configuration.Resources},
	} {
		equal, err := canonicallyEqual(comparison.before, comparison.after)
		if err != nil {
			return fmt.Errorf(
				"container %s %s could not be compared: %w",
				before.ID, comparison.field, err,
			)
		}
		if !equal {
			// The differing values are deliberately not echoed: an environment
			// entry or a volume's host path would reach operator output and the
			// durable failure record. The operator compares with
			// `container inspect`, which the runbook prescribes.
			return fmt.Errorf(
				"container %s replacement drifted in %s",
				before.ID, comparison.field,
			)
		}
	}
	return nil
}

// verifyLabelsOnlyGained enforces the Phase 2 label contract in both
// directions: nothing the pre-migration binary reads may be lost, and the
// replacement must actually carry both namespaces. Reaching the current-only
// shape here would be a Phase 3 outcome produced without Phase 3's review.
func verifyLabelsOnlyGained(before, after ContainerActual) error {
	afterLabels := after.Configuration.Labels
	for key, value := range before.Configuration.Labels {
		existing, present := afterLabels[key]
		if !present || existing != value {
			return fmt.Errorf(
				"container %s replacement lost label %s, so the "+
					"pre-migration binary can no longer discover it",
				before.ID, key,
			)
		}
	}
	if class := ClassifyOwnership(afterLabels); class != OwnershipDual {
		return fmt.Errorf(
			"container %s replacement is %s, not dual-labeled",
			before.ID, class,
		)
	}
	return nil
}

// NamedVolumeAttachments lists the named volumes a container has mounted, in
// mount order. Apple Container reports a named volume as a mount whose type
// carries a "volume" object; a tmpfs or bind mount carries neither.
func NamedVolumeAttachments(actual ContainerActual) []VolumeAttachment {
	attachments := make([]VolumeAttachment, 0, len(actual.Configuration.Mounts))
	for _, mount := range actual.Configuration.Mounts {
		volume, ok := mount.Type["volume"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := volume["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		attachments = append(attachments, VolumeAttachment{
			Name:        name,
			Destination: mount.Destination,
			ReadOnly:    containsString(mount.Options, "ro"),
		})
	}
	return attachments
}

func tmpfsDestinations(actual ContainerActual) []string {
	destinations := make([]string, 0, len(actual.Configuration.Mounts))
	for _, mount := range actual.Configuration.Mounts {
		if _, ok := mount.Type["tmpfs"]; !ok {
			continue
		}
		destinations = append(destinations, mount.Destination)
	}
	return destinations
}

func containerNetworks(actual ContainerActual) []string {
	networks := make([]string, 0, len(actual.Configuration.Networks))
	for _, network := range actual.Configuration.Networks {
		networks = append(networks, network.Network)
	}
	return networks
}

// environmentMap turns the observed environment back into specification form.
// The values are carried so the replacement behaves identically; they reach the
// child process environment rather than argv, exactly as the original did.
func environmentMap(actual ContainerActual) (map[string]string, error) {
	environment := make(
		map[string]string,
		len(actual.Configuration.InitProcess.Environment),
	)
	for _, entry := range actual.Configuration.InitProcess.Environment {
		key, value, present := strings.Cut(entry, "=")
		if !present || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf(
				"container %s has an environment entry that is not key=value",
				actual.ID,
			)
		}
		if existing, duplicate := environment[key]; duplicate &&
			existing != value {
			return nil, fmt.Errorf(
				"container %s declares environment key %s twice with "+
					"different values", actual.ID, key,
			)
		}
		environment[key] = value
	}
	return environment, nil
}

// loopbackPublications converts observed published ports into specification
// form. A publication that is not loopback-only is refused rather than
// reproduced: recreating it would re-assert a host exposure this topology
// forbids everywhere else.
func loopbackPublications(actual ContainerActual) ([]Publication, error) {
	publications := make([]Publication, 0, len(actual.Configuration.PublishedPorts))
	for _, published := range actual.Configuration.PublishedPorts {
		hostAddress, _ := published["hostAddress"].(string)
		hostPort, hostOK := numericField(published["hostPort"])
		containerPort, containerOK := numericField(published["containerPort"])
		if !hostOK || !containerOK {
			return nil, fmt.Errorf(
				"container %s publishes a port this migration cannot read",
				actual.ID,
			)
		}
		if hostAddress != "127.0.0.1" {
			return nil, fmt.Errorf(
				"container %s publishes on %s rather than loopback",
				actual.ID, hostAddress,
			)
		}
		publications = append(publications, Publication{
			HostIP:        hostAddress,
			HostPort:      hostPort,
			ContainerPort: containerPort,
		})
	}
	return publications, nil
}

func numericField(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	default:
		return 0, false
	}
}

func ownershipLabelSubset(labels map[string]string) map[string]string {
	subset := make(map[string]string, len(ownershipLabelKeys))
	for _, key := range ownershipLabelKeys {
		if value, present := labels[key]; present {
			subset[key] = value
		}
	}
	return subset
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func sortedStrings(values []string) []string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return sorted
}

// canonicallyEqual compares two observed fields by their JSON encoding. Apple
// Container reports several of them as free-form objects, and Go's encoder
// orders map keys, so this stays stable without a hand-written comparison per
// field.
func canonicallyEqual(before, after any) (bool, error) {
	encodedBefore, err := json.Marshal(before)
	if err != nil {
		return false, err
	}
	encodedAfter, err := json.Marshal(after)
	if err != nil {
		return false, err
	}
	return string(encodedBefore) == string(encodedAfter), nil
}
