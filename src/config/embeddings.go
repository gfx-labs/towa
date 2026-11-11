package config

import (
	"fmt"
	"net/url"
)

// Embeddings configures the one embeddings endpoint, which speaks the
// OpenAI /v1/embeddings shape. An empty BaseURL means there is none.
type Embeddings struct {
	// BaseURL is the endpoint's root, such as http://127.0.0.1:8080/v1.
	// "/embeddings" is added to it.
	BaseURL string `koanf:"base_url"`
	// Model is the endpoint's own name for the model.
	Model string `koanf:"model"`
	// Account is the id of the account whose credential is sent. Empty
	// sends no authorization, which is what a local sidecar wants.
	Account string `koanf:"account"`
	// Dimensions is the length of vector to ask for. Zero does not ask, and
	// the model's own length is used. When it is set, a vector of another
	// length is an error.
	Dimensions int `koanf:"dimensions"`
	// BatchSize is the most inputs sent in one request.
	BatchSize int `koanf:"batch_size"`
	// TimeoutSeconds bounds one request.
	TimeoutSeconds int `koanf:"timeout_seconds"`
}

var embeddingsDefaults = map[string]any{
	"embeddings.base_url":   "",
	"embeddings.model":      "",
	"embeddings.account":    "",
	"embeddings.dimensions": 0,
	// The default --max-client-batch-size of Text Embeddings Inference,
	// the sidecar the design recommends. OpenAI takes 2048.
	"embeddings.batch_size": 32,
	// Not measured. It has to cover a sidecar loading its model on the
	// first request.
	"embeddings.timeout_seconds": 120,
}

func (e Embeddings) validate() error {
	switch {
	case e.Dimensions < 0:
		return fmt.Errorf("embeddings.dimensions: %d is negative", e.Dimensions)
	case e.BatchSize <= 0:
		return fmt.Errorf("embeddings.batch_size: %d is not positive", e.BatchSize)
	case e.TimeoutSeconds <= 0:
		return fmt.Errorf("embeddings.timeout_seconds: %d is not positive", e.TimeoutSeconds)
	}
	if e.BaseURL == "" {
		// A model or an account without an endpoint is a config that was
		// meant to do something and does nothing.
		switch {
		case e.Model != "":
			return fmt.Errorf("embeddings.model is %q and embeddings.base_url is empty", e.Model)
		case e.Account != "":
			return fmt.Errorf("embeddings.account is %q and embeddings.base_url is empty", e.Account)
		}
		return nil
	}
	u, err := url.Parse(e.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("embeddings.base_url: %q is not an http or https URL", e.BaseURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("embeddings.base_url: %q has a query or a fragment, and a path is added to it", e.BaseURL)
	}
	if e.Model == "" {
		return fmt.Errorf("embeddings.model is empty")
	}
	return nil
}
