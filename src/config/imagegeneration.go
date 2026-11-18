package config

// ImageGeneration configures the image generation tool the model's provider
// runs. It is offered on the built-in codex provider, to models that take
// hosted tools. The provider makes the image and towa shows it to the
// client as image content. It is off until a live check has shown the
// Codex backend accepts the tool on the models in use: a model that refuses
// it would fail every turn.
type ImageGeneration struct {
	Enabled bool `name:"enabled" help:"Offer the provider's own image generation tool on the built-in codex provider, and show the images it makes to the client. A model or route without the tool is told so in its prompt. Off by default."`
	// OutputFormat is the file format the provider encodes an image in. It
	// is sent as the tool's output_format.
	OutputFormat string `name:"output_format" default:"png" validate:"oneof=png jpeg webp" help:"Format the provider encodes a generated image in: png, jpeg or webp."`
}

func (i ImageGeneration) validate() error {
	return validateFields("image_generation", i)
}
