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
	BaseURL string `name:"base_url" placeholder:"URL" help:"Root of the embeddings endpoint, such as http://127.0.0.1:8080/v1. Empty means there is none."`
	// Model is the endpoint's own name for the model.
	Model string `name:"model" validate:"required_with=BaseURL,excluded_without=BaseURL" help:"The endpoint's own name for the model."`
	// Account is the id of the account whose credential is sent. Empty
	// sends no authorization, which is what a local sidecar wants.
	Account string `name:"account" validate:"excluded_without=BaseURL" help:"Id of the account whose credential is sent. Empty sends no authorization."`
	// Dimensions is the length of vector to ask for. Zero does not ask, and
	// the model's own length is used. When it is set, a vector of another
	// length is an error.
	Dimensions int `name:"dimensions" default:"0" validate:"gte=0" help:"Length of vector to ask for. 0 does not ask, and the model's own length is used."`
	// BatchSize is the most inputs sent in one request. The default is the
	// default of --max-client-batch-size in Text Embeddings Inference, the
	// sidecar the design recommends, from the README of
	// https://github.com/huggingface/text-embeddings-inference.
	BatchSize int `name:"batch_size" default:"32" validate:"gt=0" help:"Most inputs sent in one request."`
	// TimeoutSeconds bounds one request. The default is not measured. It
	// has to cover a sidecar loading its model on the first request.
	TimeoutSeconds int `name:"timeout_seconds" default:"120" validate:"gt=0" help:"Seconds one request may take."`
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
