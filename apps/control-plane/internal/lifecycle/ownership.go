package lifecycle

import (
	"errors"
	"fmt"
	"strings"
)

// Container ownership labels are compatibility identifiers, not display names.
// ADR-0022 and Issue #123 moved them from the legacy `com.mrbaron3.workflow`
// namespace to `com.mrbaron3.servo` in phases; this file is their single home so
// writer, reader, and selector can never move independently.
//
// Phase 3B is the end of that migration. `com.mrbaron3.servo` is now the sole
// ownership namespace: nothing in this binary reads, writes, or classifies the
// legacy keys, and no code path can adopt a resource that carries them alone.
// Phase 3A had already stopped writing them, and its sweep proved every managed
// resource on the host carries the current namespace, which is what made the
// reader safe to narrow.
//
// The names below still say "current" rather than dropping the qualifier. That
// is deliberate: the committed evidence, the runbook's classification table, and
// the retained Phase 3A rollback plans all spell it that way, and renaming here
// would turn a phase whose whole content is a removal into a rename a reviewer
// has to read twice to see what actually went away.
//
// Two consequences are load-bearing, and they point in opposite directions:
//
//   - Not adopting. A resource this binary cannot prove it owns is refused, never
//     claimed. Every resource that still carries the legacy namespace alone is now
//     unreadable and classifies as missing-label, so callers stop instead of
//     reusing the name. Refusing is recoverable; adopting is not, because Apple
//     Container attaches a named volume exclusively and a wrongly adopted volume
//     is somebody else's data.
//   - Failing closed. A resource whose current labels are half-written is not
//     reported as somebody else's. "Unmanaged" means the name belongs to another
//     deployment, and a partially labelled resource of ours is the one case where
//     that reading is both wrong and expensive — it is what later logic uses to
//     decide a name is free.
//
// Rolling back past Phase 3B is a deliberate operational choice, not a fallback
// this code preserves: it requires the retained private Phase 3A backups and a
// pre-Phase-3B binary. See docs/runbooks/container-label-migration.md.
const (
	CurrentLabelNamespace = "com.mrbaron3.servo"

	CurrentManagedLabelKey = CurrentLabelNamespace + ".agentopsctl"
	CurrentRoleLabelKey    = CurrentLabelNamespace + ".role"
	CurrentSpecLabelKey    = CurrentLabelNamespace + ".spec-sha256"

	ManagedLabelValue = "v1"
)

// ownershipLabelKeys is every label key this binary reads or writes. Durable
// evidence records these and nothing else: a foreign label's value is arbitrary
// third-party text that has no business in an audit trail.
var ownershipLabelKeys = []string{
	CurrentManagedLabelKey, CurrentRoleLabelKey, CurrentSpecLabelKey,
}

// LabelPresence describes how one ownership label appears on a resource. It is
// deliberately independent of the label's meaning so the ownership marker, the
// role, and the specification digest are all resolved through the same rules.
type LabelPresence string

const (
	// LabelAbsent means the key is not written at all.
	LabelAbsent LabelPresence = "absent"
	// LabelPresent means the key is written with a value that can be read.
	LabelPresent LabelPresence = "present"
	// LabelBlank means the key is written with an empty or whitespace-only
	// value. That is a half-written label, not a value: no writer emits one
	// deliberately, so it is treated as a resource caught mid-creation rather
	// than as a marker naming another deployment.
	LabelBlank LabelPresence = "blank"
)

// Readable reports whether the label resolved to a value a caller may act on.
func (presence LabelPresence) Readable() bool {
	return presence == LabelPresent
}

// OwnershipClass is the ownership verdict for one runtime resource. Every case
// is explicit so callers classify rather than infer.
type OwnershipClass string

const (
	// OwnershipMissingLabel means the resource carries no ownership key at all.
	// Since Phase 3B this also covers every resource whose only ownership
	// labels are in the retired legacy namespace.
	OwnershipMissingLabel OwnershipClass = "missing-label"
	// OwnershipUnmanaged means the ownership key is present but names a value
	// this binary does not manage, so the name belongs to another deployment.
	OwnershipUnmanaged OwnershipClass = "unmanaged"
	// OwnershipOwned means the current namespace proves this binary owns the
	// resource.
	OwnershipOwned OwnershipClass = "owned"
	// OwnershipMalformed means the current namespace is present but incoherent.
	// The resource is neither owned nor foreign, and it must stop the caller.
	OwnershipMalformed OwnershipClass = "malformed"
)

// Owned reports whether the class proves this binary owns the resource.
// Malformed is never owned and never unowned: it is a hard stop.
func (class OwnershipClass) Owned() bool {
	return class == OwnershipOwned
}

