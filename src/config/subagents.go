package config

// Config keys of the subagent limits. A refused call names the one that
// refused it.
const (
	KeySubagentsMaxDepth      = "subagents.max_depth"
	KeySubagentsMaxConcurrent = "subagents.max_concurrent"
	KeySubagentsModels        = "subagents.models"
)

// Subagents configures the child turns a session's model may start.
type Subagents struct {
	// MaxDepth is how many sessions may stand between a subagent and the
	// session the client opened, counting the subagent. 1 lets a session
	// start subagents that start none. 0 lets no session start one.
	MaxDepth int `koanf:"max_depth" validate:"gte=0"`
	// MaxConcurrent is how many subagents may run at once in the process,
	// at every depth together. A call over it is refused and does not
	// wait. 0 means no limit.
	MaxConcurrent int `koanf:"max_concurrent" validate:"gte=0"`
	// Models are the named models a call may choose for its subagent. A
	// call that names none runs the subagent on its parent's model, which
	// needs no entry here.
	Models []string `koanf:"models" validate:"unique,dive,required"`
}

// The defaults are what jcode does at 8e11eb23e. Its direct subagent tool is
// gone (crates/jcode-app-core/src/tool/tests.rs:342). The in-process child
// that is left is run_swarm_task in crates/jcode-app-core/src/server/swarm.rs:
//   - max_depth 1: the child's tool set has "subagent" and "task" removed
//     (swarm.rs:1990-1993), so a child starts no child.
//   - max_concurrent 32: default_swarm_max_concurrent_agents
//     (crates/jcode-config-types/src/lib.rs:609-611), where 0 also turns
//     the limit off (lib.rs:584-587). jcode counts the live agents of one
//     swarm. Here it counts the running subagents of the process.
//   - models empty: swarm_model_selection is false and the allowlist is
//     empty (crates/jcode-base/src/config/default_file.rs:612-613), and the
//     child takes the coordinator's model (swarm.rs:1968).
var subagentsDefaults = map[string]any{
	KeySubagentsMaxDepth:      1,
	KeySubagentsMaxConcurrent: 32,
	KeySubagentsModels:        []string{},
}

func (s Subagents) validate() error {
	return s.Validate()
}

// Validate reports settings the spawner cannot use.
func (s Subagents) Validate() error {
	return validateFields("subagents", s)
}
