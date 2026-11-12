package config

// Claude configures the Messages API client.
type Claude struct {
	// Thinking is the thinking parameter of a request. Measured on
	// 2026-11-12 with a subscription login
	// (docs/notes/2026-11-12-claude-thinking.md): a request with no
	// thinking parameter still gets thinking blocks from Opus 5.5, but their
	// text is empty, so a client is shown nothing. "adaptive" alone is the
	// same. The text comes only when a display is asked for.
	//
	// Of the nine Claude models in the built-in table, claude-haiku-4-5
	// alone refuses adaptive thinking, with an HTTP 400 that says so. The
	// table marks it, and a request to it carries no thinking parameter.
	Thinking string `name:"thinking" default:"adaptive" validate:"oneof=adaptive off" help:"Thinking parameter of a request: adaptive lets the model decide how much to think, off sends no thinking parameter. A model the built-in table marks as without adaptive thinking (claude-haiku-4-5) is sent none."`
	// ThinkingDisplay is the display value sent with adaptive thinking.
	// Empty sends none, and the thinking then arrives without text.
	ThinkingDisplay string `name:"thinking_display" default:"summarized" help:"Display value sent with adaptive thinking, as the API names it. summarized returns a summary of the thinking, which an ACP client is shown as the agent's thoughts. Empty sends none, and the thinking then arrives without text."`
}

// Thinking parameters of the Messages API client.
const (
	ClaudeThinkingAdaptive = "adaptive"
	ClaudeThinkingOff      = "off"
)

func (c Claude) validate() error {
	return validateFields("claude", c)
}
