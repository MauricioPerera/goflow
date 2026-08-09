package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// Describe renders every Definition in store as plain text, one piece
// per block with its actions and their descriptions/input schemas —
// meant to be handed directly to an agent as context before it decides
// whether to reuse an existing piece or generate a new one. Deliberately
// plain text, not JSON: this is for a language model to read, not for a
// program to parse (a program should call store.List() directly).
// Sorted by name for stable, diffable output.
func Describe(store Store) (string, error) {
	defs, err := store.List()
	if err != nil {
		return "", fmt.Errorf("catalog: describing store: %w", err)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

	if len(defs) == 0 {
		return "(catalog is empty — no pieces saved yet)", nil
	}

	var sb strings.Builder
	for i, def := range defs {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "%s (%s)\n", def.Name, def.DisplayName)
		if def.Description != "" {
			fmt.Fprintf(&sb, "  %s\n", def.Description)
		}
		sort.Slice(def.Actions, func(i, j int) bool { return def.Actions[i].Name < def.Actions[j].Name })
		for _, a := range def.Actions {
			fmt.Fprintf(&sb, "  - %s.%s: %s\n", def.Name, a.Name, a.Description)
			if a.InputSchema != "" {
				fmt.Fprintf(&sb, "    input: %s\n", a.InputSchema)
			}
		}
	}
	return sb.String(), nil
}
