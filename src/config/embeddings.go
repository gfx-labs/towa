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
	Model string `koanf:"model" validate:"required_with=BaseURL,excluded_without=BaseURL"`
	// Account is the id of the account whose credential is sent. Empty
	// sends no authorization, which is what a local sidecar wants.
	Account string `koanf:"account" validate:"excluded_without=BaseURL"`
	// Dimensions is the length of vector to ask for. Zero does not ask, and
	// the model's own length is used. When it is set, a vector of another
	// length is an error.
	Dimensions int `koanf:"dimensions" validate:"gte=0"`
	// BatchSize is the most inputs sent in one request.
	BatchSize int `koanf:"batch_size" validate:"gt=0"`
	// TimeoutSeconds bounds one request.
	TimeoutSeconds int `koanf:"timeout_seconds" validate:"gt=0"`
}

var embeddingsDefaults = map[string]any{
	"embeddings.base_url":   "",
	"embeddings.model":      "",
	"embeddings.account":    "",
	"embeddings.dimensions": 0,
	// The default of --max-client-batch-size in Text Embeddings Inference,
	// the sidecar the design recommends, from the README of
	// https://github.com/huggingface/text-embeddings-inference.
	"embeddings.batch_size": 32,
	// Not measured. It has to cover a sidecar loading its model on the
	// first request.
	"embeddings.timeout_seconds": 120,
}

func (e Embeddings) validate() error {
	if err := validateFields("embeddings", e); err != nil {
		return err
	}
	if e.BaseURL == "" {
		return nil
	}
	u, err := url.Parse(e.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("embeddings.base_url: must be an http or https URL with a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("embeddings.base_url: must not have a query or fragment because a path is added to it")
	}
	return nil
}
