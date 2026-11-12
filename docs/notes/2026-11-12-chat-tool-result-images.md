# chat: a tool result's image goes in a user message after the results

2026-11-12

## The bug

The Chat Completions client refused a request whose history held a tool result with an image, audio or a binary resource (`a Chat Completions tool result cannot carry binary content`). The `read` tool returns an image for an image file, and an MCP tool can return an image or audio. The result is stored before the request is built, so the turn failed and so did every later turn of the session.

Reproduced with the built binary over ACP against Ollama 0.24.0 (`qwen2.5:7b`, provider kind `chat`, account kind `none`): the prompt "Call the read tool on the file pixel.png, then say done." ended with

```
turn stopped: request: provider: request (fail): items[2].parts[0]: a Chat Completions tool result cannot carry binary content: at lan:open/qwen2.5:7b
```

and the next prompt in the same session, which asked for one word, ended with the same error.

## The fix

`messages` in `src/provider/chat/request.go`:

- The tool message keeps the result's text and ends with a line saying that the rest is in the user message after the tool results.
- The content goes in one user message placed after the run of tool results: for each result, a text part naming the tool call id, then its image or audio parts. The old code gave up because a user message between the results of one response's calls is refused by endpoints, which was not measured here. After the last of them it is not between any.
- A binary resource that is not an image is still an error, as it is in a user message.
- `binaryPart` holds the conversion that `userContent` had, so a prompt and a tool result turn a part into the same wire part.

Measured after the fix:

- The same two prompts through the binary: both turns end with `end_turn`, and the log has no error.
- `gemma4:e4b`, which reads images, through the client: two calls, the first result a 96 by 96 red PNG and the second text. Asked for the colour in one word, it answered `Red`.
- By `curl` against Ollama, the same shape (two tool messages, then a user message with an `image_url` part) is taken by `qwen2.5:7b` and `gemma4:e4b` with status 200.

## Tests

`TestRequestBodyForAPlainEndpoint` has an image in the first of three tool results and asserts the three tool messages in order followed by the user message. It fails on the client before the fix. `TestRequestRejectsWhatItCannotSend` keeps a case for a result holding a PDF.

## Not done

- Nothing says whether a model reads images. A model that does not is still sent the image, and what it does with it is the endpoint's business: Ollama's `qwen2.5:7b` answered without it. `models.go` has no such field and none was added.
- Only Ollama was measured. No hosted endpoint was reachable with credit. Two reports from other projects say the same shape is taken where an image inside the tool message is refused: xai-org/plugin-marketplace issue 1554 for a DeepSeek model, and atlasfutures/semantic-router issue 255 for a Moonshot model. Neither was checked here.
- `docs/notes/2026-11-11-dialect-codex.md` leaves `view_image` out of the Codex tool set and gives Chat's refusal as one of two reasons. That reason is gone. The tool is still left out, for the other one.
