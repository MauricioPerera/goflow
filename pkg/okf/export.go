package okf

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"goflow/pkg/catalog"
	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

// Bundle is a rendered OKF bundle: bundle-relative path (e.g.
// "index.md", "pieces/stripe.md") -> the concept or index document's
// full rendered content. Every path uses forward slashes regardless of
// OS, matching §6's own bundle-relative link convention.
type Bundle map[string]string

// ExportBundle assembles a complete OKF bundle from goflow's live
// stores — every JS-authored catalog piece (catalogStore) and built-in
// Go piece (pieces.All()), every saved flow (flowStore), and every
// credential NAME (credStore — never a decrypted value, matching how
// credentials.Store.Get is never exposed over any transport elsewhere
// in this project). Called fresh on every request, never cached, same
// "must reflect what's true right now" reasoning httpapi.Server.
// buildRegistry's own doc comment already gives for the identical
// choice. credStore may be nil (matching mcpapi.Handler.CredStore's own
// nil-means-off convention) — the Credentials section is then empty
// rather than an error.
func ExportBundle(catalogStore catalog.Store, flowStore flowstore.Store, credStore credentials.Store, now time.Time) (Bundle, error) {
	bundle := Bundle{}

	pieceEntries, err := exportPieces(bundle, catalogStore, flowStore, now)
	if err != nil {
		return nil, fmt.Errorf("okf: exporting pieces: %w", err)
	}
	flowEntries, err := exportFlows(bundle, flowStore, now)
	if err != nil {
		return nil, fmt.Errorf("okf: exporting flows: %w", err)
	}
	credEntries, err := exportCredentials(bundle, credStore, flowStore, now)
	if err != nil {
		return nil, fmt.Errorf("okf: exporting credentials: %w", err)
	}

	bundle["index.md"] = renderIndex([]indexSection{
		{Heading: "Pieces", Entries: pieceEntries},
		{Heading: "Flows", Entries: flowEntries},
		{Heading: "Credentials", Entries: credEntries},
	})
	return bundle, nil
}

