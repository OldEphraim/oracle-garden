package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidationError is one structured complaint about a payload. Callers
// (handlers, the engine, the agent runtime's parse-fail-retry path) can
// surface these to UI or to the model in a follow-up prompt.
//
// Path is a JSON-pointer-style string indicating where in the payload the
// failure occurred (e.g. "/headlines/0/source"). It's empty for top-level
// keyword failures (e.g. "additionalProperties").
type ValidationError struct {
	Path    string
	Keyword string // the JSON Schema keyword that failed (e.g. "required", "type", "minimum")
	Message string
}

func (e ValidationError) String() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Keyword, e.Message)
	}
	return fmt.Sprintf("%s [%s]: %s", e.Path, e.Keyword, e.Message)
}

// ValidateAgainst parses payload as JSON and validates it against the schema
// registered as typeID (e.g. "thesis.v1"). Returns:
//
//   - (nil, nil) on success.
//   - (nil, *ErrUnknownType) if the typeID isn't registered.
//   - (errors, nil) on validation failure with the structured error list.
//   - (nil, err) on JSON parse failure (so the caller can branch on parse
//     vs. validate).
//
// The caller controls retry policy (the agent runtime retries once with the
// validation message appended; handlers usually surface as 400).
func (r *Registry) ValidateAgainst(payload []byte, typeID string) ([]ValidationError, error) {
	def, err := r.Get(typeID)
	if err != nil {
		return nil, err
	}

	var data any
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, fmt.Errorf("types: %s: payload is not valid JSON: %w", typeID, err)
	}

	if err := def.Compiled.Validate(data); err != nil {
		return flattenValidationError(err), nil
	}
	return nil, nil
}

// flattenValidationError converts jsonschema/v6's nested error tree into a
// flat slice of ValidationError. The library reports parent-and-child errors
// for the same failure; we keep only the leaves (non-empty Causes are
// recursed into).
func flattenValidationError(err error) []ValidationError {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		// Non-validation error (shouldn't happen for jsonschema/v6 .Validate,
		// but fall back gracefully).
		return []ValidationError{{Message: err.Error()}}
	}
	var out []ValidationError
	walkValidationError(ve, &out)
	if len(out) == 0 {
		// No leaves — surface the top-level message rather than returning
		// an empty slice that callers might mistake for "no errors".
		out = append(out, ValidationError{
			Path:    instanceLocation(ve),
			Keyword: keywordOf(ve),
			Message: messageOf(ve),
		})
	}
	return out
}

func walkValidationError(ve *jsonschema.ValidationError, out *[]ValidationError) {
	if len(ve.Causes) == 0 {
		*out = append(*out, ValidationError{
			Path:    instanceLocation(ve),
			Keyword: keywordOf(ve),
			Message: messageOf(ve),
		})
		return
	}
	for _, cause := range ve.Causes {
		walkValidationError(cause, out)
	}
}

func instanceLocation(ve *jsonschema.ValidationError) string {
	if len(ve.InstanceLocation) == 0 {
		return ""
	}
	return "/" + strings.Join(ve.InstanceLocation, "/")
}

func keywordOf(ve *jsonschema.ValidationError) string {
	// ErrorKind carries the offending keyword (e.g. "required", "type"). v6
	// reports this via the Kind field's KeywordPath() method when available;
	// we fall back to a stringification.
	if ve.ErrorKind == nil {
		return ""
	}
	if k, ok := ve.ErrorKind.(interface{ KeywordPath() []string }); ok {
		path := k.KeywordPath()
		if len(path) > 0 {
			return path[len(path)-1]
		}
	}
	return ""
}

func messageOf(ve *jsonschema.ValidationError) string {
	// jsonschema/v6's localized rendering needs a *message.Printer from
	// golang.org/x/text. The default Error() is already the English message
	// for a single node, which is what we want — and avoids dragging x/text
	// into our public type-error format.
	return ve.Error()
}
