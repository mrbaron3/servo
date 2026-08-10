package lifecycle

import (
	"errors"
	"fmt"
)

// Container ownership labels are compatibility identifiers, not display names.
// ADR-0022 and Issue #123 migrate them from the legacy `com.mrbaron3.workflow`
// namespace to `com.mrbaron3.servo` in three phases; this file is their single
// home so writer, reader, and selector can never move independently.
//
// Phase 1 (this file) writes both namespaces on every newly managed resource
// and reads either one. Two consequences are load-bearing:
//
//   - Rollback: a resource created here still carries the complete legacy
//     triple, so a pre-migration binary — which reads the legacy keys only —
//     keeps discovering it. Apple Container attaches a named volume
//     exclusively, so a resource that becomes invisible to the running binary
//     cannot be replaced either.
//   - Fail-closed: a resource whose two namespaces disagree is partially
//     migrated, not foreign. It is reported as a distinct failure and never as
//     an unowned resource, because "unowned" is what later phases use to decide
//     a name belongs to somebody else.
const (
	LegacyLabelNamespace  = "com.mrbaron3.workflow"
	CurrentLabelNamespace = "com.mrbaron3.servo"

	LegacyManagedLabelKey  = LegacyLabelNamespace + ".agentopsctl"
	LegacyRoleLabelKey     = LegacyLabelNamespace + ".role"
	LegacySpecLabelKey     = LegacyLabelNamespace + ".spec-sha256"
	CurrentManagedLabelKey = CurrentLabelNamespace + ".agentopsctl"
	CurrentRoleLabelKey    = CurrentLabelNamespace + ".role"
	CurrentSpecLabelKey    = CurrentLabelNamespace + ".spec-sha256"

	ManagedLabelValue = "v1"
)

// LabelAgreement describes how one dual-written label pair appears on a
// resource. It is deliberately independent of the label's meaning so the
// ownership marker, the role, and the specification digest are all resolved
// through the same presence/conflict rules.
type LabelAgreement string

const (
	// LabelAbsent means neither namespace carries the key.
	LabelAbsent LabelAgreement = "absent"
	// LabelLegacyOnly means only `com.mrbaron3.workflow.*` carries the key,
	// which is every resource created before Phase 1.
	LabelLegacyOnly LabelAgreement = "legacy-only"
	// LabelCurrentOnly means only `com.mrbaron3.servo.*` carries the key, which
	// is what Phase 3 leaves behind.
	LabelCurrentOnly LabelAgreement = "current-only"
	// LabelDual means both namespaces carry the same value.
	LabelDual LabelAgreement = "dual"
	// LabelConflicting means both namespaces carry the key with different
	// values, including a half-written pair where one side is empty.
	LabelConflicting LabelAgreement = "conflicting"
)

// Agreed reports whether the two namespaces can be read as a single value.
func (agreement LabelAgreement) Agreed() bool {
	switch agreement {
	case LabelLegacyOnly, LabelCurrentOnly, LabelDual:
		return true
	default:
		return false
	}
}

// OwnershipClass is the ownership verdict for one runtime resource. Every case
// Issue #123 enumerates is explicit so later phases classify rather than infer.
type OwnershipClass string

const (
	// OwnershipMissingLabel means the resource carries no ownership key at all.
	OwnershipMissingLabel OwnershipClass = "missing-label"
	// OwnershipUnmanaged means an ownership key is present but names a value
	// this binary does not manage, so the name belongs to another deployment.
	OwnershipUnmanaged OwnershipClass = "unmanaged"
	// OwnershipLegacyOnly means the resource predates Phase 1 and is owned.
	OwnershipLegacyOnly OwnershipClass = "legacy-only"
	// OwnershipCurrentOnly means the resource carries only the current
	// namespace and is owned.
	OwnershipCurrentOnly OwnershipClass = "current-only"
	// OwnershipDual means both namespaces agree the resource is owned.
	OwnershipDual OwnershipClass = "dual"
	// OwnershipConflicting means the two namespaces disagree. The resource is
	// partially migrated and must stop the caller.
	OwnershipConflicting OwnershipClass = "conflicting"
)

// Owned reports whether the class proves this binary owns the resource.
// Conflicting is never owned and never unowned: it is a hard stop.
func (class OwnershipClass) Owned() bool {
	switch class {
	case OwnershipLegacyOnly, OwnershipCurrentOnly, OwnershipDual:
		return true
	default:
		return false
	}
}

// ReadDualLabel resolves one dual-written label pair into a single value plus
// the agreement that produced it. The value is meaningful only when the
// agreement reports Agreed.
func ReadDualLabel(
	labels map[string]string,
	legacyKey, currentKey string,
) (string, LabelAgreement) {
	legacy, legacyPresent := labels[legacyKey]
	current, currentPresent := labels[currentKey]
	switch {
	case !legacyPresent && !currentPresent:
		return "", LabelAbsent
	case legacyPresent && !currentPresent:
		return legacy, LabelLegacyOnly
	case !legacyPresent && currentPresent:
		return current, LabelCurrentOnly
	case legacy == current:
		return legacy, LabelDual
	default:
		return "", LabelConflicting
	}
}

// ReadRoleLabel resolves the runtime role recorded on a managed resource.
func ReadRoleLabel(labels map[string]string) (string, LabelAgreement) {
	return ReadDualLabel(labels, LegacyRoleLabelKey, CurrentRoleLabelKey)
}

// ReadSpecLabel resolves the sealed specification digest recorded on a managed
// container.
func ReadSpecLabel(labels map[string]string) (string, LabelAgreement) {
	return ReadDualLabel(labels, LegacySpecLabelKey, CurrentSpecLabelKey)
}

