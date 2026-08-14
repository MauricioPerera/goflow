# Using goflow with DeepSeek Harness (dsh)

goflow speaks MCP natively (`pkg/mcpapi`, hand-written JSON-RPC 2.0, no SDK,
no SSE, no sessions — mounted at `POST /mcp` under the same bearer auth as
every other route). That means connecting it to [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)
(`dsh`) needs **no custom dsh plugin** — the generic MCP client bridge dsh
already ships (`@deepseek-ai/dsh-mcp-client`) is enough. This file documents
the exact config and the verification that it works end-to-end, agent → MCP
→ goflow → back.

## 1. Run goflow

```sh
go build ./cmd/server/
GOFLOW_CREDENTIALS_KEY=$(openssl rand -hex 32) \
GOFLOW_API_TOKEN=goflow-dev-token \
./server
```

`GOFLOW_ADDR` defaults to `127.0.0.1:8080`. Both `GOFLOW_API_TOKEN` and a
32-byte-hex `GOFLOW_CREDENTIALS_KEY` are mandatory — the server refuses to
boot without them (see `cmd/server/main.go`).

## 2. Add the MCP route to your dsh profile

Edit `$DSH_HOME/profiles/<profile>/cordis.patch.yml` (e.g.
`~/.dsh/profiles/web/cordis.patch.yml` or `.../headless/cordis.patch.yml`)
and insert a new plugin row:

```yaml
- insert:
    - id: mcp-goflow
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        serverName: goflow
        transport: streamable-http
        url: http://127.0.0.1:8080/mcp
        headers:
          Authorization: !!js '`Bearer ${process.env.GOFLOW_API_TOKEN}`'
```

Run `dsh` with `GOFLOW_API_TOKEN` in its own process environment (same
value as step 1) — the `!!js` expression reads it at plugin-activation time.

That's it. No goflow-specific dsh package, no code. `dsh-mcp-client` calls
`initialize` → `notifications/initialized` → `tools/list` on boot and
registers every goflow tool on `ctx.tools` under the `mcp__goflow__*`
namespace — `mcp__goflow__goflow_list_flows`, `mcp__goflow__goflow_save_flow`,
`mcp__goflow__goflow_run_flow`, `mcp__goflow__goflow_save_credential`, and
the rest of goflow's ~25 MCP tools (`pkg/mcpapi`).

## 3. Verify

Independent of dsh, tail goflow's own stdout — every `/mcp` request is
logged. A correct connection looks like:

```
POST /mcp 200   ← initialize
POST /mcp 202   ← notifications/initialized
GET  /mcp 405   ← dsh-mcp-client probes for an SSE stream; goflow doesn't
                   support GET on /mcp — this 405 is expected, not an error
POST /mcp 200   ← tools/list
```

A `401` here means `GOFLOW_API_TOKEN` wasn't set in the *dsh* process that
booted the plugin (`dsh-mcp-client` retries with exponential backoff per its
`reconnect` config, so a burst of 401s self-heals once the env var is
correct and the process restarts).

### End-to-end proof (from this integration's own testing)

Asked a real dsh agent, running `deepseek-v4-pro:cloud` via a local Ollama
route, to save a flow through goflow's MCP tools:

> Usá goflow para guardar un flow llamado 'ping-test' con un trigger EMPTY
> y una sola action CODE que devuelva el string 'pong'.

The agent called `mcp__goflow__goflow_save_flow`, and the flow was
confirmed independently against goflow's own REST API — not dsh's word for
it:

```sh
$ curl -s http://127.0.0.1:8080/flows -H "Authorization: Bearer goflow-dev-token"
{"flows":[{"name":"ping-test","displayName":"","description":"","webhookEnabled":false}]}
```

Repeated later against a second dsh profile (`web`, the browser UI) with a
fresh prompt — the agent called `mcp__goflow__goflow_list_flows` and
correctly reported the same `ping-test` flow back through the chat UI.

## Notes / gotchas

- **`api: openai-completions` is unrelated to goflow** — that's a separate
  piece of the same dsh setup (an Ollama LLM route for the *model*, not the
  tool bridge). Mentioned here only so a reader diffing a full
  `cordis.patch.yml` doesn't confuse the two unrelated entries.
- **`transport: streamable-http`, not `stdio`** — goflow's MCP server is an
  HTTP handler on the same process as the REST API, not a spawned binary.
- **No session negotiation** — goflow's MCP transport is intentionally
  minimal (see its own docs: "no SDK, no streaming SSE, no sessions").
  `dsh-mcp-client` (built on the official MCP SDK) tolerates this fine: it
  never receives an `Mcp-Session-Id` and simply omits one on subsequent
  requests, which is spec-legal.
- **Auth is one shared bearer token** for the whole goflow server — the
  same `GOFLOW_API_TOKEN` gates `/mcp`, `/flows`, `/credentials`, `/runs`,
  everything except the public `/webhooks/{name}` and OAuth routes.
