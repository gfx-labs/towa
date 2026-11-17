package provider

import (
	"context"
	"errors"

	"github.com/gfx-labs/towa/src/auth"
)

// ModelInfo is what an endpoint's model list says about one model. A zero
// figure is one the list does not give.
type ModelInfo struct {
	// ID is the provider's own name for the model.
	ID string
	// ContextWindow is the context size in tokens.
	ContextWindow int64
	// MaxOutputTokens is the most tokens one response may hold.
	MaxOutputTokens int64
	// NoAdaptiveThinking says the list marks adaptive thinking as not
	// supported.
	NoAdaptiveThinking bool
	// Efforts are the reasoning efforts the list says the model takes, in
	// the provider's own words and order. Nil means the list does not say.
	Efforts []string
}

// ErrNoModelList is what Models returns for a credential whose endpoint has
// no model list that gives figures.
var ErrNoModelList = errors.New("the endpoint has no model list that gives a model's figures")

// ModelLister is a client whose endpoint lists the models an account can
// use, with their figures.
type ModelLister interface {
	Models(ctx context.Context, cred auth.Credential) ([]ModelInfo, error)
}
