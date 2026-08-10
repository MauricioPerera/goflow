// Package pieces is goflow's catalog: a small set of ready-made,
// independently-tested pieces (pkg/pieces/<name>), each with its own New()
// constructor that builds and validates itself (piece.MustValidate — see
// the goflow-piece-authoring skill for why that matters). This file is just
// the aggregator; there's no dynamic discovery, no marketplace, no
// versioning — add a piece here by importing its package and listing it in
// RegisterAll, same as any other Go dependency.
package pieces

import (
	"fmt"

	"goflow/pkg/piece"
	"goflow/pkg/pieces/approval"
	base64piece "goflow/pkg/pieces/base64"
	cryptopiece "goflow/pkg/pieces/crypto"
	csvpiece "goflow/pkg/pieces/csv"
	"goflow/pkg/pieces/datetime"
	"goflow/pkg/pieces/delay"
	emailpiece "goflow/pkg/pieces/email"
	hashpiece "goflow/pkg/pieces/hash"
	httppiece "goflow/pkg/pieces/http"
	"goflow/pkg/pieces/json"
	regexpiece "goflow/pkg/pieces/regex"
	"goflow/pkg/pieces/schedule"
	"goflow/pkg/pieces/storage"
	"goflow/pkg/pieces/text"
	"goflow/pkg/pieces/uuid"
	"goflow/pkg/pieces/webhook"
	"goflow/pkg/pieces/webhookreply"
)

// All returns a fresh instance of every catalog piece. Fresh each call,
// deliberately — a Piece's Actions/Triggers maps hold closures, and none of
// the catalog pieces need to share state across Registry instances. A piece
// that DOES need shared state (e.g. a rate limiter) would build that into
// its own New(); none of today's pieces need it, so there's no existing
// example of the pattern to point to yet.
func All() []piece.Piece {
	return []piece.Piece{
		httppiece.New(),
		json.New(),
		delay.New(),
		webhook.New(),
		schedule.New(),
		cryptopiece.New(),
		storage.New(),
		approval.New(),
		webhookreply.New(),
		text.New(),
		datetime.New(),
		hashpiece.New(),
		regexpiece.New(),
		csvpiece.New(),
		uuid.New(),
		base64piece.New(),
		emailpiece.New(),
	}
}

// RegisterAll registers every catalog piece into r via
// Registry.RegisterValidated, stopping at the first failure. Since every
// catalog piece already validates itself in its own New() (MustValidate,
// which panics on failure), a RegisterAll failure here would mean
// RegisterValidated caught something MustValidate didn't — which shouldn't
// happen for the pieces in this package, but RegisterAll uses the
// non-panicking path anyway since that's the interface a caller assembling
// pieces at runtime (rather than at this package's own init time) should
// get.
func RegisterAll(r *piece.Registry) error {
	for _, p := range All() {
		if err := r.RegisterValidated(p); err != nil {
			return fmt.Errorf("registering piece %q: %w", p.Name, err)
		}
	}
	return nil
}
