package config

// ImageGeneration configures OpenAI's hosted image generation tool. The
// provider runs the tool inside the response and returns the image, which
// towa passes to the ACP client as image content.
//
// It is off by default: a request that carries the tool is refused by a
// model or account that does not have it, and that would fail every turn,
// text included. The tool's fields are from
// https://developers.openai.com/api/docs/guides/tools-image-generation.
type ImageGeneration struct {
	Enabled bool `name:"enabled" help:"Give the model OpenAI's hosted image generation tool, on the built-in codex provider. The model must support the tool and the account must have access to it."`
	// Quality is the tool's quality setting.
	Quality string `name:"quality" default:"auto" validate:"oneof=auto low medium high" help:"Quality of a generated image: auto, low, medium or high."`
	// OutputFormat is the image's file format, which is also its MIME type.
	OutputFormat string `name:"output_format" default:"png" validate:"oneof=png jpeg webp" help:"File format of a generated image: png, jpeg or webp."`
}

func (i ImageGeneration) validate() error {
	return validateFields("imagegeneration", i)
}
