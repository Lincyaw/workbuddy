package agentm

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xeipuuv/gojsonschema"
)

// schemaJSON embeds the canonical structured-output contract that ships with
// the repo at schemas/agentm-output.schema.json. Keeping it embedded means
// validation works regardless of the worker process's cwd at dispatch time.
//
//go:embed agentm-output.schema.json
var schemaJSON []byte

var compiledSchema *gojsonschema.Schema

func init() {
	loader := gojsonschema.NewBytesLoader(schemaJSON)
	schema, err := gojsonschema.NewSchema(loader)
	if err != nil {
		// Embedded schema is part of the binary; a parse failure here is a
		// build-time bug, not a runtime condition. Panic is acceptable.
		panic(fmt.Sprintf("agentm: compile embedded output schema: %v", err))
	}
	compiledSchema = schema
}

// ParseAndValidate parses raw JSON (the body of a RESULT: line or a result
// file) and returns the typed Output. It returns an error if the JSON is
// malformed OR if it fails schema validation; the error message is single-
// line and safe to drop into an issue comment as a failure_reason.
func ParseAndValidate(raw []byte) (*Output, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("empty structured output")
	}
	if err := validateJSON([]byte(trimmed)); err != nil {
		return nil, err
	}
	var out Output
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

// ParseResult unmarshals a RESULT body into an Output WITHOUT running the
// conditional schema checks (e.g. session_log_path-required-on-success) that
// the backend can only satisfy after filling backend-owned fields. Unknown
// fields are rejected to preserve the schema's additionalProperties:false
// intent. Callers MUST run ValidateOutput after filling backend-owned fields
// (session_log_path) — see (*session).resolveOutput. session_log_path is a
// host-side artifact path the sandboxed agent cannot know, so requiring it on
// the agent's raw RESULT line would be unsatisfiable in agent_env mode.
func ParseResult(raw []byte) (*Output, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("empty structured output")
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var out Output
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

// ValidateOutput runs the embedded schema against a (possibly backend-
// completed) Output. Used after ParseResult + backend field-fill so the
// final object is held to the full contract.
func ValidateOutput(out *Output) error {
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return validateJSON(raw)
}

// validateJSON runs the embedded schema against a JSON document and collapses
// any violations into a single-line error safe for an issue comment.
func validateJSON(raw []byte) error {
	res, err := compiledSchema.Validate(gojsonschema.NewBytesLoader(raw))
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	if !res.Valid() {
		issues := res.Errors()
		msgs := make([]string, 0, len(issues))
		for _, e := range issues {
			msgs = append(msgs, e.String())
		}
		return fmt.Errorf("schema violations: %s", strings.Join(msgs, "; "))
	}
	return nil
}
