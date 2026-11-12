# A prompt part that holds nothing

2026-11-12

## What was wrong

`loop.UserItem` turned every content block of a prompt into a part, including a text block whose text is empty. The turn stored that user item and then built the request, and all three provider clients refuse a part with nothing set. The turn ended with a request stop before any request was sent.

The stored item stayed in the session, so every later prompt failed the same way. Reproduced on `02e98ad` against the `read_turn` recordings with the prompt followed by one empty text block: the first turn and a normal second turn in the same session both ended with `items[0].parts[1]: part has nothing set`, with no request served.

The same rule already existed for tool results (`told`, from `2026-11-12-token-use.md`). Prompts were missed.

## What changed

- `UserItem` leaves out a part with nothing set. A prompt with no part left is refused with invalid params before anything is stored, so the session is not created or changed.
- `Conversation` leaves the same parts out of a stored user message, and leaves out a user message with no part left. A session that already holds such a prompt can be used again. The stored item is not rewritten.

An embedded text resource with empty text is not covered. Its part is not empty (it has a URI), and what each provider does with it differs: the chat client sends an empty text part, the other two refuse it. `session.Supports.Prompt.EmbeddedContext` is off in `src/cli`, so a client cannot send one today. Decide it when embedded context is turned on.

## Tests

Assertions added to `TestAPromptSurvivesTheTripThroughTheTranscript` and `TestConversation` in `src/loop`.
