package config

import "fmt"

// Session configures what a new session starts with.
type Session struct {
	// Mode is the mode a new session starts in. A client changes it for one
	// session with session/set_mode, session/set_config_option or the mode
	// key of _meta.acpharness on session/new.
	Mode string `koanf:"mode"`
	// ListPageSize is the most sessions one session/list response holds.
	// Zero means every session in one response, which is the default: no
	// measurement says where a page should end.
	ListPageSize int `koanf:"list_page_size"`
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
	"session.mode":           SessionModeAuto,
	"session.list_page_size": 0,
}

func (s Session) validate() error {
	if s.ListPageSize < 0 {
		return fmt.Errorf("session.list_page_size: %d is negative", s.ListPageSize)
	}
	switch s.Mode {
	case SessionModeAuto, SessionModeAsk:
		return nil
	}
	return fmt.Errorf("session.mode: %q is not %q or %q", s.Mode, SessionModeAuto, SessionModeAsk)
}
