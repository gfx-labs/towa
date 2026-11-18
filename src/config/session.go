package config

// Session configures what a new session starts with.
type Session struct {
	// Mode is the mode a new session starts in. A client changes it for one
	// session with session/set_mode, session/set_config_option or the mode
	// key of _meta.towa on session/new.
	Mode string `name:"mode" default:"auto" validate:"oneof=auto ask" help:"Mode a new session starts in: auto runs every tool call, ask asks the client before one that writes or executes."`
	// ListPageSize is the most sessions one session/list response holds.
	// Zero means every session in one response, which is the default: no
	// measurement says where a page should end.
	ListPageSize int `name:"list_page_size" default:"0" validate:"gte=0" help:"Most sessions in one session/list response. 0 puts every session in one response."`
	// KeepHistorySeconds is how long an open session keeps its history in
	// memory after a turn. A turn within that time reads only what was
	// appended since. 15 minutes holds the history across the pauses of
	// someone at work in a session, and an idle session gives it back.
	KeepHistorySeconds int64 `name:"keep_history_seconds" default:"900" validate:"gte=0" help:"Seconds an open session keeps its history in memory after a turn, so that the next turn does not read the whole log. 0 reads the log at every turn."`
}

// Session modes.
const (
	// SessionModeAuto runs every tool call without asking.
	SessionModeAuto = "auto"
	// SessionModeAsk asks the client before a tool call that writes or
	// executes.
	SessionModeAsk = "ask"
)

func (s Session) validate() error {
	return validateFields("session", s)
}
