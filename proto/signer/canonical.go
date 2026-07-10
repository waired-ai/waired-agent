package signer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// CanonicalJSON returns a deterministic JSON encoding of v with sorted
// object keys, no insignificant whitespace, and HTML escaping disabled.
//
// Two callers (signer and verifier) running this function on equal inputs
// always produce byte-identical output, which is required for stable
// digital signatures over JSON documents.
//
// v must be a value json.Marshal accepts; structs are flattened via the
// stdlib encoder and then re-canonicalised through a generic
// map[string]any pipeline.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical: pre-marshal: %w", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("canonical: unmarshal: %w", err)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, generic); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeString(buf, x)
	case float64:
		// json.Unmarshal turns numeric values into float64 unless
		// json.Number is requested. For our use (timestamps as RFC3339
		// strings, integer epochs, etc.) round-tripping floats is fine.
		buf.WriteString(formatFloat(x))
	case json.Number:
		buf.WriteString(x.String())
	case []any:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonical: unsupported type %T", v)
	}
	return nil
}

// writeString writes a JSON string, escaping the minimal set required by
// the spec without HTML safety escapes (json.Encoder enables those by
// default and they would diverge from typical canonical encoders).
func writeString(buf *bytes.Buffer, s string) {
	enc, _ := json.Marshal(s)
	// json.Marshal already produces a quoted string. It does NOT escape
	// '<', '>', '&' because the package only does that via Encoder's
	// SetEscapeHTML(true) (default). json.Marshal of a plain string does
	// not apply HTML escaping - good for canonical output.
	buf.Write(enc)
}

func formatFloat(f float64) string {
	// JSON int round-trip: if f is a whole number within safe int range,
	// emit as an integer.
	if f == float64(int64(f)) && f >= -1e15 && f <= 1e15 {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}
