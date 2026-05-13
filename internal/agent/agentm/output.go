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

	documentLoader := gojsonschema.NewStringLoader(trimmed)
	res, err := compiledSchema.Validate(documentLoader)
	if err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	if !res.Valid() {
		issues := res.Errors()
		msgs := make([]string, 0, len(issues))
		for _, e := range issues {
			msgs = append(msgs, e.String())
		}
		return nil, fmt.Errorf("schema violations: %s", strings.Join(msgs, "; "))
	}

	var out Output
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}
