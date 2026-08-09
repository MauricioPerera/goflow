package model_test

import (
	"testing"

	"goflow/pkg/model"
)

// FuzzParseFlowVersion feeds arbitrary bytes to ParseFlowVersion — the
// entry point an external caller (a human, or an agent) uses to hand this
// engine a flow it didn't build via Go struct literals, so it's the one
// function in this project that should assume NOTHING about its input's
// shape. The only contract under test: never panic, no matter how
// malformed, deeply nested, or adversarial the input — a bad flow
// definition must always come back as a plain error.
func FuzzParseFlowVersion(f *testing.F) {
	seeds := []string{
		`{"id":"fv-1","trigger":{"name":"t","displayName":"T","type":"EMPTY"}}`,
		`{}`,
		`null`,
		`[]`,
		`""`,
		`0`,
		`true`,
		``,
		`{not valid json`,
		`{"trigger":{"nextAction":{"nextAction":{"nextAction":null}}}}`,
		`{"trigger":{"type":"PIECE_TRIGGER","input":{"a":[1,2,{"b":"{{ 1+1 }}"}]}}}`,
		`{"id":` + `"` + string(make([]byte, 10000)) + `"}`,
		`{"trigger":{"nextAction":{"router":{"children":[null,null]}}}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ParseFlowVersion panicked on input %q: %v", data, r)
			}
		}()
		model.ParseFlowVersion(data) // an error is fine; a panic is not
	})
}