// ReadOwnershipLabel resolves one ownership label into a value plus how it
// appeared. The value is meaningful only when the presence reports Readable.
func ReadOwnershipLabel(
	labels map[string]string,
	key string,
) (string, LabelPresence) {
	value, present := labels[key]
	switch {
	case !present:
		return "", LabelAbsent
	case strings.TrimSpace(value) == "":
		return "", LabelBlank
	default:
		return value, LabelPresent
	}
}

// ReadRoleLabel resolves the runtime role recorded on a managed resource.
func ReadRoleLabel(labels map[string]string) (string, LabelPresence) {
	return ReadOwnershipLabel(labels, CurrentRoleLabelKey)
}

// ReadSpecLabel resolves the sealed specification digest recorded on a managed
// container.
func ReadSpecLabel(labels map[string]string) (string, LabelPresence) {
	return ReadOwnershipLabel(labels, CurrentSpecLabelKey)
}

// ClassifyOwnership resolves the ownership contract of one runtime resource
// from its labels alone. It never consults the resource name: a name collision
// with another deployment has to be distinguishable from ownership.
func ClassifyOwnership(labels map[string]string) OwnershipClass {
	value, presence := ReadOwnershipLabel(labels, CurrentManagedLabelKey)
	if presence == LabelBlank {
		return OwnershipMalformed
	}
	// A blank ancillary label is checked BEFORE the marker's value is judged, and
	// the order is the point. A resource carrying an unknown marker beside a
	// half-written role used to classify as unmanaged — "somebody else's name" —
	// which is the one reading a half-written resource must never get, and which
	// also dropped it out of the pre-stop gate.
	if carriesBlankOwnershipLabel(labels) {
		return OwnershipMalformed
	}
	if presence == LabelAbsent {
		// A resource carrying this namespace's role or specification digest but
		// no ownership marker was labelled by this binary and then interrupted.
		// Reporting it as missing-label would make it indistinguishable from a
		// resource nobody owns, which is the reading that lets its name be reused.
		if carriesOwnershipLabel(labels) {
			return OwnershipMalformed
		}
		return OwnershipMissingLabel
	}
	if value != ManagedLabelValue {
		return OwnershipUnmanaged
	}
	return OwnershipOwned
}

// carriesOwnershipLabel reports whether any ownership key other than the managed
// marker is present, whatever its value.
func carriesOwnershipLabel(labels map[string]string) bool {
	for _, key := range []string{CurrentRoleLabelKey, CurrentSpecLabelKey} {
		if _, present := labels[key]; present {
			return true
		}
	}
	return false
}

// carriesBlankOwnershipLabel reports whether an ancillary ownership key is
// written with an empty value, which no writer does deliberately.
func carriesBlankOwnershipLabel(labels map[string]string) bool {
	for _, key := range []string{CurrentRoleLabelKey, CurrentSpecLabelKey} {
		if _, presence := ReadOwnershipLabel(labels, key); presence == LabelBlank {
			return true
		}
	}
	return false
}

// ErrMalformedOwnershipLabels marks every failure caused by a resource whose
// current ownership labels are incomplete. Callers match it with errors.Is so a
// half-written resource is never reported through a remediation that assumes
// ordinary drift — telling an operator to stop and recreate a container is the
// opposite of what a half-labelled one needs.
var ErrMalformedOwnershipLabels = errors.New(
	"incomplete container ownership labels; resolve them before continuing",
)

// RequireOwned returns nil when the current namespace proves this binary owns
// the resource. A half-labelled resource is reported as malformed rather than as
// a foreign one, so no caller can mistake it for a name owned elsewhere.
func RequireOwned(subject string, labels map[string]string) error {
	class := ClassifyOwnership(labels)
	if class.Owned() {
		return nil
	}
	if class == OwnershipMalformed {
		return malformedLabelError(
			subject, "ownership", describeMalformedOwnership(labels),
		)
	}
	return fmt.Errorf("%s is not owned by agentopsctl", subject)
}

