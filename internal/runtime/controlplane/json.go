package controlplane

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
)

// Reject duplicate fields (also nested), trailing values and excessive nesting;
// encoding/json's last-key-wins behavior is unsafe for identity and completion.
func validObject(data []byte) error {
	// The native token decoder checks duplicate names, including escaped names,
	// without allocating an interface value and a second name map per object.
	// Preserve encoding/json's replacement of invalid UTF-8 in string values.
	d := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowInvalidUTF8(true))
	first, err := d.ReadToken()
	if err != nil {
		return err
	}
	if first.Kind() != '{' {
		return errors.New("expected JSON object")
	}
	for d.StackDepth() > 0 {
		kind := d.PeekKind()
		if d.StackDepth() > 64 && kind != '}' && kind != ']' {
			return errors.New("JSON nesting exceeds budget")
		}
		if _, err := d.ReadToken(); err != nil {
			return err
		}
	}
	if _, err := d.ReadToken(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
