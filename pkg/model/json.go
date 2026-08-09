package model

import (
	"encoding/json"
	"fmt"
)

// ParseFlowVersion decodes a JSON-defined flow into a *FlowVersion ready to
// pass to Engine.ExecuteBegin/ExecuteResume — what makes a flow something a
// caller (a human, or an AI agent) can define as data instead of Go struct
// literals. Every flow-definition type in this package (FlowVersion down to
// Condition) carries a json tag for exactly this purpose.
//
// Known limitation: an Input map's values decode as whatever
// encoding/json produces for arbitrary JSON — string, float64, bool, nil,
// []any, map[string]any. There is no JSON representation for
// *piece.OAuth2Auth (see piece.AuthInputKey's doc comment) — a JSON-defined
// flow can only put a plain string under AuthInputKey. That covers every
// catalog piece needing a secret EXCEPT http's OAuth2 path specifically
// (its plain-string Authorization-header path, and pkg/pieces/crypto's and
// pkg/pieces/hash's keys, all accept a string directly — the latter two
// were extended to accept a string alongside []byte for exactly this
// reason). Building a flow that needs a real OAuth2 connection
// (AccessToken/Data/Props) still requires Go code.
func ParseFlowVersion(data []byte) (*FlowVersion, error) {
	var fv FlowVersion
	if err := json.Unmarshal(data, &fv); err != nil {
		return nil, fmt.Errorf("parsing flow JSON: %w", err)
	}
	return &fv, nil
}
