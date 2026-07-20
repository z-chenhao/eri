# Agent runtime architecture: Eri, Pi, Hermes, and OpenCode

> Status: research, not a product or technical baseline
> Research date: 2026-07-18; subagent follow-up: 2026-07-19
> Purpose: test Eri's overall architecture, identify accidental complexity before the first public MVP, and distinguish useful reference patterns from designs Eri should not copy.

## 1. Conclusion

Eri's overall direction is sound and should not become a copy of Pi, Hermes, or OpenCode.

Those projects primarily solve how an Agent Harness, coding agent, or multi-channel agent keeps calling tools. Eri must additionally sustain a personal relationship, proactive commitments, external side effects, crash recovery, approval, evaluated delivery, trustworthy memory, and user data sovereignty. Those are product conditions, not decorative layers.

The main risk is accidental implementation complexity:

1. `internal/agent/service.go` has historically concentrated context, loop, checkpoint, budget, Eval, Trace, commit, and failure delivery.
2. Required invariants must not hide behind optional runtime interface probes and silent degradation.
3. Untyped maps, string phases/statuses, and duplicated Trace mirrors make contracts brittle.
4. Evolution and Dataset Snapshot management are complete online subsystems whose inclusion must follow explicit product scope.
5. The Loop needs a small lifecycle-fact boundary so cognitive execution, durability, and observation do not intertwine.

Recommendation: retain the domain-modular monolith, Durable Runtime, Effect Intent, Eval, Memory, and separate Observatory; keep shrinking the Agent kernel. Do not add a Workflow Engine, universal Event Sourcing, ports/adapters framework, or new network services.

## 2. Method and pinned evidence

This comparison uses official repository source pinned to commits:

