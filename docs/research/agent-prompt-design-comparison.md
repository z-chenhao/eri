# Agent prompt design: Pi, OpenCode, Hermes, and Eri

> Status: research, not a product or technical baseline
> Research date: 2026-07-20
> Baseline reconciliation: updated 2026-07-22 after the Context/Memory protocol refactor; measurements below describe that updated tree.
> Purpose: identify which instructions truly belong in Eri's primary prompt, remove duplicated ownership, and preserve the confirmed Soul, local-first, authority, Eval, and Delivery boundaries.

## 1. Conclusion

There is no evidence that a generally capable Agent needs one large universal prompt. The strongest common pattern is purpose separation:

- a small stable identity and reasoning contract;
- only currently available capability guidance;
- Tool descriptions and Schemas for Tool-specific behavior;
- progressively disclosed Skills and project rules;
- Runtime enforcement for authority, persistence, recovery, and side effects;
- separate prompts for generation, Eval, compaction, and delegation;
- volatile facts added after a cacheable stable prefix.

Pi expresses this most simply. OpenCode's mature V1 shows the cost of provider-specific prompt accumulation, while its new Core/V2 makes the intended Runtime boundary unusually explicit. Hermes has the best documented stable/context/volatile assembly, but also demonstrates how compatibility guidance, Memory rules, Skills, and Tool instructions can become prompt debt when several layers own the same rule.

Eri should not copy the shortest prompt. Its personal relationship, local-first privacy, durable side effects, evaluated delivery, and user-owned Memory are real product semantics. It should express each semantic rule once at its proper owner.

## 2. Method and pinned evidence

Only official repositories and documentation were used. Branches move, so the implementation evidence is pinned:

