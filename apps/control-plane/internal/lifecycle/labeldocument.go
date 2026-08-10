package lifecycle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Apple Container 1.1.0 exposes no route that changes the labels of an existing
// volume, network, or container: the complete XPC surface is
// volumeCreate/Delete/Inspect/List and networkCreate/Delete/List, and the
// published ClientVolume and NetworkClient carry no update method. Phase 3A
// therefore edits the runtime's own metadata documents while its services are
// stopped, and every guard in this file exists because that is a privileged
// thing to do.
//
// The central guard is byte stability. A document is parsed into its original
// key order with every value kept as raw bytes, and the rewriter must be able to
// reproduce the untouched file exactly before it is allowed to write anything.
// A rewriter that cannot reproduce the file it just read does not understand
// that file, and the correct response to a document this code does not fully
// understand is to refuse it — not to re-render it in Go's dialect of JSON.
//
// The practical consequence is that a completed run differs from its backup in
// the labels field and nowhere else, which is what makes "only the ownership
// keys changed" a fact an operator can check rather than a claim they must
// trust.

// jsonObject is one JSON object with its key order preserved and each value
// held as the exact bytes the source file contained.
type jsonObject struct {
	keys   []string
	values map[string]json.RawMessage
}

// parseJSONObject reads exactly one JSON object and nothing else. Duplicate
// keys and trailing content are rejected rather than resolved: both mean the
// file was produced by something whose conventions this code has not verified.
func parseJSONObject(document []byte) (*jsonObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	opening, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("parse metadata document: %w", err)
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New(
			"metadata document is not a JSON object",
		)
	}
	object := &jsonObject{values: make(map[string]json.RawMessage)}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("parse metadata document key: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("metadata document key is not a string")
		}
		if _, duplicate := object.values[key]; duplicate {
			return nil, fmt.Errorf(
				"metadata document repeats the %q field", key,
			)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf(
				"parse metadata document field %q: %w", key, err,
			)
		}
		object.keys = append(object.keys, key)
		object.values[key] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("parse metadata document close: %w", err)
	}
	// Anything after the object means the file holds more than the single
	// document this code accounts for.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New(
			"metadata document carries content after the top level object",
		)
	}
	return object, nil
}

// encode re-emits the object in its original key order using each value's
// original bytes. Apple Container writes compact JSON, so no whitespace is
// introduced; a source file that was not compact fails the round-trip guard and
// is refused.
func (object *jsonObject) encode() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for index, key := range object.keys {
		if index > 0 {
			buffer.WriteByte(',')
		}
		encodedKey, err := encodeJSONString(key)
		if err != nil {
			return nil, err
		}
		buffer.Write(encodedKey)
		buffer.WriteByte(':')
		buffer.Write(object.values[key])
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// labelTextIsPortable reports whether text encodes to identical bytes under
// Foundation, which writes Apple Container's files, and Go, which writes this
// tool's replacement. Foundation escapes a forward slash and Go does not, and Go
// escapes HTML delimiters where Foundation does not, so a label containing any
// of them would make a rewritten file differ from its source in a way that has
// nothing to do with the migration. Every ownership label this project writes is
// plain, and one that is not stops the run for an operator to look at.
func labelTextIsPortable(text string) bool {
	for _, character := range text {
		if character < 0x20 || character > 0x7e {
			return false
		}
		switch character {
		case '"', '\\', '/', '<', '>', '&':
			return false
		}
	}
	return true
}

// encodeJSONString renders one portable JSON string.
//
// The refusal deliberately does not echo the text. This is reached with label
// keys AND values — a foreign label's value is arbitrary third-party text, and
// rollback calls this path through inspectMetadataFile — so reproducing it here
// would put attacker-chosen bytes on an operator's terminal from the one command
// still allowed to touch runtime metadata. What an operator needs in order to
// act is which character is unportable and where, and both survive redaction.
func encodeJSONString(text string) ([]byte, error) {
	if !labelTextIsPortable(text) {
		return nil, fmt.Errorf(
			"metadata text contains %s, which this migration will not "+
				"re-encode; inspect the document by hand to find it",
			describeUnportableText(text),
		)
	}
	return []byte(`"` + text + `"`), nil
}

// describeUnportableText names the first character a portable encoding cannot
// carry, by class and position, without reproducing the text around it.
func describeUnportableText(text string) string {
	for index, character := range text {
		if labelTextIsPortable(string(character)) {
			continue
		}
		switch {
		case character < 0x20 || character == 0x7f:
			return fmt.Sprintf("a control character at byte %d", index)
		case character > 0x7f:
			return fmt.Sprintf("a non-ASCII character at byte %d", index)
		default:
			return fmt.Sprintf("the character %q at byte %d", character, index)
		}
	}
	return "an unportable character"
}

// parseLabelMap decodes one labels object, requiring every value to be a
// string. Apple Container's own model types the field as [String: String], and
// a document that disagrees is not one this code recognises.
func parseLabelMap(raw json.RawMessage) (map[string]string, error) {
	object, err := parseJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("parse labels: %w", err)
	}
	labels := make(map[string]string, len(object.keys))
	for _, key := range object.keys {
		var value string
		if err := json.Unmarshal(object.values[key], &value); err != nil {
			return nil, fmt.Errorf("label %q is not a string", key)
		}
		labels[key] = value
	}
	return labels, nil
}