| Project | Commit | Evidence |
| --- | --- | --- |
| Pi | `3da591ab74ab9ab407e72ed882600b2c851fae21b` | [agent-loop.ts](https://github.com/badlogic/pi-mono/blob/3da591ab74ab9ab407e72ed882600b2c851fae21b/packages/agent/src/agent-loop.ts), [agent.ts](https://github.com/badlogic/pi-mono/blob/3da591ab74ab9ab407e72ed882600b2c851fae21b/packages/agent/src/agent.ts), [durable-harness.md](https://github.com/badlogic/pi-mono/blob/3da591ab74ab9ab407e72ed882600b2c851fae21b/packages/agent/docs/durable-harness.md) |
| Hermes | `1fc6530815ca453d5f2ffd9225ecec35b0d8e93b` | [conversation_loop.py](https://github.com/NousResearch/hermes-agent/blob/1fc6530815ca453d5f2ffd9225ecec35b0d8e93b/agent/conversation_loop.py), [tool_executor.py](https://github.com/NousResearch/hermes-agent/blob/1fc6530815ca453d5f2ffd9225ecec35b0d8e93b/agent/tool_executor.py), [tool_search.py](https://github.com/NousResearch/hermes-agent/blob/1fc6530815ca453d5f2ffd9225ecec35b0d8e93b/tools/tool_search.py) |
| OpenCode | `95fe7b2d74b9f17d5573dfc783d1bf8f9e3f298` | [prompt.ts](https://github.com/anomalyco/opencode/blob/95fe7b2d74b9f17d5573dfc783d1bf8f9e3f298/packages/opencode/src/session/prompt.ts), [processor.ts](https://github.com/anomalyco/opencode/blob/95fe7b2d74b9f17d5573dfc783d1bf8f9e3f298/packages/opencode/src/session/processor.ts), [permission/index.ts](https://github.com/anomalyco/opencode/blob/95fe7b2d74b9f17d5573dfc783d1bf8f9e3f298/packages/opencode/src/permission/index.ts) |

File size illustrates responsibility concentration, not quality. In the pinned snapshots, Pi's low-level loop is 792 lines but its coding session is 3,283; Hermes has core files from roughly 5,700 to 22,000 lines; OpenCode's Session Prompt is 1,631 lines. A simple loop does not imply a simple product.

The 2026-07-19 subagent follow-up also checked current official sources: [Pi's example descriptor](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/examples/extensions/subagent/agents.ts), [OpenCode's Agent descriptor](https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/agent/agent.ts), [Task Tool](https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/tool/task.ts), [permission derivation](https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/agent/subagent-permissions.ts), and [Hermes delegation](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/delegation.md). These branch links are supporting current implementation evidence, not pinned reproducibility claims.

## 3. What each architecture solves

| Dimension | Eri | Pi | Hermes | OpenCode |
| --- | --- | --- | --- | --- |
| Primary product | Long-term personal assistant | Embeddable Agent Harness and coding CLI | General agent across devices | Client/server coding agent |
| Cognitive loop | Native assistant/tool loop | Native assistant/tool loop | Native tool loop with many recovery branches | Session-driven infinite loop |
| Authority state | SQLite, Content Store, Outbox | In-memory context; append-only harness session | SQLite/session logs plus process state | Session Message/Part plus database |
| Side effects | Intent, Policy/Approval, dispatch, Receipt/Reconcile | Tool hooks, no built-in authority system | Guardrail/Approval/Checkpoint per tool | Permission reply plus Tool Part/Snapshot |
| Recovery | Durable Model, Tool, Approval, Eval, Delivery boundaries | Session durable; full harness remains semi-durable | Broad session/gateway recovery with complex paths | Strong session persistence, process coordination rebuilt |
| Context | Persistent and in-run compaction with manifest | `transformContext` plus compaction | Multiple compaction/repair/provider paths | filtering, tool pruning, compaction turn |
| Extension | Built-ins, Skills, out-of-process MCP Plugins | Tools, hooks, coding extensions | large Tool/Skill/MCP/Plugin ecosystem | registry, Plugin, MCP |
| Delivery gate | LLM Judge, Artifact, Outbox, Receipt | model text is result | several hooks, no equivalent delivery transaction | session text is result |
| Long-term memory | Evidence, Claim, Belief, weights | session/Skills | memory, user model, session search | project sessions/instructions |

## 4. Lessons by project

### 4.1 Pi: copy the small kernel, not its safety model

Pi cleanly separates `Model -> Tool Calls -> Tool Results -> next Turn`, emits narrow lifecycle events, exposes context/tool/turn hooks, distinguishes Steering from Follow-up, and can execute sibling Tool Calls while preserving model order. Eri should retain this clarity in its pure Loop driver.

Pi does not replace Eri's Runtime. It deliberately has no built-in file, process, network, or credential permission system. Its durable harness is semi-durable: the Host rebuilds Model/Tool/Extension state, a Provider stream cannot resume, and interrupted tools are retryable only when explicitly idempotent. That is insufficient for bookings, email, or calendar unknown outcomes.

Use Pi's Loop API and lifecycle boundaries. Keep Eri's Effect Intent, Approval, Outbox, Eval, and Memory as first-class domains rather than hooks.

### 4.2 Hermes: product breadth and the cost of uncontrolled complexity

Hermes is the closest product reference: Gateway, channels, Cron, Memory, Skills, session search, subagents, Tool Search, model switching, and long-running operation.

Useful lessons include long-lived channel presence and delivery, lazy Tool disclosure from a current registry snapshot, prompt-prefix caching, `doctor`, and real guardrail/approval/recovery cases.

Do not copy its giant core files, property injection, global registries, lazy imports, provider branching, compatibility burden, or fixed iteration budgets. A pre-release Go project should not inherit historical shims. Eri's Agent Loop intentionally has no fixed model-turn count; budget, cancellation, deadline, device pressure, approval waits, provider errors, and governed no-progress detection are the stopping boundaries.

### 4.3 OpenCode: useful Session/Processor boundaries, excessive framework cost for Eri

OpenCode's Session Prompt coordinates turns while Session Processor reduces provider events into explicit Message and Tool Part states. Permission uses an explicit ask/reply boundary; compaction covers completed turns, oversized turns, and Tool Result pruning; one registry assembles built-ins, Plugins, MCP, and dynamic tools.

These domain boundaries are useful. Its Effect/Layer/Scope/Runner/Bridge graph, coding-worktree concerns, and dual runtime are not appropriate imports into Go Eri. Its pending permissions are mainly process coordination, not equivalent to Eri's durable encrypted approval continuation.

### 4.4 Subagent follow-up: use descriptors, narrow authority, keep Eri's durability

OpenCode provides the clearest named registry pattern: an Agent record has a name, description, mode, model/prompt options, and permission rules; the Task Tool dynamically exposes only currently callable subagent names and descriptions. Its background completion can re-prompt the primary session, but the background job registry is process-local and therefore not an Eri recovery boundary.

Pi Core has no formal subagent registry. Its official example is still useful: each specialist declares name, description, Tool allowlist, model, and system prompt, then runs in a fresh subprocess. The example is foreground and non-durable, so it supplies a descriptor pattern rather than a Runtime design.

Hermes supplies the strongest isolation and authority lessons: fresh Context, inherited denies, leaf mode, reduced Tool sets, depth/concurrency limits, structured results, and asynchronous completion. Its current delegation is primarily one Agent class with global delegation configuration, not a named multi-provider registry.

Eri therefore uses a `Descriptor + Registry + Provider` contract. The Descriptor is the colleague or department's job card; the Registry is the available internal organization; `builtin.delegate` is the only assignment desk. Effective authority is the parent task ceiling intersected with the Descriptor and provider enforcement. Eri keeps its own durable `subagent_runs`, Event Spine completion, encrypted continuation, Eval, and Outbox rather than copying another project's process-local background mechanism.

## 5. Eri's architectural advantages

- **Cognition versus reliability:** the Agent Loop decides what to do; Runtime ensures the work is authorized, recoverable, idempotent, and deliverable.
- **External side effects:** Intent, parameter hash, idempotency key, Receipt, Unknown, and Reconcile model reality better than retrying a Tool function.
- **Delivery is not model stop:** Candidate, Judge, Artifact Version, Outbox, and Channel Receipt separate completion from communication.
- **Provider-independent identity and memory:** weighted Evidence/Claim/Belief, conflict preservation, deletion lineage, and replaceable providers support a real long-term relationship.
- **Healthy Go direction:** one composition root, consumer-side interfaces, domain packages independent of daemon/local API/SQLite, thin command entrypoints, and no boundary-free utility packages.

## 6. Complexity to remove

### 6.1 Concentrated Agent Service

Task claim/resume, context assembly, compaction, model/tool loop, approval continuation, Eval repair/evolution signal, Trace, Artifact, failure, and delivery are separate reasons to change. Split cohesive files within `internal/agent`; do not create generic engines, orchestrators, ports, or adapters. Keep a small Loop driver whose responsibility is Turn transitions.

### 6.2 Required capabilities must be explicit

Model capability discovery, checkpoints, lease renewal, intent reconciliation, observability repositories, and Web surface contracts are production requirements, not optional extension points. Missing requirements should fail at compile or composition time, not return a late 501 or silent default. Only truly optional capabilities, such as a Tool-specific reconciler, stay narrow and optional.

### 6.3 Type internal contracts

Replace authoritative `map[string]any`, bare phase/status strings, and duplicate JSON mirrors with typed structures and enums. Serialize only at Content Store or HTTP boundaries. Because the project has not shipped, reset development data instead of adding dual-read migration shims.

### 6.4 Keep observation outside cognition

A thin synchronous lifecycle Observer can receive governed facts for logs, trace persistence, Observatory, and tests. Durable Checkpoint remains an explicit correctness boundary. The Observer never decides behavior and never receives private Chain of Thought, full prompts, or ungoverned Tool Results.

### 6.5 Product-scoped complexity

Online Evolution/Canary and Dataset Candidate/Snapshot management are expensive but currently explicit MVP choices. Do not delete them under the label of refactoring. If the user narrows product scope, remove each complete vertical slice—schema, wiring, API, UI, tests, and documentation—without disabled shells or compatibility layers.

### 6.6 Version names that are not over-versioning

Keep `/api/v1`, the current SQLite `user_version`, `api/plugin/v1`, and tolerant parsing of external Agent Skills ecosystems. These are wire/schema versions and interoperability boundaries. Remove obsolete-field dual reads and temporary fallbacks, not every occurrence of `v1`.

## 7. Priority

### Before first GitHub push

1. Retain the top-level architecture and explicit MVP product scope.
2. Keep required production capabilities compile-time explicit.
3. Split Agent Service by cohesive behavior inside its package.
4. Type durable internal structures without pre-release migration baggage.
5. Emit a thin governed lifecycle fact stream.
6. Unit-test the pure Loop: given Model Response, Tool Outcome, cancellation, budget, or approval state, assert whether it continues, pauses, evaluates, or terminates.

### After real MVP pressure

- Add Hermes-style dynamic Tool disclosure only when schemas materially consume Context; rebuild the catalog from the current Registry each time.
- Add Pi-style Steering and Follow-up only after messages persist before turn-boundary consumption.
- Feed Presence and canvas from lifecycle facts without streaming unapproved candidate text.
- Isolate provider request normalization and context transformation from the Loop.
- Add session branching/replay only when real tasks justify it.

### Explicit non-goals

- Fixed Workflow Engine or global Event Sourcing.
- OpenCode-style Effect/Layer dependency framework.
- Skill Runtime or Skill service.
- Fixed model-turn limit as a substitute for budgets and safe stop conditions.
- Provider/platform compatibility branches in the Agent core.
- Pre-release deprecated APIs, migration chains, shims, or obsolete field dual reads.
- Extension hooks that bypass Policy, Intent, Approval, Eval, or Delivery.

## 8. Target shape

```text
Durable Runtime / Agent Service
  ├─ assemble typed turn context
  ├─ persist checkpoint
  ├─ call small Agent Loop driver
  │    ├─ provider-native Model message
  │    ├─ Tool request / governed observation
  │    └─ lifecycle facts
  ├─ enforce Policy / Approval / Effect Intent
  ├─ evaluate candidate
  └─ atomically commit Artifact + Delivery Outbox

Lifecycle facts ──> logs / trace persistence / Observatory / tests
```

There is no new network service, Workflow DSL, or general framework. Complexity remains only where Eri has real product invariants.

## 9. Final judgment

Eri is more complex than Pi if the question is only whether a model can call tools until it emits text. Eri's boundaries are stronger when the question is whether a long-term personal assistant remains trustworthy through crashes, approval, side effects, delivery failure, and conflicting memory.

The correct move is deletion-first contraction, not an architecture rewrite: remove unconfirmed scope, optional-capability shims, dynamic internal contracts, and giant coordination files while preserving Eri's actual product advantages.
