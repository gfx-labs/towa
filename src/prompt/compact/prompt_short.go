package compact

// The short prompt is jcode's, copied byte for byte from
// github.com/1jehuang/jcode at origin/master
// 8e11eb23ea33c7b09d43aa9a3b52fa3f0728f39e.
// TestPromptsMatchTheirSources compares it with that commit.

// shortSystem is the system prompt of the summary call in
// generate_compaction_artifact, crates/jcode-base/src/compaction.rs:1799.
const shortSystem = "You are a helpful assistant that summarizes conversations."

// shortPrompt and searchSentence, joined by one space, are SUMMARY_PROMPT
// in crates/jcode-compaction-core/src/lib.rs:85. The prompt's last sentence
// is kept apart because it is true only when the conversation_search tool
// is enabled.
const (
	shortPrompt = `Summarize our conversation so you can continue this work later.

Write in natural language with these sections:
- **Context:** What we're working on and why (1-2 sentences)
- **What we did:** Key actions taken, files changed, problems solved
- **Current state:** What works, what's broken, what's next
- **User preferences:** Specific requirements or decisions they made

Be concise but preserve important details.`
	searchSentence = `You can search the full conversation later if you need exact error messages or code snippets.`
)

// shortResultBytes is how much of a tool result jcode's summary request
// shows, in bytes: build_compaction_conversation_text,
// crates/jcode-compaction-core/src/lib.rs:217.
const shortResultBytes = 500

// emergencyFormat is the sentence jcode records when it cuts the context
// without a model summary: build_emergency_summary_text,
// crates/jcode-compaction-core/src/lib.rs:538. The arguments are the number
// of messages cut, the context's tokens and the budget, the last two in
// thousands.
const emergencyFormat = "**[Emergency compaction]**: %d messages were dropped to recover from context overflow. " +
	"The conversation had ~%dk tokens which exceeded the %dk limit."
