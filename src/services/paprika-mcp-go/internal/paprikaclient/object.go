package paprikaclient

import (
	"encoding/json"
	"fmt"
)

// Object is one Paprika object with every field kept as raw JSON.
//
// Reading into an Object and writing it back preserves fields the service does
// not model, and preserves values byte for byte. Change only the fields you
// mean to change, using [Object.Set], then pass the Object to a Save method.
// Typed views are decoded from the same bytes and never replace the Object.
type Object map[string]json.RawMessage

// ParseObject decodes a JSON object.
func ParseObject(data []byte) (Object, error) {
	var o Object
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("%w: not a JSON object: %w", ErrUnexpectedResponse, err)
	}
	if o == nil {
		return nil, fmt.Errorf("%w: null object", ErrUnexpectedResponse)
	}
	return o, nil
}

// Has reports whether the field is present, even if null.
func (o Object) Has(key string) bool {
	_, ok := o[key]
	return ok
}

// Get decodes one field into v. A missing field leaves v unchanged.
func (o Object) Get(key string, v any) error {
	raw, ok := o[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("field %q: %w", key, err)
	}
	return nil
}

// String returns a string field, or "" when it is missing, null, or not a string.
func (o Object) String(key string) string {
	var s string
	if err := o.Get(key, &s); err != nil {
		return ""
	}
	return s
}

// UID returns the "uid" field.
func (o Object) UID() string {
	return o.String("uid")
}

// Set encodes v and stores it in the field.
func (o Object) Set(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("field %q: %w", key, err)
	}
	o[key] = raw
	return nil
}

// Clone returns a copy that can be changed without affecting o.
func (o Object) Clone() Object {
	c := make(Object, len(o))
	for k, v := range o {
		c[k] = append(json.RawMessage(nil), v...)
	}
	return c
}

// Entry pairs a typed view with the raw object it came from.
type Entry[T any] struct {
	// Raw holds every field, for round-tripping on writes.
	Raw Object
	// Value is a typed view of the fields the service understands.
	Value T
}

func decodeEntry[T any](raw json.RawMessage) (Entry[T], error) {
	var e Entry[T]
	obj, err := ParseObject(raw)
	if err != nil {
		return e, err
	}
	e.Raw = obj
	if err := json.Unmarshal(raw, &e.Value); err != nil {
		return e, fmt.Errorf("%w: %w", ErrUnexpectedResponse, err)
	}
	return e, nil
}
