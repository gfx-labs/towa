package config

// ImageGeneration configures the image generation tool the model's provider
// runs. It is offered on the built-in codex provider, to models that take
// hosted tools. The provider makes the image and towa shows it to the
// client as image content.
type ImageGeneration struct {
	Enabled bool `name:"enabled" default:"true" help:"Offer the provider's own image generation tool on the built-in codex provider, and show the images it makes to the client. A model or route without the tool is told so in its prompt."`
	// OutputFormat is the file format the provider encodes an image in. It
	// is sent as the tool's output_format.
	OutputFormat string `name:"output_format" default:"png" validate:"oneof=png jpeg webp" help:"Format the provider encodes a generated image in: png, jpeg or webp."`
}

func (i ImageGeneration) validate() error {
	return validateFields("image_generation", i)
}
