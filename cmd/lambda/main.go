// Command goflow-lambda runs a single, fixed flow directly through
// pkg/engine on AWS Lambda — no HTTP server, no FileStore-backed
// catalog/flowstore/credentials/runstore, no pkg/oauth, no pkg/scheduler.
// Those four are all broken or unviable if the whole goflow-server binary
// is deployed to Lambda as-is (file-store persistence doesn't survive
// Lambda's many parallel, ephemeral execution environments; pkg/oauth's
// state is in-memory only; pkg/scheduler needs a continuously running
// process, which Lambda's freeze-between-invocations model doesn't give
// it) — this entrypoint sidesteps all four instead of trying to fix them:
// the flow to run is embedded in the binary at BUILD time (flow.json, via
// go:embed) rather than loaded from a Store at runtime, so there is no
// persistence for a cold/parallel instance to ever disagree about, and no
// runtime flow-authoring surface (no POST /flows, no goflow_save_flow
// equivalent) for OAuth or a scheduler to need to gate or drive.
//
// Deploying a DIFFERENT flow means replacing flow.json and rebuilding —
// deliberately, for the same reason: nothing here can drift across
// instances if nothing here can change at runtime at all.
//
// The registry is built from ONLY pkg/pieces.All(), the built-in Go
// pieces — the same two-line construction pkg/catalog.BuildRegistry uses
// for its own built-in half (see that function's doc comment), just
// without the Store-backed extra layer it adds on top, since there is no
// catalog Store here to layer anything onto. A PIECE action needing a
// real secret must have it resolved into its Input before ExecuteBegin
// runs: pkg/engine has no concept of a $credential marker at all — that
// substitution is pkg/flowstore's job, sitting above the engine on every
// other transport (HTTP, MCP) — so flow.json must either bake in a real
// value directly, or a future version of this file would need to resolve
// one (e.g. from an env var or AWS Secrets Manager) into the flow's Input
// before calling ExecuteBegin.
//
// This is the one new dependency in the project beyond
// github.com/dop251/goja: github.com/aws/aws-lambda-go, to receive
// invocations the way AWS actually documents and supports (a hand-rolled
// net/http client against the Lambda Runtime API was the plain-stdlib
// alternative, but the official SDK is what's maintained).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-lambda-go/lambda"

	"goflow/pkg/engine"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

//go:embed flow.json
var flowJSON []byte

var (
	eng            *engine.Engine
	flow           model.FlowVersion
	executeTrigger bool
)

// setup parses the embedded flow.json and builds the built-in-only
// registry, populating the flow/eng package vars — split out from main so
// main_test.go can drive the exact same cold-start path handleRequest
// actually runs against, instead of a test double.
func setup() error {
	if err := json.Unmarshal(flowJSON, &flow); err != nil {
		return fmt.Errorf("flow.json is not a valid FlowVersion: %w", err)
	}
	if flow.Trigger == nil {
		return fmt.Errorf("flow.json has no trigger")
	}

	reg := piece.NewRegistry()
	for _, p := range pieces.All() {
		reg.Register(p)
	}
	eng = engine.New(reg)
	return nil
}

func main() {
	if err := setup(); err != nil {
		log.Fatalf("%v — refusing to start", err)
	}

	// GOFLOW_LAMBDA_EXECUTE_TRIGGER mirrors every other transport's
	// executeTrigger flag: unset/false passes the Lambda event straight
	// through as the trigger step's output (the only sensible choice for
	// an EMPTY trigger, and the same default POST /flows/run itself
	// uses); true would invoke a PIECE_TRIGGER's Run hook instead, for a
	// flow.json that uses one.
	executeTrigger = os.Getenv("GOFLOW_LAMBDA_EXECUTE_TRIGGER") == "true"

	lambda.Start(handleRequest)
}

// handleRequest runs the embedded flow once per Lambda invocation, with
// the invocation event decoded as the trigger payload — the same
// "arbitrary JSON in, full ExecutionState out" contract POST /flows/run
// and goflow_run_flow already share, just reached through a Lambda
// invocation instead of an HTTP request or an MCP tool call. ExecuteBegin
// never returns an error (see its own doc comment) — a broken step
// becomes a FAILED verdict on the returned state, not an invocation
// error, matching how every other transport here already treats a run
// failure as a normal result, not a fault.
func handleRequest(ctx context.Context, event json.RawMessage) (*model.ExecutionState, error) {
	var trigger any
	if len(event) > 0 {
		if err := json.Unmarshal(event, &trigger); err != nil {
			return nil, err
		}
	}
	return eng.ExecuteBegin(&flow, engine.BeginInput{TriggerPayload: trigger, ExecuteTrigger: executeTrigger}), nil
}
