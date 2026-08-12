// Package okf renders goflow's live catalog (pieces), flowstore (flows),
// and credentials (names only, never values) as an Open Knowledge Format
// v0.2 bundle — https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md
// — so an AI agent (or a human with a plain markdown viewer) can browse
// this project's own catalog the same way it would browse any other OKF
// bundle, with no goflow-specific tooling required.
//
// Every concept here is GENERATED from the live Store data at request
// time (see export.go), never hand-authored — the same "one source of
// truth, derived views don't drift" discipline this project already
// applies everywhere else (buildRegistry, catalog.DescribeCombined,
// GET /pieces/export). There is deliberately no persisted OKF bundle on
// disk to go stale.
//
// One real, disclosed limitation: OKF's trust-tier mechanism (§5.2 of
// the spec) is built around knowing WHO produced or confirmed a concept
// — generated.by as human:<id> or <agent>/<version>, verified.by the
// same. goflow has no per-caller identity at all (a single shared
// bearer token, no accounts — see README's OAuth entry for why that's a
// deliberate scope boundary, not an oversight), so there is no real
// actor to attribute a piece or flow's authorship to. Every concept here
// sets generated.by to process:goflow-okf-export — an honest statement
// about what produced THIS DOCUMENT (and when), not a claim about who
// authored the underlying piece or flow. verified is omitted entirely:
// nothing in goflow records a human confirming a piece/flow, so every
// concept is genuinely, correctly "unverified" in OKF's own sense —
// absence here is accurate, not a placeholder.
package okf

import (
	"fmt"
	"strings"
	"time"
)

// generatedByProcess is the actor (§7 of the spec) every concept's
// generated.by is set to — see the package doc comment for why this
// names the export process, not the underlying piece/flow's author.
const generatedByProcess = "process:goflow-okf-export"

// Concept is one OKF concept document: a YAML frontmatter block plus a
// markdown body (§4 of the spec). Only Type is required by the spec;
// every other field is optional here too and simply omitted from the
// rendered frontmatter when empty.
type Concept struct {
	Type        string
	Title       string
	Description string
	Resource    string
	Tags        []string
	GeneratedAt time.Time
	Body        string
}

// Render encodes c as a conformant OKF concept document: a YAML
// frontmatter block delimited by "---" lines, followed by the markdown
// body. Hand-rolled rather than pulling in a YAML library — every field
// here is a plain string, string list, or a small generated{by,at} map
// this package itself controls, well within what a minimal, always-
// double-quoted-scalar emitter can encode correctly without a general
// YAML implementation (matching this project's stdlib-only stance the
// same way pkg/license's Ed25519 choice and pkg/mcpapi's hand-written
// JSON-RPC already do for their own "simple enough not to need a
// dependency" cases).
func (c Concept) Render() string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("type: " + yamlQuote(c.Type) + "\n")
	if c.Title != "" {
		b.WriteString("title: " + yamlQuote(c.Title) + "\n")
	}
	if c.Description != "" {
		b.WriteString("description: " + yamlQuote(c.Description) + "\n")
	}
	if c.Resource != "" {
		b.WriteString("resource: " + yamlQuote(c.Resource) + "\n")
	}
	if len(c.Tags) > 0 {
		quoted := make([]string, len(c.Tags))
		for i, t := range c.Tags {
			quoted[i] = yamlQuote(t)
		}
		b.WriteString("tags: [" + strings.Join(quoted, ", ") + "]\n")
	}
	if !c.GeneratedAt.IsZero() {
		b.WriteString(fmt.Sprintf("generated: { by: %s, at: %s }\n", generatedByProcess, c.GeneratedAt.UTC().Format(time.RFC3339)))
	}
	b.WriteString("---\n\n")
	b.WriteString(c.Body)
	return b.String()
}

// yamlQuote renders s as a YAML double-quoted scalar — always quoted,
// never bare, so this never has to decide whether a given string
// "needs" quoting (a colon, a leading special character, a value that
// looks like a YAML bool/number all quietly break an unquoted scalar).
// Double-quoted YAML strings use C-style backslash escapes, so this only
// ever needs to escape backslash and the closing quote itself.
func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

// ValidateConformance checks doc against OKF §11's conformance rule for
// a single concept document: a frontmatter block delimited by "---"
// lines, containing a non-empty type field. This is deliberately NOT a
// general YAML parser — it exists to self-check this package's own
// Render output (exercised in tests), not to validate arbitrary
// third-party OKF documents, which would need real YAML parsing this
// project has no dependency for for (see the package doc comment).
func ValidateConformance(doc string) error {
	if !strings.HasPrefix(doc, "---\n") {
		return fmt.Errorf("okf: document does not start with a \"---\" frontmatter delimiter")
	}
	rest := doc[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end == -1 {
		return fmt.Errorf("okf: no closing \"---\" frontmatter delimiter found")
	}
	frontmatter := rest[:end]
	for _, line := range strings.Split(frontmatter, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "type:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "type:"))
		value = strings.Trim(value, `"`)
		if value == "" {
			return fmt.Errorf("okf: type field is present but empty")
		}
		return nil
	}
	return fmt.Errorf("okf: no non-empty type field found in frontmatter")
}

// indexEntry renders one §8 index.md list line: "* [title](path) -
// description". description is omitted (along with the trailing " - ")
// when empty, matching the spec's own worked examples.
func indexEntry(title, path, description string) string {
	if description == "" {
		return fmt.Sprintf("* [%s](%s)\n", title, path)
	}
	return fmt.Sprintf("* [%s](%s) - %s\n", title, path, description)
}

// renderIndex builds a §8 index.md body from an ordered list of
// sections, each a heading plus its own list of already-rendered
// indexEntry lines. No frontmatter — §8 permits one only on a
// bundle-root index.md, for okf_version, which this package does not
// emit (every consumer of this bundle is a live goflow deployment, not
// a distributed archive versioned independently of the server it came
// from).
func renderIndex(sections []indexSection) string {
	var b strings.Builder
	for i, s := range sections {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("# " + s.Heading + "\n\n")
		if len(s.Entries) == 0 {
			b.WriteString("(none)\n")
			continue
		}
		for _, e := range s.Entries {
			b.WriteString(e)
		}
	}
	return b.String()
}

type indexSection struct {
	Heading string
	Entries []string
}