// ClassifyOwnership resolves the ownership contract of one runtime resource
// from its labels alone. It never consults the resource name: a name collision
// with another deployment has to be distinguishable from ownership.
func ClassifyOwnership(labels map[string]string) OwnershipClass {
	value, agreement := ReadDualLabel(
		labels,
		LegacyManagedLabelKey,
		CurrentManagedLabelKey,
	)
	switch agreement {
	case LabelAbsent:
		return OwnershipMissingLabel
	case LabelConflicting:
		return OwnershipConflicting
	}
	if value != ManagedLabelValue {
		return OwnershipUnmanaged
	}
	switch agreement {
	case LabelLegacyOnly:
		return OwnershipLegacyOnly
	case LabelCurrentOnly:
		return OwnershipCurrentOnly
	default:
		return OwnershipDual
	}
}

// ErrConflictingLabels marks every failure caused by a resource whose two label
// namespaces disagree. Callers match it with errors.Is so a partial migration is
// never reported through a remediation that assumes ordinary drift — telling an
// operator to stop and recreate a container is the opposite of what a partially
// migrated one needs.
var ErrConflictingLabels = errors.New(
	"partial container label migration; resolve it before continuing",
)

// RequireOwned returns nil when either namespace proves this binary owns the
// resource. A partially migrated resource is reported as a conflict rather than
// as a foreign one, so no caller can mistake it for a name owned elsewhere.
func RequireOwned(subject string, labels map[string]string) error {
	class := ClassifyOwnership(labels)
	if class.Owned() {
		return nil
	}
	if class == OwnershipConflicting {
		return conflictingLabelError(
			subject, "ownership",
			LegacyManagedLabelKey, CurrentManagedLabelKey,
		)
	}
	return fmt.Errorf("%s is not owned by agentopsctl", subject)
}

// RequireManaged is the gate for stopping, signalling, or deleting a container.
// It proves ownership and additionally proves that no label pair is
// half-written: a container whose role or specification namespaces disagree is
// as partially migrated as one whose ownership marker does, and a destructive
// path is exactly where that has to stop the caller.
func RequireManaged(subject string, labels map[string]string) error {
	if err := RequireOwned(subject, labels); err != nil {
		return err
	}
	for _, pair := range []struct {
		kind                  string
		legacyKey, currentKey string
	}{
		{"role", LegacyRoleLabelKey, CurrentRoleLabelKey},
		{"specification digest", LegacySpecLabelKey, CurrentSpecLabelKey},
	} {
		if _, agreement := ReadDualLabel(
			labels,
			pair.legacyKey,
			pair.currentKey,
		); agreement == LabelConflicting {
			return conflictingLabelError(
				subject, pair.kind, pair.legacyKey, pair.currentKey,
			)
		}
	}
	return nil
}

// RequireRole returns nil when both namespaces agree the resource carries the
// wanted runtime role. Like RequireOwned it keeps the key pair inside this file
// so Phase 3 can drop the legacy namespace without editing any caller.
func RequireRole(subject, want string, labels map[string]string) error {
	role, agreement := ReadRoleLabel(labels)
	if agreement == LabelConflicting {
		return conflictingLabelError(
			subject, "role",
			LegacyRoleLabelKey, CurrentRoleLabelKey,
		)
	}
	if !agreement.Agreed() || role != want {
		// Ownership is checked separately, so naming it here would send the
		// operator to a boundary that is already known to be sound.
		return fmt.Errorf("%s role label does not match %q", subject, want)
	}
	return nil
}

// RequireSpecDigest returns nil when both namespaces agree the resource was
// sealed with the wanted specification digest. A disagreement between the two
// namespaces is a partial migration, not image or runtime drift, and the two
// have opposite remediations.
func RequireSpecDigest(subject, want string, labels map[string]string) error {
	sealed, agreement := ReadSpecLabel(labels)
	if agreement == LabelConflicting {
		return conflictingLabelError(
			subject, "specification digest",
			LegacySpecLabelKey, CurrentSpecLabelKey,
		)
	}
	if !agreement.Agreed() || sealed != want {
		return fmt.Errorf(
			"%s immutable image or runtime specification drifted",
			subject,
		)
	}
	return nil
}

// conflictingLabelError renders the fail-closed reason a partially migrated
// resource stops the caller. It names the two keys but never echoes their
// values: a label value is attacker- or accident-supplied text that reaches
// operator output and the durable lifecycle failure record, and that record
// only redacts credentials it already knows. The operator resolves the conflict
// with `container inspect`, which the runbook prescribes.
func conflictingLabelError(subject, kind, legacyKey, currentKey string) error {
	return fmt.Errorf(
		"%s carries conflicting %s labels: %s and %s disagree; %w",
		subject, kind, legacyKey, currentKey, ErrConflictingLabels,
	)
}

// dualLabelArgs renders the `--label` arguments that write one value into both
// namespaces. Every managed resource is created through this helper so a
// namespace can never be added to the writer on one path only.
func dualLabelArgs(legacyKey, currentKey, value string) []string {
	return []string{
		"--label", legacyKey + "=" + value,
		"--label", currentKey + "=" + value,
	}
}

// managedOwnershipLabelArgs renders the ownership marker written on every
// container, network, and volume this binary creates.
func managedOwnershipLabelArgs() []string {
	return dualLabelArgs(
		LegacyManagedLabelKey,
		CurrentManagedLabelKey,
		ManagedLabelValue,
	)
}
