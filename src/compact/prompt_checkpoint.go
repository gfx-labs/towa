package compact

// The checkpoint prompts are Pi's, copied byte for byte from
// github.com/earendil-works/pi at commit
// 4839e41d6463d759a92ee55f4ca7c8ae57e52c9c (MIT, Copyright (c) 2025 Mario
// Zechner). TestPromptsMatchTheirSources compares them with that commit.

// checkpointSystem is SUMMARIZATION_SYSTEM_PROMPT in
// packages/coding-agent/src/core/compaction/utils.ts.
const checkpointSystem = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

// checkpointPrompt is SUMMARIZATION_PROMPT in
// packages/coding-agent/src/core/compaction/compaction.ts.
const checkpointPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// checkpointMergePrompt is UPDATE_SUMMARIZATION_PROMPT in the same file,
// with UPDATE_SUMMARIZATION_INSTRUCTIONS put in where the template names
// it. It is used when an earlier summary exists.
const checkpointMergePrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// summaryPrefix and summarySuffix are COMPACTION_SUMMARY_PREFIX and
// COMPACTION_SUMMARY_SUFFIX in packages/coding-agent/src/core/messages.ts.
// The summary is given to the model between them, as a user message.
const (
	summaryPrefix = `The conversation history before this point was compacted into the following summary:

<summary>
`
	summarySuffix = `
</summary>`
)

// checkpointResultChars is TOOL_RESULT_MAX_CHARS in
// packages/coding-agent/src/core/compaction/utils.ts: how much of a tool
// result the summary request shows, in UTF-16 units as Pi counts them.
const checkpointResultChars = 2000
