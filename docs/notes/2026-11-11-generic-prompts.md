# Generic instructions

The generic dialect uses instructions authored for acpharness, not an upstream
capture or a guessed model-family template. This follows the generic neutral
dialect in `docs/design.md:205-211` and the Chat Completions first-task milestone.

`prompts.Lookup` accepts any nonempty exact wire model for generic. Model
existence, family and context/output metadata remain the responsibility of
`models.Select` before runtime request preparation. Lookup does not rename the
model, infer aliases, or grant a missing Codex/Claude capture a generic fallback.

The authored system text describes coding work and honest verification. The tools
section directs use of the supplied definitions and checking uncertain outcomes.
The interruption message is explicitly a user-role message warning about partial
operations. ACP instruction append/override remains unchanged. No new config.

Coverage extends the existing lookup contract with an empty generic model error
and non-catalog wire names. Those names are composed inputs. No test copies the
authored prose, and no provider response or recording was created for this change.
