package okf

import (
	"strings"
	"testing"
	"time"
)

func TestConcept_Render_IsConformant(t *testing.T) {
	c := Concept{
		Type:        "goflow Piece",
		Title:       "Stripe",
		Description: "Create charges and refunds.",
		Resource:    "goflow://pieces/stripe",
		Tags:        []string{"goflow", "piece", "js"},
		GeneratedAt: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
		Body:        "# Actions\n\nsome body text\n",
	}
	doc := c.Render()
	if err := ValidateConformance(doc); err != nil {
		t.Fatalf("ValidateConformance: %v; doc=%s", err, doc)
	}
	if !strings.Contains(doc, `title: "Stripe"`) {
		t.Fatalf("doc missing rendered title: %s", doc)
	}
	if !strings.Contains(doc, "generated: { by: process:goflow-okf-export, at: 2026-08-12T00:00:00Z }") {
		t.Fatalf("doc missing rendered generated block: %s", doc)
	}
	if !strings.Contains(doc, "some body text") {
		t.Fatalf("doc missing body: %s", doc)
	}
}

func TestConcept_Render_OnlyTypeSet_StillConformant(t *testing.T) {
	// §11: type is the only always-required field — a concept carrying
	// just type is fully conformant.
	c := Concept{Type: "goflow Flow"}
	doc := c.Render()
	if err := ValidateConformance(doc); err != nil {
		t.Fatalf("ValidateConformance: %v; doc=%s", err, doc)
	}
	if strings.Contains(doc, "title:") || strings.Contains(doc, "generated:") {
		t.Fatalf("doc = %s, want no optional fields rendered when unset", doc)
	}
}

func TestConcept_Render_EscapesSpecialCharacters(t *testing.T) {
	c := Concept{
		Type:        "goflow Piece",
		Title:       `A "quoted" title with \backslash\ and a: colon`,
		Description: "line one\nline two",
	}
	doc := c.Render()
	if err := ValidateConformance(doc); err != nil {
		t.Fatalf("ValidateConformance: %v; doc=%s", err, doc)
	}
	// The escaped backslash-quote sequence must survive intact — this is
	// a structural check that yamlQuote actually escaped rather than
	// leaving a raw unescaped quote that would break the YAML block.
	if !strings.Contains(doc, `\"quoted\"`) {
		t.Fatalf("doc = %s, want the embedded quotes escaped", doc)
	}
	if !strings.Contains(doc, `\\backslash\\`) {
		t.Fatalf("doc = %s, want the embedded backslashes escaped", doc)
	}
	if !strings.Contains(doc, `line one\nline two`) {
		t.Fatalf("doc = %s, want the embedded newline escaped to \\n on one line", doc)
	}
}

func TestValidateConformance_MissingType_Rejected(t *testing.T) {
	doc := "---\ntitle: \"no type here\"\n---\n\nbody\n"
	if err := ValidateConformance(doc); err == nil {
		t.Fatal("ValidateConformance succeeded on a document with no type field, want an error")
	}
}

func TestValidateConformance_EmptyType_Rejected(t *testing.T) {
	doc := "---\ntype: \"\"\n---\n\nbody\n"
	if err := ValidateConformance(doc); err == nil {
		t.Fatal("ValidateConformance succeeded on an empty type field, want an error")
	}
}

func TestValidateConformance_NoFrontmatter_Rejected(t *testing.T) {
	if err := ValidateConformance("just a plain markdown file\n"); err == nil {
		t.Fatal("ValidateConformance succeeded on a document with no frontmatter, want an error")
	}
}

func TestValidateConformance_UnclosedFrontmatter_Rejected(t *testing.T) {
	doc := "---\ntype: \"goflow Piece\"\n\nbody with no closing delimiter\n"
	if err := ValidateConformance(doc); err == nil {
		t.Fatal("ValidateConformance succeeded on unclosed frontmatter, want an error")
	}
}

func TestRenderIndex_EmptySection_SaysNone(t *testing.T) {
	doc := renderIndex([]indexSection{{Heading: "Credentials", Entries: nil}})
	if !strings.Contains(doc, "# Credentials") || !strings.Contains(doc, "(none)") {
		t.Fatalf("doc = %s, want a Credentials heading and a (none) placeholder", doc)
	}
}

func TestIndexEntry_WithAndWithoutDescription(t *testing.T) {
	withDesc := indexEntry("Stripe", "stripe.md", "Payments")
	if withDesc != "* [Stripe](stripe.md) - Payments\n" {
		t.Fatalf("indexEntry with description = %q", withDesc)
	}
	withoutDesc := indexEntry("Stripe", "stripe.md", "")
	if withoutDesc != "* [Stripe](stripe.md)\n" {
		t.Fatalf("indexEntry without description = %q", withoutDesc)
	}
}
