// Package csv provides goflow's "CSV" catalog piece: parse/stringify CSV
// text via encoding/csv. Correctness pitfall worth stating plainly: Go map
// iteration order is randomized, so stringify's header mode requires an
// explicit "headers" input to fix column order — never derive column order
// from ranging over a map[string]any's own keys, or output would be
// nondeterministic between otherwise-identical calls.
package csv

import (
	"encoding/csv"
	"fmt"
	"strings"

	"goflow/pkg/piece"
)

const PieceName = "csv"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "CSV",
		Actions: map[string]piece.Action{
			"parse":     parseAction(),
			"stringify": stringifyAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func delimiterRune(input map[string]any) (rune, error) {
	d, ok := input["delimiter"].(string)
	if !ok || d == "" {
		return ',', nil
	}
	runes := []rune(d)
	if len(runes) != 1 {
		return 0, fmt.Errorf("invalid delimiter %q: must be exactly one character", d)
	}
	return runes[0], nil
}

func parseAction() piece.Action {
	return piece.Action{
		Name: "parse", DisplayName: "Parse",
		Run: func(ctx piece.ActionContext) (any, error) {
			text, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			delim, err := delimiterRune(ctx.Input)
			if err != nil {
				return nil, err
			}
			hasHeader, _ := ctx.Input["hasHeader"].(bool)

			if strings.TrimSpace(text) == "" {
				if hasHeader {
					return map[string]any{"rows": []map[string]any{}}, nil
				}
				return map[string]any{"rows": [][]string{}}, nil
			}

			r := csv.NewReader(strings.NewReader(text))
			r.Comma = delim
			records, err := r.ReadAll()
			if err != nil {
				return nil, fmt.Errorf("parsing CSV: %w", err)
			}

			if !hasHeader {
				return map[string]any{"rows": records}, nil
			}
			if len(records) == 0 {
				return map[string]any{"rows": []map[string]any{}}, nil
			}
			header := records[0]
			rows := make([]map[string]any, 0, len(records)-1)
			for _, rec := range records[1:] {
				row := make(map[string]any, len(header))
				for i, h := range header {
					if i < len(rec) {
						row[h] = rec[i]
					} else {
						row[h] = ""
					}
				}
				rows = append(rows, row)
			}
			return map[string]any{"rows": rows}, nil
		},
	}
}

func stringifyAction() piece.Action {
	return piece.Action{
		Name: "stringify", DisplayName: "Stringify",
		Run: func(ctx piece.ActionContext) (any, error) {
			delim, err := delimiterRune(ctx.Input)
			if err != nil {
				return nil, err
			}

			var records [][]string
			if headersRaw, ok := ctx.Input["headers"].([]any); ok {
				headers := make([]string, len(headersRaw))
				for i, h := range headersRaw {
					headers[i], _ = h.(string)
				}
				rowsRaw, ok := ctx.Input["rows"].([]any)
				if !ok {
					return nil, fmt.Errorf("missing required input: rows ([]any of maps, since headers was provided)")
				}
				records = append(records, headers)
				for _, rowRaw := range rowsRaw {
					row, ok := rowRaw.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("each row must be a map when headers is provided, got %T", rowRaw)
					}
					rec := make([]string, len(headers))
					for i, h := range headers {
						if v, ok := row[h]; ok {
							rec[i] = fmt.Sprint(v)
						}
					}
					records = append(records, rec)
				}
			} else {
				rowsRaw, ok := ctx.Input["rows"].([]any)
				if !ok {
					return nil, fmt.Errorf("missing required input: rows ([]any of []any)")
				}
				for _, rowRaw := range rowsRaw {
					cells, ok := rowRaw.([]any)
					if !ok {
						return nil, fmt.Errorf("each row must be a []any when headers is not provided, got %T", rowRaw)
					}
					rec := make([]string, len(cells))
					for i, v := range cells {
						if s, ok := v.(string); ok {
							rec[i] = s
						} else {
							rec[i] = fmt.Sprint(v)
						}
					}
					records = append(records, rec)
				}
			}

			var sb strings.Builder
			w := csv.NewWriter(&sb)
			w.Comma = delim
			if err := w.WriteAll(records); err != nil {
				return nil, fmt.Errorf("writing CSV: %w", err)
			}
			return map[string]any{"text": sb.String()}, nil
		},
	}
}