// exportPieces writes pieces/{name}.md for every built-in Go piece and
// every JS-authored catalog piece, plus pieces/index.md, and returns the
// root index's own "Pieces" entries. Go and JS pieces share one
// namespace and one directory — GET /catalog's own DescribeCombined
// already treats them as one merged listing for the same reason: a
// caller deciding whether to reuse a piece shouldn't have to separately
// check two places.
func exportPieces(bundle Bundle, catalogStore catalog.Store, flowStore flowstore.Store, now time.Time) ([]string, error) {
	var rootEntries []string
	var indexEntries []string

	for _, p := range pieces.All() {
		usedBy, err := flowstore.FindFlowsReferencingPiece(flowStore, p.Name)
		if err != nil {
			return nil, err
		}
		c := Concept{
			Type:        "goflow Piece",
			Title:       p.DisplayName,
			Description: "Built-in Go piece.",
			Resource:    "goflow://pieces/" + p.Name,
			Tags:        []string{"goflow", "piece", "go"},
			GeneratedAt: now,
			Body:        goPieceBody(p, usedBy),
		}
		path := "pieces/" + p.Name + ".md"
		bundle[path] = c.Render()
		rootEntries = append(rootEntries, indexEntry(p.DisplayName, "/"+path, c.Description))
		indexEntries = append(indexEntries, indexEntry(p.DisplayName, p.Name+".md", c.Description))
	}

	defs, err := catalogStore.List()
	if err != nil {
		return nil, err
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	for _, def := range defs {
		usedBy, err := flowstore.FindFlowsReferencingPiece(flowStore, def.Name)
		if err != nil {
			return nil, err
		}
		title := def.DisplayName
		if title == "" {
			title = def.Name
		}
		c := Concept{
			Type:        "goflow Piece",
			Title:       title,
			Description: def.Description,
			Resource:    "goflow://pieces/" + def.Name,
			Tags:        []string{"goflow", "piece", "js"},
			GeneratedAt: now,
			Body:        jsPieceBody(def, usedBy),
		}
		path := "pieces/" + def.Name + ".md"
		bundle[path] = c.Render()
		rootEntries = append(rootEntries, indexEntry(title, "/"+path, def.Description))
		indexEntries = append(indexEntries, indexEntry(title, def.Name+".md", def.Description))
	}

	bundle["pieces/index.md"] = renderIndex([]indexSection{{Heading: "Pieces", Entries: indexEntries}})
	return rootEntries, nil
}

// goPieceBody renders a built-in Go piece's concept body. Lighter than a
// JS piece's: piece.Action/piece.Trigger (pkg/piece) carry no
// Description/InputSchema/Examples fields the way
// catalog.ActionDefinition/TriggerDefinition do — those only exist for
// JS-authored pieces, since native Go code documents itself in its own
// source comments instead. This asymmetry is real, not a bug in this
// exporter: there is no richer data to pull from for a Go piece today.
func goPieceBody(p piece.Piece, usedBy []string) string {
	var b strings.Builder
	if len(p.Actions) > 0 {
		b.WriteString("# Actions\n\n")
		for _, name := range sortedKeys(p.Actions) {
			a := p.Actions[name]
			b.WriteString("## " + a.DisplayName + " (`" + a.Name + "`)\n\n")
		}
	}
	if len(p.Triggers) > 0 {
		b.WriteString("# Triggers\n\n")
		for _, name := range sortedTriggerKeys(p.Triggers) {
			t := p.Triggers[name]
			b.WriteString("## " + t.DisplayName + " (`" + t.Name + "`)\n\n")
		}
	}
	b.WriteString(usedBySection(usedBy))
	return b.String()
}

// jsPieceBody renders a JS-authored catalog piece's concept body — the
// full Description/InputSchema/RequiresAuth every action and trigger
// already carries in catalog.Definition, since these ARE the source of
// truth GatedStore.Save already validated (see the package doc comment
// on why nothing here is hand-authored separately).
func jsPieceBody(def catalog.Definition, usedBy []string) string {
	var b strings.Builder
	if len(def.Actions) > 0 {
		b.WriteString("# Actions\n\n")
		for _, a := range def.Actions {
			b.WriteString("## " + a.DisplayName + " (`" + a.Name + "`)\n\n")
			if a.Description != "" {
				b.WriteString(a.Description + "\n\n")
			}
			if a.InputSchema != "" {
				b.WriteString("Input: " + a.InputSchema + "\n\n")
			}
			if a.RequiresAuth != "" {
				b.WriteString("Requires auth: " + a.RequiresAuth + "\n\n")
			}
		}
	}
	if len(def.Triggers) > 0 {
		b.WriteString("# Triggers\n\n")
		for _, t := range def.Triggers {
			b.WriteString("## " + t.DisplayName + " (`" + t.Name + "`)\n\n")
			if t.Description != "" {
				b.WriteString(t.Description + "\n\n")
			}
		}
	}
	b.WriteString(usedBySection(usedBy))
	return b.String()
}

// exportFlows writes flows/{name}.md for every saved flow, plus
// flows/index.md, and returns the root index's own "Flows" entries.
func exportFlows(bundle Bundle, flowStore flowstore.Store, now time.Time) ([]string, error) {
	defs, err := flowStore.List()
	if err != nil {
		return nil, err
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

	var rootEntries []string
	var indexEntries []string
	for _, def := range defs {
		title := def.DisplayName
		if title == "" {
			title = def.Name
		}
		c := Concept{
			Type:        "goflow Flow",
			Title:       title,
			Description: def.Description,
			Resource:    "goflow://flows/" + def.Name,
			Tags:        []string{"goflow", "flow"},
			GeneratedAt: now,
			Body:        flowBody(def),
		}
		path := "flows/" + def.Name + ".md"
		bundle[path] = c.Render()
		rootEntries = append(rootEntries, indexEntry(title, "/"+path, def.Description))
		indexEntries = append(indexEntries, indexEntry(title, def.Name+".md", def.Description))
	}

	bundle["flows/index.md"] = renderIndex([]indexSection{{Heading: "Flows", Entries: indexEntries}})
	return rootEntries, nil
}

// flowBody renders a saved flow's concept body: its trigger shape and
// the OnFailureFlow/OnPauseFlow/webhook/example configuration already on
// its FlowDefinition. Deliberately does NOT walk the flow's own action
// tree to list every piece it calls (the reverse edge, piece -> "used
// by these flows", already covers the same relationship from the
// piece's own concept — see usedBySection); adding a second, forward
// edge here would mean writing a new action-tree walker this exporter
// doesn't otherwise need, real added scope for a relationship the graph
// already has from the other direction.
func flowBody(def flowstore.FlowDefinition) string {
	var b strings.Builder
	b.WriteString("# Trigger\n\n")
	b.WriteString("Type: `" + string(def.Flow.Trigger.Type) + "`\n\n")
	if def.Flow.Trigger.Type == model.TriggerPiece {
		b.WriteString(fmt.Sprintf("Piece: [%s](/pieces/%s.md), trigger `%s`\n\n", def.Flow.Trigger.PieceName, def.Flow.Trigger.PieceName, def.Flow.Trigger.TriggerName))
	}
	if def.InputSchema != "" {
		b.WriteString("Input: " + def.InputSchema + "\n\n")
	}
	if def.WebhookEnabled {
		b.WriteString("# Webhook ingress\n\n`POST /webhooks/" + def.Name + "`\n\n")
	}
	if def.OnFailureFlow != "" {
		b.WriteString(fmt.Sprintf("# On failure\n\nRuns [%s](/flows/%s.md)\n\n", def.OnFailureFlow, def.OnFailureFlow))
	}
	if def.OnPauseFlow != "" {
		b.WriteString(fmt.Sprintf("# On pause\n\nRuns [%s](/flows/%s.md)\n\n", def.OnPauseFlow, def.OnPauseFlow))
	}
	if len(def.Examples) > 0 {
		b.WriteString(fmt.Sprintf("# Examples\n\n%d worked example(s) run against this flow before every save.\n\n", len(def.Examples)))
	}
	return b.String()
}

// exportCredentials writes credentials/{name}.md for every credential
// NAME (never a decrypted value — credentials.Store.Get, the only thing
// that ever returns one, is for trusted Go callers only and is never
// wired to any transport in this project, OKF export included), plus
// credentials/index.md, and returns the root index's own "Credentials"
// entries. A nil credStore (recording/credentials disabled) produces an
// empty section, not an error.
func exportCredentials(bundle Bundle, credStore credentials.Store, flowStore flowstore.Store, now time.Time) ([]string, error) {
	if credStore == nil {
		bundle["credentials/index.md"] = renderIndex([]indexSection{{Heading: "Credentials", Entries: nil}})
		return nil, nil
	}
	names, err := credStore.List()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	var rootEntries []string
	var indexEntries []string
	for _, name := range names {
		usedBy, err := flowstore.FindFlowsReferencingCredential(flowStore, name)
		if err != nil {
			return nil, err
		}
		c := Concept{
			Type:        "goflow Credential",
			Title:       name,
			Description: "Encrypted-at-rest credential — value never exposed here or anywhere else over any transport.",
			Resource:    "goflow://credentials/" + name,
			Tags:        []string{"goflow", "credential"},
			GeneratedAt: now,
			Body:        usedBySection(usedBy),
		}
		path := "credentials/" + name + ".md"
		bundle[path] = c.Render()
		rootEntries = append(rootEntries, indexEntry(name, "/"+path, ""))
		indexEntries = append(indexEntries, indexEntry(name, name+".md", ""))
	}

	bundle["credentials/index.md"] = renderIndex([]indexSection{{Heading: "Credentials", Entries: indexEntries}})
	return rootEntries, nil
}

// usedBySection renders the "# Used by" section every piece and
// credential concept shares — the flows referencing it, via the exact
// same flowstore.FindFlowsReferencing{Piece,Credential} functions
// already backing GET /pieces/{name}/usage and
// GET /credentials/{name}/usage, so this can never disagree with those
// routes about what "used by" means.
func usedBySection(usedBy []string) string {
	var b strings.Builder
	b.WriteString("# Used by\n\n")
	if len(usedBy) == 0 {
		b.WriteString("(no saved flow currently references this)\n")
		return b.String()
	}
	sort.Strings(usedBy)
	for _, name := range usedBy {
		b.WriteString(indexEntry(name, "/flows/"+name+".md", ""))
	}
	return b.String()
}

func sortedKeys(m map[string]piece.Action) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedTriggerKeys(m map[string]piece.Trigger) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
