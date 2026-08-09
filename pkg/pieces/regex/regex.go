// Package regex provides goflow's "Regex" catalog piece: match/find/replace/
// extract-groups over Go's regexp package. "No match" is a normal, valid
// outcome for match/find_all/extract_groups — never an error, so a flow can
// branch on the result itself (e.g. via a ROUTER checking "matched"), the
// same philosophy as pkg/pieces/http's failOnErrorStatus being opt-in.
// Only an invalid pattern (fails to compile) is an error.
package regex

import (
	"fmt"
	"regexp"

	"goflow/pkg/piece"
)

const PieceName = "regex"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Regex",
		Actions: map[string]piece.Action{
			"match":          matchAction(),
			"find_all":       findAllAction(),
			"replace":        replaceAction(),
			"extract_groups": extractGroupsAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func compile(ctx piece.ActionContext) (*regexp.Regexp, string, error) {
	text, ok := ctx.Input["text"].(string)
	if !ok {
		return nil, "", fmt.Errorf("missing required input: text (string)")
	}
	pattern, ok := ctx.Input["pattern"].(string)
	if !ok {
		return nil, "", fmt.Errorf("missing required input: pattern (string)")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, "", fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}
	return re, text, nil
}

func matchAction() piece.Action {
	return piece.Action{
		Name: "match", DisplayName: "Match",
		Run: func(ctx piece.ActionContext) (any, error) {
			re, text, err := compile(ctx)
			if err != nil {
				return nil, err
			}
			m := re.FindString(text)
			return map[string]any{"matched": re.MatchString(text), "match": m}, nil
		},
	}
}

func findAllAction() piece.Action {
	return piece.Action{
		Name: "find_all", DisplayName: "Find All",
		Run: func(ctx piece.ActionContext) (any, error) {
			re, text, err := compile(ctx)
			if err != nil {
				return nil, err
			}
			matches := re.FindAllString(text, -1)
			if matches == nil {
				matches = []string{}
			}
			return map[string]any{"matches": matches}, nil
		},
	}
}

func replaceAction() piece.Action {
	return piece.Action{
		Name: "replace", DisplayName: "Replace",
		Run: func(ctx piece.ActionContext) (any, error) {
			re, text, err := compile(ctx)
			if err != nil {
				return nil, err
			}
			replacement, ok := ctx.Input["replacement"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: replacement (string)")
			}
			return map[string]any{"text": re.ReplaceAllString(text, replacement)}, nil
		},
	}
}

func extractGroupsAction() piece.Action {
	return piece.Action{
		Name: "extract_groups", DisplayName: "Extract Groups",
		Run: func(ctx piece.ActionContext) (any, error) {
			re, text, err := compile(ctx)
			if err != nil {
				return nil, err
			}
			m := re.FindStringSubmatch(text)
			if m == nil {
				return map[string]any{"fullMatch": "", "groups": []string{}}, nil
			}
			groups := m[1:]
			if groups == nil {
				groups = []string{}
			}
			return map[string]any{"fullMatch": m[0], "groups": groups}, nil
		},
	}
}