// encodeLabels renders a labels object with sorted keys so the same label set
// always produces the same bytes, which is what lets a re-run of an interrupted
// migration recognise work it already did.
func encodeLabels(labels map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			buffer.WriteByte(',')
		}
		encodedKey, err := encodeJSONString(key)
		if err != nil {
			return nil, err
		}
		encodedValue, err := encodeJSONString(labels[key])
		if err != nil {
			return nil, err
		}
		buffer.Write(encodedKey)
		buffer.WriteByte(':')
		buffer.Write(encodedValue)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// descend walks the parent objects on a label path, returning the object that
// directly holds the labels field.
func descend(
	document []byte,
	parents []string,
) (*jsonObject, error) {
	object, err := parseJSONObject(document)
	if err != nil {
		return nil, err
	}
	for _, key := range parents {
		raw, present := object.values[key]
		if !present {
			return nil, fmt.Errorf(
				"metadata document has no %q field", key,
			)
		}
		if object, err = parseJSONObject(raw); err != nil {
			return nil, fmt.Errorf("parse %q: %w", key, err)
		}
	}
	return object, nil
}

// readLabelsAtPath returns the labels stored at one path without changing
// anything.
func readLabelsAtPath(
	document []byte,
	path []string,
) (map[string]string, error) {
	if len(path) == 0 {
		return nil, errors.New("label path is empty")
	}
	holder, err := descend(document, path[:len(path)-1])
	if err != nil {
		return nil, err
	}
	raw, present := holder.values[path[len(path)-1]]
	if !present {
		return nil, fmt.Errorf(
			"metadata document has no %q field", path[len(path)-1],
		)
	}
	return parseLabelMap(raw)
}

// rewriteLabels replaces the labels object at one path and re-emits the
// document. Every other field keeps the exact bytes it had, in the position it
// had, so the result differs from the source in the labels field alone.
func rewriteLabels(
	document []byte,
	path []string,
	labels map[string]string,
) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("label path is empty")
	}
	object, err := parseJSONObject(document)
	if err != nil {
		return nil, err
	}
	key := path[0]
	raw, present := object.values[key]
	if !present {
		return nil, fmt.Errorf("metadata document has no %q field", key)
	}
	var replacement []byte
	if len(path) == 1 {
		// Parsing the existing value first means a labels field that is not a
		// string map stops the run before anything is written.
		existing, err := parseLabelMap(raw)
		if err != nil {
			return nil, err
		}
		if replacement, err = encodeLabels(labels); err != nil {
			return nil, err
		}
		// Rewriting a label set to itself must be a no-op at the byte level,
		// otherwise the migration would claim a change it did not make.
		if same, err := encodeLabels(existing); err == nil &&
			bytes.Equal(same, replacement) {
			replacement = raw
		}
	} else if replacement, err = rewriteLabels(raw, path[1:], labels); err != nil {
		return nil, err
	}
	object.values[key] = replacement
	return object.encode()
}

// requireByteStableRoundTrip proves this code can reproduce a metadata document
// exactly before it is allowed to write one. It is the gate that turns "unknown
// shape" from a schema-guessing exercise into a mechanical check.
func requireByteStableRoundTrip(document []byte, path []string) error {
	labels, err := readLabelsAtPath(document, path)
	if err != nil {
		return err
	}
	rewritten, err := rewriteLabels(document, path, labels)
	if err != nil {
		return err
	}
	if !bytes.Equal(rewritten, document) {
		return errors.New(
			"metadata document is not byte stable through this migration's " +
				"encoder; refusing to rewrite a file this tool cannot reproduce",
		)
	}
	return nil
}
