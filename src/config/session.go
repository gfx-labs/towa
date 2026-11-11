package config

import "fmt"

// Session configures what a new session starts with.
type Session struct {
	// Mode is the mode a new session starts in. A client changes it for one
	// session with session/set_mode, session/set_config_option or the mode
	// key of _meta.acpharness on session/new.
	Mode string `koanf:"mode"`
}

// Session modes.
const (
	// SessionModeAuto runs every tool call without asking.
	SessionModeAuto = "auto"
	// SessionModeAsk asks the client before a tool call that writes or
	// executes.
	SessionModeAsk = "ask"
)

var sessionDefaults = map[string]any{
	"session.mode": SessionModeAuto,
}

func (s Session) validate() error {
	switch s.Mode {
	case SessionModeAuto, SessionModeAsk:
		return nil
	}
	return fmt.Errorf("session.mode: %q is not %q or %q", s.Mode, SessionModeAuto, SessionModeAsk)
}
