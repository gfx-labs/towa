# chat: a tool call's arguments are stored and sent as an object

2026-11-12

## The bug

The Chat Completions client stored a tool call's argument text as it came when the text was valid JSON, and in a JSON string otherwise. It sent the stored text back unquoted. Two cases broke a session:

- A call with no argument text. Some endpoints write a call to a tool that takes nothing as `"arguments": ""` (vLLM issue 19419, CLIProxyAPI issue 6516, Vercel AI SDK issue 6687). The client stored `""`. The tool's schema asks for an object, so the call failed with `schema_mismatch` and never ran.
- Argument text the model left unfinished, such as `{"command":`.

In both cases the next request carried `"arguments": ""` or `"arguments": "{\"command\":"` in the history. A server that parses the member refuses the whole request, and the call stays in the history, so every later turn of the session failed the same way.

Measured against Ollama 0.24.0 (`llama3.2:3b`) through the client, with one stored call and its result in the history:

| stored input | before | after |
|---|---|---|
| `""` | 400 `invalid tool call arguments` | 200 |
| `"{\"command\":"` | 400 `invalid tool call arguments` | 200 |
| `"{}"` | 200 | 200 |
| `{}` | 200 | 200 |

Ollama also refuses `5`, `[1]` and `"x"` as argument text. It takes `null`, `{}` and `{"input":"..."}`. No other form was tried.

## The fix

- `argument` in `src/provider/chat/stream.go`: no argument text is stored as `{}`. A JSON object and a JSON string are stored as they came. Anything else, including a number or a list, is stored in a JSON string. This is what `transcript.ToolCall.Input` says an input is, and what the Anthropic client already did.
- `arguments` in `src/provider/chat/request.go`: an object goes back as it is. A string holding an object, which the argument repair ran, goes back as that object, as before. Any other input goes back as `{"input": <the value>}`, the form the Anthropic client uses, so the model still sees what it wrote.

A session stored before the fix with a `""` input now sends `{"input":""}` for it.

## Tests

- `TestACallWithNoArgumentTextIsAnEmptyObject`: the stored input and the next request's body. The `tool_calls` element is the fragment quoted in CLIProxyAPI issue 6516. The chunk around it is written by hand, which `AGENTS.md` does not allow for a recording. No recording could be made: Ollama writes `{}` itself, nothing answered on `localhost:1234`, and the address of the LM Studio server is not in the repo. `src/loop/testdata/factory_write_1.json` shows LM Studio opening a call with `"arguments":""`, followed there by more argument text. Replace the hand-written chunk with a recording when one can be made.
- `TestRequestBodyForAPlainEndpoint`: unfinished argument text goes back wrapped, and an object inside a string goes back as the object.

Both fail against the client before the fix.

## Not done

- `src/provider/openai` stores empty argument text of a function call as `""` and sends it back as it came. Read in the code, not run: no Responses endpoint was at hand that writes a call with no argument text. Not changed here, and that package is another session's.
