package lifecycle

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// This file is the read-only container inventory `agentopsctl migrate-labels`
// prints. It began as Phase 2's pre-mutation snapshot; the sweep it fed was
// retired in Phase 3A, and Phase 3B removed the legacy namespace it classified
// against. What remains is a diagnostic: it reports what this binary owns, what
// it does not, and what it refuses to reason about — all from the current
// namespace alone.
//
// Named volumes are still recorded per container. Apple Container attaches them
// exclusively, so which container holds which volume is the fact an operator
// needs before touching anything, and it is the one this snapshot exists to
// preserve.

// MigrationDisposition is the inventory's verdict for one container.
//
// The Phase 2 sweep's own verdicts — `pending` and `migrated` — are deliberately
// NOT declared here. Nothing produces them, and a constant is not what keeps the
// committed evidence readable: MigrationAudit.Totals is keyed by this defined
// string type, which decodes any key whether or not a constant exists for it.
// `evidence/label-p2/*.json` therefore keeps decoding on its own, which is what
// TestMergedPhase2EvidenceStillDecodes proves.
type MigrationDisposition string

const (
	// MigrationSkipped is a container with nothing to do: owned, or not owned by
	// this binary at all.
	MigrationSkipped MigrationDisposition = "skipped"
	// MigrationMalformed is a container whose current ownership labels are
	// incomplete. It is a hard stop and never a deletion candidate.
	MigrationMalformed MigrationDisposition = "malformed"
	// MigrationBlocked is a container this inventory refuses to give a verdict
	// for. It is the fail-closed default, not a routine outcome.
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
	ID          string         `json:"id"`
	State       string         `json:"state"`
	Image       string         `json:"image"`
	ImageDigest string         `json:"imageDigest"`
	Ownership   OwnershipClass `json:"ownership"`
	// RolePresence and SpecPresence replace the role and digest VALUES this
	// record used to carry. Those were observed text, and no observed text is
	// published; presence is what the fail-closed rules actually turn on.
	RolePresence    LabelPresence        `json:"rolePresence"`
	SpecPresence    LabelPresence        `json:"specPresence"`
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

// HasMalformed reports whether any container's current ownership labels are
// incomplete. It is a hard stop: the resource has to be resolved by an operator
// who can see the labels with `container inspect`.
func (audit MigrationAudit) HasMalformed() bool {
	return audit.Totals[MigrationMalformed] > 0
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
	_, rolePresence := ReadRoleLabel(labels)
	_, specPresence := ReadSpecLabel(labels)
	record := ContainerInventoryRecord{
		ID:              actual.ID,
		State:           actual.Status.State,
		Image:           actual.Configuration.Image.Reference,
		ImageDigest:     actual.Configuration.Image.Descriptor.Digest,
		Ownership:       ClassifyOwnership(labels),
		RolePresence:    rolePresence,
		SpecPresence:    specPresence,
		OwnershipLabels: ownershipLabelSubset(labels),
		NamedVolumes:    NamedVolumeAttachments(actual),
		TmpfsMounts:     tmpfsDestinations(actual),
		Networks:        containerNetworks(actual),
	}
	record.Disposition, record.Reason = disposition(labels, record.Ownership)
	return record
}

// disposition maps an ownership class onto the inventory's verdict. Every branch
// returns a reason, because an audit row without one cannot be acted on by the
// operator who reads it weeks later.
//
// Since Phase 3B a container carrying only the retired legacy namespace is
// indistinguishable from one carrying no ownership label at all, and both are
// reported as skipped-because-not-ours. That is the intended end state: the
// binary does not adopt what it cannot prove it owns.
func disposition(
	labels map[string]string,
	class OwnershipClass,
) (MigrationDisposition, string) {
	// An owned container whose role or specification label is blank is as
	// incompletely labelled as one whose marker is, and reporting it as
	// "skipped" would hide a resource no destructive path may touch.
	if class.Owned() {
		for _, label := range []struct{ kind, key string }{
			{"role", CurrentRoleLabelKey},
			{"specification digest", CurrentSpecLabelKey},
		} {
			if _, presence := ReadOwnershipLabel(
				labels, label.key,
			); presence == LabelBlank {
				return MigrationMalformed, label.kind +
					" label is present but empty; resolve the incomplete " +
					"ownership labels before acting on this container"
			}
		}
	}
	switch class {
	case OwnershipMalformed:
		return MigrationMalformed, "ownership labels are incomplete; " +
			"resolve them before acting on this container"
	case OwnershipMissingLabel:
		return MigrationSkipped, "no ownership label; not managed by agentopsctl"
	case OwnershipUnmanaged:
		return MigrationSkipped, "ownership label names another deployment"
	case OwnershipOwned:
		// Deliberately not "completely labelled": an absent role or specification
		// digest is accepted above (a volume has no role, and a container may be
		// created without a sealed digest), so this row is only evidence that no
		// ownership label is half-written — not that every one is present.
		return MigrationSkipped, "owned; no incomplete ownership label in " +
			CurrentLabelNamespace
	default:
		// Unreachable while OwnershipClass stays closed, but a new class must
		// fail closed rather than inherit "skip".
		return MigrationBlocked, "unrecognized ownership class"
	}
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
// Apple Container reports the *effective* environment: the values the original
// specification supplied plus whatever the image itself declares. Handing an
// image default back would over-specify the replacement, and because these
// values are injected into the `container` CLI's own process environment so it
// can pass `--env KEY` without putting secrets in argv, an image's PATH or HOME
// would displace the host's for that invocation. Subtracting the image's own
// declarations recovers the operator-supplied set, which is what the original
// specification actually carried. imageEnvironment may be nil, in which case
// the full observed set is used.
func environmentMap(
	actual ContainerActual,
	imageEnvironment []string,
) (map[string]string, error) {
	declared := make(map[string]string, len(imageEnvironment))
	for _, entry := range imageEnvironment {
		if key, value, present := strings.Cut(entry, "="); present {
			declared[key] = value
		}
	}
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
		// The two-value lookup matters: a missing key also yields "", so a
		// one-value comparison would treat an operator-supplied `KEY=` as an
		// image default and drop it from the replacement entirely.
		if declaredValue, isDeclared := declared[key]; isDeclared &&
			value == declaredValue {
			continue
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

// An ownership label is not trusted input. Anyone who can create a container can
// put `com.mrbaron3.servo.role` on it with any text they like, and that text
// reaches operator terminals and durable evidence — which redacts only the
// credentials it already knows about. Publishing it verbatim would be an
// attacker-chosen channel into an audit trail read weeks later by someone who
// was not there.
//
// So nothing observed is ever published. Every ownership label renders as one of
// the fixed tokens below, chosen to keep the distinctions the diagnostics are
// actually used for — is this ours, is it somebody else's, is it half-written,
// is the digest even digest-shaped — while carrying no bits the resource chose.
//
// A shape bound was tried first and rejected: a 32-character role or a
// 64-character digest still passes observed content through, and "bounded" was
// not the requirement.
const (
	// ManagedLabelToken means the marker names this binary.
	ManagedLabelToken = "managed"
	// PresentLabelToken means the label is written with some readable value.
	// Which value it is deliberately does not reach the reader.
	PresentLabelToken = "present"
	// DigestShapedLabelToken means the specification label has the exact shape
	// of a SHA-256 digest, which is the only thing this binary writes there.
	DigestShapedLabelToken = "digest-shaped"
	// UnrecognizedLabelToken means the label is written with something this
	// binary does not produce.
	UnrecognizedLabelToken = "unrecognized-value"
	// BlankLabelToken means the label is written with an empty value, which is
	// how a half-written label appears.
	BlankLabelToken = "blank"
)

// specDigestShape is the exact shape of a SHA-256 digest.
var specDigestShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ownershipLabelToken renders one ownership label for publication. Every branch
// returns a constant declared in this file, so no argument can reach the result.
func ownershipLabelToken(key, value string, presence LabelPresence) string {
	switch presence {
	case LabelAbsent:
		return ""
	case LabelBlank:
		return BlankLabelToken
	}
	switch key {
	case CurrentManagedLabelKey:
		if value == ManagedLabelValue {
			return ManagedLabelToken
		}
	case CurrentRoleLabelKey:
		return PresentLabelToken
	case CurrentSpecLabelKey:
		if specDigestShape.MatchString(value) {
			return DigestShapedLabelToken
		}
	}
	return UnrecognizedLabelToken
}

// ownershipLabelSubset renders the ownership labels a record publishes. The keys
// are this binary's own and are safe to name; every value is a token, so no
// resource can choose what an audit trail says.
func ownershipLabelSubset(labels map[string]string) map[string]string {
	subset := make(map[string]string, len(ownershipLabelKeys))
	for _, key := range ownershipLabelKeys {
		value, presence := ReadOwnershipLabel(labels, key)
		if presence == LabelAbsent {
			continue
		}
		subset[key] = ownershipLabelToken(key, value, presence)
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