// RequireManaged is the gate for stopping, signalling, or deleting a container.
// Every caller resolves its subject through AppleRuntime.Container first, so
// "managed" here means specifically a managed CONTAINER: volumes and networks
// prove ownership with RequireOwned, which is the weaker gate they need.
//
// The distinction is the point. containerArgs refuses a container specification
// without a role and then writes the role label unconditionally, so a container
// carrying the marker alone is not a shape this binary can produce — it is one
// whose labelling was interrupted, or one a metadata edit left half-written.
// Volumes and networks legitimately carry the marker alone, which is why
// ClassifyOwnership cannot demand a role and this gate has to.
//
// The per-label loop below is redundant with the classifier and is kept
// deliberately: it names WHICH label is at fault, and a caller reading
// "ownership labels are incomplete" on a container with a valid marker has no
// other way to find out. It also supplies the human-readable kind — the
// specification digest's key spells "spec-sha256" and says nothing about a
// digest on its own.
func RequireManaged(subject string, labels map[string]string) error {
	for _, label := range []struct{ kind, key string }{
		{"role", CurrentRoleLabelKey},
		{"specification digest", CurrentSpecLabelKey},
	} {
		if _, presence := ReadOwnershipLabel(
			labels, label.key,
		); presence == LabelBlank {
			return malformedLabelError(
				subject, label.kind, label.key+" is present but empty",
			)
		}
	}
	if err := RequireOwned(subject, labels); err != nil {
		return err
	}
	// Ownership is proven, so the role is now either readable or absent: a blank
	// one was refused above and again by the classifier. Absent is the case that
	// has to stop here rather than in ClassifyOwnership, because refusing it
	// there would refuse every managed volume and network too.
	//
	// The specification digest stays optional on purpose: containerArgs omits it
	// for a container that was never sealed, so demanding it would refuse a
	// container this binary legitimately created.
	if _, presence := ReadRoleLabel(labels); !presence.Readable() {
		return malformedLabelError(
			subject, "role", CurrentRoleLabelKey+" is absent",
		)
	}
	return nil
}

// RequireRole returns nil when the resource carries the wanted runtime role.
// Like RequireOwned it keeps the key inside this file so no caller spells the
// namespace itself.
func RequireRole(subject, want string, labels map[string]string) error {
	role, presence := ReadRoleLabel(labels)
	if presence == LabelBlank {
		return malformedLabelError(
			subject, "role", CurrentRoleLabelKey+" is present but empty",
		)
	}
	if !presence.Readable() || role != want {
		// Ownership is checked separately, so naming it here would send the
		// operator to a boundary that is already known to be sound.
		return fmt.Errorf("%s role label does not match %q", subject, want)
	}
	return nil
}

// RequireSpecDigest returns nil when the resource was sealed with the wanted
// specification digest. A blank digest label is an incomplete resource, not
// image or runtime drift, and the two have opposite remediations.
func RequireSpecDigest(subject, want string, labels map[string]string) error {
	sealed, presence := ReadSpecLabel(labels)
	if presence == LabelBlank {
		return malformedLabelError(
			subject, "specification digest",
			CurrentSpecLabelKey+" is present but empty",
		)
	}
	if !presence.Readable() || sealed != want {
		return fmt.Errorf(
			"%s immutable image or runtime specification drifted",
			subject,
		)
	}
	return nil
}

// describeMalformedOwnership names why the current namespace is incoherent. It
// names keys only, never values: see malformedLabelError.
//
// The branches below are in the SAME order ClassifyOwnership decides in, and
// that is the whole requirement rather than a stylistic one. Classification
// treats a blank role or specification label as malformed whatever the marker
// says, so a container carrying a valid marker beside a blank role is malformed
// because of the role. A diagnostic that checked only the marker would fall
// through and tell the operator the marker was absent — while it is sitting
// right there, valid — and send them looking for the wrong label on a resource
// no destructive path may touch.
func describeMalformedOwnership(labels map[string]string) string {
	if _, presence := ReadOwnershipLabel(
		labels, CurrentManagedLabelKey,
	); presence == LabelBlank {
		return CurrentManagedLabelKey + " is present but empty"
	}
	for _, key := range []string{CurrentRoleLabelKey, CurrentSpecLabelKey} {
		if _, presence := ReadOwnershipLabel(labels, key); presence == LabelBlank {
			return key + " is present but empty"
		}
	}
	return CurrentManagedLabelKey + " is absent while other " +
		CurrentLabelNamespace + ".* ownership labels are present"
}

// malformedLabelError renders the fail-closed reason an incompletely labelled
// resource stops the caller. It names keys but never echoes their values: a
// label value is attacker- or accident-supplied text that reaches operator
// output and the durable lifecycle failure record, and that record only redacts
// credentials it already knows. The operator resolves it with
// `container inspect`, which the runbook prescribes.
func malformedLabelError(subject, kind, reason string) error {
	return fmt.Errorf(
		"%s carries incomplete %s labels: %s; %w",
		subject, kind, reason, ErrMalformedOwnershipLabels,
	)
}

// ownershipLabelArgs renders the `--label` arguments for one ownership label.
//
// Phase 3A stopped writing the legacy namespace, and Phase 3B stopped reading
// it. As long as the writer kept emitting the legacy keys, every newly created
// resource re-manufactured exactly the state the sweep existed to remove, and
// the legacy keys could never reach zero; the reader could only be narrowed once
// they had.
func ownershipLabelArgs(currentKey, value string) []string {
	return []string{"--label", currentKey + "=" + value}
}

// managedOwnershipLabelArgs renders the ownership marker written on every
// container, network, and volume this binary creates.
func managedOwnershipLabelArgs() []string {
	return ownershipLabelArgs(CurrentManagedLabelKey, ManagedLabelValue)
}
