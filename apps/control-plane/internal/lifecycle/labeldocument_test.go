package lifecycle

import (
	"strings"
	"testing"
)

// The Apple Container metadata files are written by Foundation, which emits
// compact JSON with escaped forward slashes and no particular key order. The
// rewriter has to put a label pair into one of those files without becoming the
// author of the rest of it, so these tests are mostly about what must NOT
// change.

const foundationVolumeDocument = `{"driver":"local","format":"ext4",` +
	`"options":{},"creationDate":808039875.620406,"name":"probe",` +
	`"sizeInBytes":549755813888,` +
	`"source":"\/Users\/example\/volumes\/probe\/volume.img",` +
	`"labels":{"com.mrbaron3.workflow.agentopsctl":"v1"}}`

func TestRewriteLabelsReproducesUnmodifiedDocumentByteForByte(t *testing.T) {
	// The round-trip guard is the whole safety argument: a rewriter that cannot
	// reproduce the file it just read does not understand that file well enough
	// to be allowed to write it.
	rewritten, err := rewriteLabels(
		[]byte(foundationVolumeDocument),
		[]string{"labels"},
		map[string]string{"com.mrbaron3.workflow.agentopsctl": "v1"},
	)
	if err != nil {
		t.Fatalf("rewrite unmodified document: %v", err)
	}
	if string(rewritten) != foundationVolumeDocument {
		t.Fatalf(
			"rewriting with identical labels changed the document:\n"+
				" before %s\n after  %s",
			foundationVolumeDocument, rewritten,
		)
	}
}

func TestRewriteLabelsChangesOnlyTheLabelsField(t *testing.T) {
	rewritten, err := rewriteLabels(
		[]byte(foundationVolumeDocument),
		[]string{"labels"},
		map[string]string{
			"com.mrbaron3.workflow.agentopsctl": "v1",
			"com.mrbaron3.servo.agentopsctl":    "v1",
		},
	)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// Every other field keeps its exact original bytes, including the escaped
	// slashes Foundation writes and the float literal, which a naive decode into
	// float64 would re-render in scientific notation.
	for _, fragment := range []string{
		`"driver":"local"`,
		`"format":"ext4"`,
		`"options":{}`,
		`"creationDate":808039875.620406`,
		`"name":"probe"`,
		`"sizeInBytes":549755813888`,
		`"source":"\/Users\/example\/volumes\/probe\/volume.img"`,
	} {
		if !strings.Contains(string(rewritten), fragment) {
			t.Errorf("rewritten document lost %s:\n%s", fragment, rewritten)
		}
	}
	if !strings.Contains(
		string(rewritten), `"com.mrbaron3.servo.agentopsctl":"v1"`,
	) {
		t.Errorf("rewritten document missing new label:\n%s", rewritten)
	}
}

func TestRewriteLabelsPreservesTopLevelKeyOrder(t *testing.T) {
	rewritten, err := rewriteLabels(
		[]byte(foundationVolumeDocument),
		[]string{"labels"},
		map[string]string{"com.mrbaron3.servo.agentopsctl": "v1"},
	)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// Key order is not semantically meaningful to Apple Container, but keeping
	// it makes the backup-versus-result diff a single field, which is what an
	// operator has to audit before approving the run.
	wantOrder := []string{
		"driver", "format", "options", "creationDate", "name", "sizeInBytes",
		"source", "labels",
	}
	position := 0
	for _, key := range wantOrder {
		index := strings.Index(string(rewritten[position:]), `"`+key+`":`)
		if index < 0 {
			t.Fatalf("key %q out of order or missing in:\n%s", key, rewritten)
		}
		position += index
	}
}

func TestRewriteLabelsDescendsIntoNestedPath(t *testing.T) {
	// Container metadata nests the same label map one level down inside
	// runtime-configuration.json, and the sibling fields there are large.
	document := `{"path":"\/tmp\/x","containerConfiguration":` +
		`{"id":"probe","labels":{"a":"1"},"useInit":true},"options":{}}`
	rewritten, err := rewriteLabels(
		[]byte(document),
		[]string{"containerConfiguration", "labels"},
		map[string]string{"a": "1", "b": "2"},
	)
	if err != nil {
		t.Fatalf("rewrite nested: %v", err)
	}
	for _, fragment := range []string{
		`"path":"\/tmp\/x"`,
		`"id":"probe"`,
		`"useInit":true`,
		`"labels":{"a":"1","b":"2"}`,
	} {
		if !strings.Contains(string(rewritten), fragment) {
			t.Errorf("nested rewrite lost %s:\n%s", fragment, rewritten)
		}
	}
}

func TestRewriteLabelsRejectsUnknownShapes(t *testing.T) {
	for name, testCase := range map[string]struct {
		document string
		path     []string
	}{
		"top level is an array": {
			document: `[{"labels":{}}]`,
			path:     []string{"labels"},
		},
		"labels is not an object": {
			document: `{"labels":"v1"}`,
			path:     []string{"labels"},
		},
		"labels value is not a string": {
			document: `{"labels":{"a":1}}`,
			path:     []string{"labels"},
		},
		"path is absent": {
			document: `{"name":"probe"}`,
			path:     []string{"labels"},
		},
		"nested parent is absent": {
			document: `{"labels":{}}`,
			path:     []string{"containerConfiguration", "labels"},
		},
		"nested parent is not an object": {
			document: `{"containerConfiguration":7}`,
			path:     []string{"containerConfiguration", "labels"},
		},
		"trailing bytes after the document": {
			document: `{"labels":{}} trailing`,
			path:     []string{"labels"},
		},
		"duplicate keys": {
			document: `{"labels":{},"labels":{}}`,
			path:     []string{"labels"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := rewriteLabels(
				[]byte(testCase.document), testCase.path,
				map[string]string{"a": "1"},
			); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestReadLabelsAtPathReturnsTheStoredPair(t *testing.T) {
	labels, err := readLabelsAtPath(
		[]byte(foundationVolumeDocument), []string{"labels"},
	)
	if err != nil {
		t.Fatalf("read labels: %v", err)
	}
	if len(labels) != 1 ||
		labels["com.mrbaron3.workflow.agentopsctl"] != "v1" {
		t.Fatalf("unexpected labels: %#v", labels)
	}
}
