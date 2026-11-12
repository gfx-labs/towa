package config

// Config keys of the subagent limits. A refused call names the one that
// refused it.
const (
	KeySubagentsMaxDepth      = "subagents.max_depth"
	KeySubagentsMaxConcurrent = "subagents.max_concurrent"
	KeySubagentsModels        = "subagents.models"
)

// Subagents configures the child turns a session's model may start.
//
// max_depth is 0 by default, so the model is not given the tool: its
// definition is sent with every request and most sessions start no child
// (docs/notes/2026-11-12-token-use.md). The other defaults are what jcode
// does at 8e11eb23e. Its direct subagent tool is gone
// (crates/jcode-app-core/src/tool/tests.rs:342). The in-process child that
// is left is run_swarm_task in crates/jcode-app-core/src/server/swarm.rs:
//   - jcode's depth is 1: the child's tool set has "subagent" and "task"
//     removed (swarm.rs:1990-1993), so a child starts no child.
//   - max_concurrent 32: default_swarm_max_concurrent_agents
//     (crates/jcode-config-types/src/lib.rs:609-611), where 0 also turns
//     the limit off (lib.rs:584-587). jcode counts the live agents of one
//     swarm. Here it counts the running subagents of the process.
//   - models empty: swarm_model_selection is false and the allowlist is
//     empty (crates/jcode-base/src/config/default_file.rs:612-613), and the
//     child takes the coordinator's model (swarm.rs:1968).
type Subagents struct {
	// MaxDepth is how many sessions may stand between a subagent and the
	// session the client opened, counting the subagent. 1 lets a session
	// start subagents that start none. 0 lets no session start one, and
	// leaves the tool out.
	MaxDepth int `name:"max_depth" default:"0" validate:"gte=0" help:"Sessions that may stand between a subagent and the session the client opened, counting the subagent. 1 gives the model the subagent tool. 0 lets no session start one."`
	// MaxConcurrent is how many subagents may run at once in the process,
	// at every depth together. A call over it is refused and does not
	// wait. 0 means no limit.
	MaxConcurrent int `name:"max_concurrent" default:"32" validate:"gte=0" help:"Subagents that may run at once in the process. A call over it is refused. 0 is no limit."`
	// Models are the entries of the models list a call may choose for its subagent. A
	// call that names none runs the subagent on its parent's model, which
	// needs no entry here.
	Models []string `name:"models" placeholder:"MODEL" validate:"unique,dive,required" help:"Entries of models a call may choose for its subagent, each written as it is in models. A call that names none uses its parent's model."`
}

func (s Subagents) validate() error {
	return s.Validate()
}

// Validate reports settings the spawner cannot use.
func (s Subagents) Validate() error {
	return validateFields("subagents", s)
}
