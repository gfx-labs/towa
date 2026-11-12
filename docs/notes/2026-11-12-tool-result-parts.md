# providers: a tool result the API cannot carry is still sent

2026-11-12

## The bug

A tool's result is stored before the next request is built from it. All three wire clients refused to build a request when a stored result held a part they had no form for, so the turn failed, and every later turn of the session failed on the same stored result.

| part in a tool result | `chat` | `responses` | `messages` |
|---|---|---|---|
| audio | sent (see `2026-11-12-chat-tool-result-images.md`) | refused | refused |
| resource with data that is not an image (or a PDF, outside `chat`) | refused | refused | refused |
| resource with no text and no data | sent as empty text | refused | refused |

An MCP tool can return each of these (`resultParts` in `src/mcp/tools.go`). The built-in tools return text and, from `read`, an image.

Reproduced with the built binary over ACP against Ollama 0.24.0 (`qwen2.5:7b`) and a stdio MCP server built with the Go SDK, named in `session/new`. Each session ran the prompt "Call the <tool> tool once, then say done." and then "Reply with the single word: ready". Both prompts of each session ended with the same error:

- provider kind `chat`, a tool returning text and an `application/zip` resource: `items[2].parts[1]: Chat Completions cannot carry a resource of type "application/zip"`
- provider kind `responses`, a tool returning text and `audio/wav`: `item 2: the Responses API takes no audio input`

The `messages` client was not run end to end: `src/cli/providers.go` gives it Anthropic's own base URL, and its accounts need a login. Its refusal was reproduced at the request builder, as the other two were, with a scratch test.

## The fix

- `provider.LeftOut` (`src/provider/leftout.go`) is the text that takes the place of such a part: what the tool returned, its type and size, and that the API cannot carry it. The model reads it in the result and can tell the user or try another way.
- `buildContent` in `src/provider/anthropic/request.go` and `src/provider/openai/request.go` takes a flag that says the parts are a tool result's. In a result, audio and a resource `buildResource` refuses become that text. In a prompt they are still an error: a prompt is refused before it is stored, and `session.checkPrompt` already refuses the types a session does not advertise.
- `buildResource` in both sends a resource with no text and no data as its empty text. It is an empty file, not a binary one.
- `messages` in `src/provider/chat/request.go` does the same for a part `binaryPart` refuses: a resource that is not an image, and audio whose type is not `audio/...`.

Nothing is dropped without a word: the model is told, and the client was already shown the content by the tool call update.

After the fix the same two sessions end both prompts with `end_turn` and log no error. In the `chat` session the model called the tool twice before it answered.

## Tests

`TestAToolResultTheAPICannotCarryIsStillSent` in `src/provider/boundaries_test.go` sends one result holding text, audio, a zip resource and an empty text resource through each of the three clients to a server that answers 503. It asserts that the request arrives, that it holds the text and names the resource, and that the archive's bytes are not in it. It fails for all three clients before the fix. The case in `chat`'s `TestRequestRejectsWhatItCannotSend` that asserted the refusal of a file in a result now asserts it for a prompt.

## Not done

- Audio from a tool is not sent to the Messages or Responses API in any form. Neither has an audio input part in the request shapes this client writes.
- A PDF from a tool is still left out on `chat`, which has no document part here.
- Whether a model does anything useful with the text was not measured. `qwen2.5:7b` went on to answer in both sessions.