| Project | Snapshot | Primary evidence |
| --- | --- | --- |
| Pi | `1942b2600f2bbb7b5bc86c30379e14c0766c058c` | [system prompt builder](https://github.com/earendil-works/pi/blob/1942b2600f2bbb7b5bc86c30379e14c0766c058c/packages/coding-agent/src/core/system-prompt.ts), [Agent Loop](https://github.com/earendil-works/pi/blob/1942b2600f2bbb7b5bc86c30379e14c0766c058c/packages/agent/src/agent-loop.ts), [Skills](https://github.com/earendil-works/pi/blob/1942b2600f2bbb7b5bc86c30379e14c0766c058c/packages/coding-agent/src/core/skills.ts), [compaction](https://github.com/earendil-works/pi/blob/1942b2600f2bbb7b5bc86c30379e14c0766c058c/packages/coding-agent/src/core/compaction/compaction.ts) |
| OpenCode | `b67fda133a186c7c294c8822f7eda89f36d57aff` | [V1 provider prompt routing](https://github.com/anomalyco/opencode/blob/b67fda133a186c7c294c8822f7eda89f36d57aff/packages/opencode/src/session/system.ts#L27-L42), [V2 Agent prompts](https://github.com/anomalyco/opencode/blob/b67fda133a186c7c294c8822f7eda89f36d57aff/packages/core/src/plugin/agent.ts#L11-L204), [typed System Context](https://github.com/anomalyco/opencode/blob/b67fda133a186c7c294c8822f7eda89f36d57aff/packages/core/src/system-context/index.ts#L5-L39), [V2 request assembly](https://github.com/anomalyco/opencode/blob/b67fda133a186c7c294c8822f7eda89f36d57aff/packages/core/src/session/runner/llm.ts#L168-L215) |
| Hermes | `3d7e1c5f4353b358f0d9c159c339c0b67dde4e0d` | [three-tier system assembly](https://github.com/NousResearch/hermes-agent/blob/3d7e1c5f4353b358f0d9c159c339c0b67dde4e0d/agent/system_prompt.py#L147-L240), [Prompt Assembly guide](https://github.com/NousResearch/hermes-agent/blob/3d7e1c5f4353b358f0d9c159c339c0b67dde4e0d/website/docs/developer-guide/prompt-assembly.md#L7-L38), [Skills disclosure](https://github.com/NousResearch/hermes-agent/blob/3d7e1c5f4353b358f0d9c159c339c0b67dde4e0d/tools/skills_tool.py#L1-L12), [Tool Search](https://github.com/NousResearch/hermes-agent/blob/3d7e1c5f4353b358f0d9c159c339c0b67dde4e0d/tools/tool_search.py#L426-L583) |

Word counts below are whitespace-delimited source measurements, not provider tokenizer counts. They are useful only as relative prompt-debt signals.

## 3. Lessons from the reference Agents

### 3.1 Pi: conditional capability guidance

Pi's default prompt is a small coding identity, a one-line index for active Tools, a deduplicated set of guidelines, project context, a compact Skill index, and the working directory. A Tool contributes its prompt snippet or guideline only while that Tool is active. Its complete description and JSON Schema travel through the provider's native Tools field rather than being repeated in prose.

Skill metadata is visible up front, while full instructions are read only after a match. Conversation length is controlled by Runtime compaction with a fixed checkpoint shape and safe Tool-call boundaries, not by adding more reminders to the primary prompt.

The useful principle is conditional ownership. Eri should not copy Pi's weaker permission model, unbounded ancestor context files, persistence of assistant thinking during compaction, or example subagents that append a role to the full parent prompt.

### 3.2 OpenCode: learn from V2, not V1 prompt size

OpenCode V1 currently selects one of several large templates by model ID. The active templates are roughly 1.2K to 2.2K words before environment, project rules, Skills, MCP instructions, Tool Schemas, and history. Some templates change planning, parallelism, comments, delegation, and communication style, so switching providers can change product behavior rather than only protocol adaptation.

The more important direction is Core/V2. Its default Build Agent system text is one sentence. Environment, date, project instructions, Skills, and references are typed `SystemContext.Source` records with stable keys, codecs, baselines, updates, and removal semantics. The runner materializes only permitted Tools; when Tools must stop, it removes them and sets `toolChoice: none` instead of relying on another warning. Explore, compaction, title, and summary use separate narrow prompts.

V2 is integrated but not yet feature-equivalent with V1; [its built-in Tool list still marks migrations pending](https://github.com/anomalyco/opencode/blob/b67fda133a186c7c294c8822f7eda89f36d57aff/packages/core/src/tool/builtins.ts#L18-L29). Eri should adopt the ownership model, not the 28-word prompt or unfinished feature surface.

### 3.3 Hermes: strong assembly, visible prompt debt

Hermes explicitly assembles `stable -> context -> volatile`:

- stable: identity, model/Tool guidance, Skills, and platform hints;
- context: caller system content and one selected project rule file;
- volatile: Memory, user profile, external recall, time, session, model, and provider facts.

Turn-only recall is injected ephemerally so it does not rewrite the cached system prefix. Memory snapshots are bounded and frozen for a session. Skills use an index, full document, then referenced-resource progression. Large plugin/MCP surfaces can collapse behind search, describe, and call bridge Tools.

The same repository also shows the failure mode. Model-family guidance, coding posture, Memory behavior, Skill mandates, and Tool enforcement repeat rules that also live in Schemas or Runtime. With dozens of Skills and Tools, the supposedly stable layer becomes several thousand tokens before user context. Eri should retain the assembly and progressive disclosure while rejecting the defensive prose accumulation.

## 4. Audit of Eri before this change

The product and architecture were sound, but prompt ownership had drifted:

1. The immutable Soul correctly owned identity and stable relationship values.
2. The primary system prompt also contained detailed procedures for Memory, feedback, all-user-data deletion, Plugin installation, delegation, failure recovery, progress, and delivery.
3. Those same capability rules already existed in live Tool descriptions, Schemas, Policy, Gateway, Runtime, or Judge checks. The base prompt therefore described unavailable Tools and paid for their instructions on unrelated turns.
4. The interpersonal response profile repeated parts of Soul, whole-conversation interpretation, clarification, agency, evidence, and delivery behavior in a long negative-rule list.
5. The Judge prepended the entire generation system prompt to its own rubric. It therefore inherited instructions to use Tools while simultaneously being told not to call Tools, saw the guarded evolution instruction it was meant to judge independently, and paid again for the Skill catalog and generation-only response rules.
6. Tests asserted many exact prose fragments. This protected earlier regressions, but also rewarded retaining sentences rather than protecting rule ownership and behavior.

The central defect was not merely length. Generation, capability routing, Runtime invariants, and Eval were sharing one instruction surface.

## 5. Applied Eri design

### Stable primary kernel

The immutable Soul is unchanged. The remaining stable generation prompt now owns only:

- native Tool Calling affordance and evidence-versus-authority distinction;
- the model's inability to self-authorize or claim unconfirmed effects;
- minimum external disclosure and secret handling;
- whole-conversation interpretation and one-smallest-question behavior;
- freshness, safe recovery, and meaningful progress semantics;
- the compact Soul-guided visible response profile.

With the default Soul, this stable prompt is 870 whitespace-delimited words and 6,085 UTF-8 bytes. The test ceiling is 900 words to catch renewed accretion without pretending word count proves quality.

### Capability-local instructions

Memory, whole-user-data erasure, and Plugin operating rules moved into their active Tool descriptions. Feedback and delegation were already substantially self-describing. Runtime remains the actual authority for approval, effect intent, idempotency, recovery, and Receipt state. The base prompt contains no concrete `builtin.*` Tool ID.

### Purpose-specific Eval

The Judge no longer receives the generation system prompt. It receives:

- a final-answer or progress-specific rubric;
- the exact transcript;
- activated Skill and confirmed Tool metadata;
- a bounded Candidate Evaluation Context with the Run's Soul, relevant Memory evidence, time, timezone, and Channel.

It does not receive Agent operating rules, the general Skill catalog, capability instructions, or the guarded evolution candidate. The final-answer rubric plus interpersonal rubric is 530 words before the bounded evaluation context, compared with the former Judge path that inherited the entire roughly 1.5K-word generation prompt before adding its own long rubric.

### Cache order and test contract

Prompt assembly now puts the stable kernel and Skill catalog before versioned Experience and small Runtime facts. Selected Memory is not concatenated into that System string: it is a separate turn-scoped System message placed immediately before the authoritative source Interaction. This preserves a byte-stable reusable prefix while keeping volatile evidence adjacent to what caused the Run. Tests assert layer order, Eval separation, absence of capability IDs from the kernel, compactness, complete Tool evidence for progress Eval, sanitized checkpoint recovery, and the essential semantic boundaries instead of preserving every historical sentence.

## 6. What remains deliberately unchanged

- Soul and product meaning.
- Native `assistant(tool_calls) -> tool(result)` cognition.
- Policy, Approval, Effect Intent, idempotency, checkpoints, Outbox, Eval, Delivery, and Receipt ownership.
- Full Skill instructions only after activation.
- Context compaction at complete Tool-frame boundaries. Provider-required native Tool `reasoning_content` is retained only in encrypted provider transcripts/checkpoints for exact replay; safe Trace, logs, Memory, Episodes, datasets, and Observatory projections omit it. A governed Memory mutation removes the exposed suffix as whole frames rather than rewriting native reasoning.
- One primary Eri and restricted subagent contexts.

Eri should not add provider-specific personality prompts. A model-specific overlay is justified only by reproducible Eval evidence and must remain small, versioned, and unable to change Soul or authority.

## 7. Follow-up evidence required

Static structure and unit tests can prove separation, not response quality. A real-model baseline-versus-tuned comparison still needs to measure:

- task completion and false-success rate;
- unnecessary Tool calls and clarification quality;
- Memory correctness and local-first disclosure;
- interpersonal stability without verbosity;
- input tokens and prompt-cache hits;
- Judge agreement, repair rate, and contamination by candidate instructions.
- exact Soul snapshot/version migration before any future Soul revision, so a much older in-flight checkpoint never evaluates against a newer Soul.

If the Skill or Tool catalog grows materially, add relevance selection and an absolute disclosure budget. Eri currently has a small catalog, so introducing a new router now would add more complexity than it removes.
