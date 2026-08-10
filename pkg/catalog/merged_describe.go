package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// DescribeCombined renders two clearly titled sections of plain text meant
// for an agent deciding whether to reuse an existing piece or create a new
// one: first the Go-compiled pieces (pkg/pieces, passed in as goCatalog by
// the caller so this package never imports pkg/pieces), then the JS-authored
// pieces persisted in store. goCatalog maps piece name -> one-line
// description; the caller is responsible for assembling it.
//
// Both sections are sorted by name for stable, diffable output. The JS
// section reuses renderDefinitions — the same rendering Describe uses — so a
// piece described by Describe appears identically here. When both catalogs
// are empty, the text says so in the same style Describe uses.
func DescribeCombined(store Store, goCatalog map[string]string) (string, error) {
	defs, err := store.List()
	if err != nil {
		return "", fmt.Errorf("catalog: describing combined: %w", err)
	}

	if len(goCatalog) == 0 && len(defs) == 0 {
		return "(catalog is empty — no pieces saved yet)", nil
	}

	var sb strings.Builder

	if len(goCatalog) > 0 {
		names := make([]string, 0, len(goCatalog))
		for n := range goCatalog {
			names = append(names, n)
		}
		sort.Strings(names)
		sb.WriteString("Go pieces (compiled):\n")
		for _, n := range names {
			fmt.Fprintf(&sb, "- %s: %s\n", n, goCatalog[n])
		}
	}

	if len(defs) > 0 {
		if len(goCatalog) > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("JS pieces (saved in catalog):\n")
		sb.WriteString(renderDefinitions(defs))
	}

	return sb.String(), nil
}
