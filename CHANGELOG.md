# Changelog

## [0.7.0](https://github.com/gfx-labs/towa/compare/v0.6.0...v0.7.0) (2026-11-17)


### Features

* **anthropic:** send cc_entrypoint=towa in the billing block ([ce4eacf](https://github.com/gfx-labs/towa/commit/ce4eacf5a9c615b0e2ce1cefd729aae8448066ca))


### Bug fixes

* **auth:** offer only logins a model uses, and report a missing API key as a request error ([717acb1](https://github.com/gfx-labs/towa/commit/717acb13e7a482caea83bd358960ba087c4ad326))


### Performance

* **agentctx:** intern context file text and system prompt blocks ([925a6ee](https://github.com/gfx-labs/towa/commit/925a6eebd9824207c5235b0f892797672375b3ee))
* **anthropic:** sign the billing token in place without copying the body ([56b0616](https://github.com/gfx-labs/towa/commit/56b0616ac034a320fcd8af428bd163e473338b01))
* **loop:** build a request's conversation in one pass over its own list ([ac2a1ba](https://github.com/gfx-labs/towa/commit/ac2a1ba012d30083163e072cd41319d2e9fa59a0))
* **shell:** pool the 64 KB read buffer of a command's output ([7b75230](https://github.com/gfx-labs/towa/commit/7b75230f6d418e5c846f9de8d2acab53f9e27ff2))

## [0.6.0](https://github.com/gfx-labs/towa/compare/v0.5.1...v0.6.0) (2026-11-16)


### Features

* **auth:** offer a login per provider on a fresh install and answer authenticate ([#15](https://github.com/gfx-labs/towa/issues/15)) ([ba59953](https://github.com/gfx-labs/towa/commit/ba59953abd1d33b20a64d22cde4e37343b633c2a))
* **tools:** name the file or command in tool call titles ([0cc3f67](https://github.com/gfx-labs/towa/commit/0cc3f674184651dc982fd5bfa0402baee84bea7c))

## [0.5.1](https://github.com/gfx-labs/towa/compare/v0.5.0...v0.5.1) (2026-11-15)


### Bug fixes

* **prompt:** load a skill only when the task needs it, and not again while it is in the conversation ([3be9af5](https://github.com/gfx-labs/towa/commit/3be9af5720dfba489d6dd16e8ef2950055a83206))

## [0.5.0](https://github.com/gfx-labs/towa/compare/v0.4.0...v0.5.0) (2026-11-15)


### Features

* **acp:** name the model in usage updates ([#8](https://github.com/gfx-labs/towa/issues/8)) ([61bab58](https://github.com/gfx-labs/towa/commit/61bab58116e11f7a38436fae924154c97d476a5d))


### Bug fixes

* **batch:** accept a sub-call tool named in the functions namespace ([ab95343](https://github.com/gfx-labs/towa/commit/ab95343763b59ef8b60c36b706140ea732d1042b)), closes [#11](https://github.com/gfx-labs/towa/issues/11)
* **loop:** report a job started inside a batch under the batch's tool call ([502d2cf](https://github.com/gfx-labs/towa/commit/502d2cf514d1cdd1fa70413f420cb0bf0b206a53)), closes [#13](https://github.com/gfx-labs/towa/issues/13)
* **tools:** do not count trimmed trailing whitespace as output left out ([cccadc9](https://github.com/gfx-labs/towa/commit/cccadc9fae44c69096bcfc18e0c7cbaa4d7d6fa1)), closes [#12](https://github.com/gfx-labs/towa/issues/12)

## [0.4.0](https://github.com/gfx-labs/towa/compare/v0.3.1...v0.4.0) (2026-11-15)


### Features

* **mcp:** serve a client-provided MCP server over the ACP connection ([2b391a0](https://github.com/gfx-labs/towa/commit/2b391a06170decbda54fc01c1a305f7e6daf4c51))

## [0.3.1](https://github.com/gfx-labs/towa/compare/v0.3.0...v0.3.1) (2026-11-14)


### Bug fixes

* **loop:** send a request again when the provider cuts its response off mid-stream ([6949c9e](https://github.com/gfx-labs/towa/commit/6949c9e2d38dc85d20e5eeb9222993f6e89c21db))
* **loop:** send the session's cumulative cost in usage_update ([d0c2c16](https://github.com/gfx-labs/towa/commit/d0c2c16d8f531d2fa2709f0040356cf38e4a287a))
* **tools:** tell the model to wait for a background job with bash_output block ([708a6c4](https://github.com/gfx-labs/towa/commit/708a6c4d318fc3bc68ab25e10dadeddde3d60801))

## [0.3.0](https://github.com/gfx-labs/towa/compare/v0.2.0...v0.3.0) (2026-11-14)


### Features

* **tools:** run bash commands in the background, and keep the turn open until they end ([f6fd95e](https://github.com/gfx-labs/towa/commit/f6fd95e3aa317e2e4e3068cacf09739d5c588de3))
* **tools:** take one edit as old_string and new_string beside edits ([5bba92c](https://github.com/gfx-labs/towa/commit/5bba92c7eee5777850e51f69819936b3194a7ce6))


### Bug fixes

* **tools:** tell the model the configured bash and read limits ([b092789](https://github.com/gfx-labs/towa/commit/b092789cab38c0a257d56b014221a7f91d6b9c68))


### Documentation

* delete the in-repo docs directory ([c0f03ea](https://github.com/gfx-labs/towa/commit/c0f03ea3b55f6c1b8c6e83c7e8feaaa196ce62eb))

## [0.2.0](https://github.com/gfx-labs/towa/compare/v0.1.2...v0.2.0) (2026-11-14)


### Features

* **acp:** embedded context, retry notices, MCP elicitation, terminal auth ([f5a9ae9](https://github.com/gfx-labs/towa/commit/f5a9ae97ebfe19155495237b8e3cd895c9b39369))
* **anthropic:** sign OAuth requests with Claude Code's cch ([c97012c](https://github.com/gfx-labs/towa/commit/c97012c4bb9fbde08f09ea4d2e87d624f83d5106))
* **models:** select a model without its account and expand provider:*/model to every account ([cda21d9](https://github.com/gfx-labs/towa/commit/cda21d991c4e918ade157d7040c8020fc5a47515))
* **session:** reasoning_effort config option ([bf49cc2](https://github.com/gfx-labs/towa/commit/bf49cc20d0df130e66917c6fd304906c27eb48aa))

## [0.1.2](https://github.com/gfx-labs/towa/compare/v0.1.1...v0.1.2) (2026-11-13)


### chore

* release 0.1.2 ([fac42d7](https://github.com/gfx-labs/towa/commit/fac42d7e191772fa68174d4804e175bfd69ec5eb))

## [0.1.1](https://github.com/gfx-labs/towa/compare/v0.1.0...v0.1.1) (2026-11-13)


### Documentation

* conversation search over full history, argument repair, prompt sections, entry ids ([d177d5c](https://github.com/gfx-labs/towa/commit/d177d5c39a73f7f9ab31ac68a6b85cbdf98a4a9f))
* embeddings sidecar, jev decisions, built-in web search, jcode-style oauth identity ([6c824e7](https://github.com/gfx-labs/towa/commit/6c824e75958ca1e815cff033292d50c23f146ed7))
* initial design from Q&A rounds ([4966782](https://github.com/gfx-labs/towa/commit/496678201028acbcff7ef76fe3144026a54bc432))
* install from the public repository ([c9679bf](https://github.com/gfx-labs/towa/commit/c9679bfb18473e8c9cd480b7ca69b844a53bd777))
* mark the per-process memory figure as unmeasured, fix the test repeat count ([d166475](https://github.com/gfx-labs/towa/commit/d166475c776da2ac26cd752393b2ef0a72bd9045))
* own ACP layer generated from the pinned schema, Pi ideas under consideration ([d98ca09](https://github.com/gfx-labs/towa/commit/d98ca093ec3003f7bfe6a6af38aa5b68e2eb9c7e))
* recommend TEI as embeddings sidecar ([c46b726](https://github.com/gfx-labs/towa/commit/c46b7260ab93717820e2c79de570813af9a96df4))
* record core interface decisions from round 6 ([c2d4b02](https://github.com/gfx-labs/towa/commit/c2d4b02577513e37b2b636bbe44532b722645715))
* record Go conventions and CLI configuration integration ([e5670c7](https://github.com/gfx-labs/towa/commit/e5670c790c1c518a966d9624f73837d7408fa598))
* record testing, workflow and upstream pin decisions from round 7 ([f7c0c75](https://github.com/gfx-labs/towa/commit/f7c0c7514b65ac28253e4ed2782bf355b39101e0))
* use the ACP methods acp-go-sdk v0.13.5 defines for model switching and usage ([1f6e3ea](https://github.com/gfx-labs/towa/commit/1f6e3eaec1ad675f309006205dd2151d5129f54b))
