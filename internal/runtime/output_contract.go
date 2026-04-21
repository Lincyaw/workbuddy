package runtime

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/xeipuuv/gojsonschema"
)

func ValidateOutputContract(agent *config.AgentConfig, result *Result) error {
	if agent == nil || result == nil || result.ExitCode != 0 {
		return nil
	}
	schemaPath := agent.OutputContractSchemaPath()
	if schemaPath == "" {
		return nil
	}

	output := structuredOutputCandidate(result)
	if output == "" {
		return fmt.Errorf("runtime: output_contract: missing structured output")
	}
	if !json.Valid([]byte(output)) {
		return fmt.Errorf("runtime: output_contract: final output is not valid JSON")
	}

	schemaLoader := gojsonschema.NewReferenceLoader("file://" + filepath.ToSlash(schemaPath))
	documentLoader := gojsonschema.NewStringLoader(output)
	validation, err := gojsonschema.Validate(schemaLoader, documentLoader)
	if err != nil {
		return fmt.Errorf("runtime: output_contract: validate %s: %w", agent.OutputContract.SchemaFile, err)
	}
	if validation.Valid() {
		return nil
	}

	var problems []string
	for _, desc := range validation.Errors() {
		problems = append(problems, desc.String())
	}
	return fmt.Errorf("runtime: output_contract: %s", strings.Join(problems, "; "))
}

func structuredOutputCandidate(result *Result) string {
	if result == nil {
		return ""
	}
	if text := strings.TrimSpace(result.LastMessage); text != "" {
		return text
	}
	return strings.TrimSpace(stripMetaBlock(result.Stdout))
}

func stripMetaBlock(stdout string) string {
	beginIdx := strings.Index(stdout, metaBeginMarker)
	if beginIdx < 0 {
		return stdout
	}
	after := stdout[beginIdx+len(metaBeginMarker):]
	endIdx := strings.Index(after, metaEndMarker)
	if endIdx < 0 {
		return stdout
	}
	return stdout[:beginIdx] + after[endIdx+len(metaEndMarker):]
}

func EmitOutputContractFailure(events chan<- launcherevents.Event, seq *uint64, sessionID, turnID string, err error, emit func(chan<- launcherevents.Event, *uint64, string, string, launcherevents.EventKind, any, []byte)) {
	if events == nil || err == nil {
		return
	}
	emit(events, seq, sessionID, turnID, launcherevents.KindError, launcherevents.ErrorPayload{
		Code:        "output_contract",
		Message:     err.Error(),
		Recoverable: false,
	}, nil)
	emit(events, seq, sessionID, turnID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{
		TurnID: turnID,
		Status: "error",
	}, nil)
}
