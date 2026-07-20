# Local Codex External Agent integration

> Status: implementation research, not a separate architecture source of truth
> Research date: 2026-07-19
> Technical contract: [MVP Technical Design section 11.4](../mvp-technical.md#114-subagent-delegation)

## Decision

Eri integrates the user's authenticated local Codex installation as one background Provider for the stable `engineering_team` role. Codex is an implementation choice, not the job description exposed to Eri. Another installation may bind that role to Claude Code, Pi Agent, or another compatible Provider without changing `builtin.delegate`. The MVP Codex adapter uses non-interactive mode over a supervised local process:

```text
codex --ask-for-approval never exec
  --ephemeral
  --ignore-user-config
  --skip-git-repo-check
  --json
  --sandbox read-only|workspace-write
  --output-schema <temporary-schema>
  --cd <configured-workspace>
  -
```

The prompt travels on stdin, not argv. Eri consumes the JSONL stream in memory, accepts only the bounded structured final result, discards stderr and raw event detail, removes temporary job artifacts, and never reads or copies Codex credentials.

## Official surface comparison

| Surface | Official capability | Eri decision |
| --- | --- | --- |
| [Non-interactive mode](https://developers.openai.com/codex/noninteractive) | `codex exec`, JSONL events, output schemas, sandbox selection, working-directory control | Use now. It fits a Go daemon, has no new runtime dependency, and can be supervised behind Eri's durable Intent and Event Spine. |
| [Codex SDK](https://developers.openai.com/codex/sdk) | Server-side TypeScript control of Codex threads and structured output | Do not add for the MVP. It would add a Node runtime solely to wrap the same provider while Eri already owns durable orchestration in Go. Reconsider only if SDK-only thread features become a real requirement. |
| [Codex App Server](https://developers.openai.com/codex/app-server) | JSON-RPC integration with authentication, history, approvals, and streamed thread events | Keep as the future deep-integration option. It is broader than the current bounded background-job contract and would require a larger lifecycle adapter. |
| [Codex hooks](https://developers.openai.com/codex/config-advanced/#hooks) | Lifecycle hooks around Codex operation | Do not use as the completion transport. Hooks are not Eri's durable queue, continuation ownership, idempotency, or delivery receipt. |

The completion event is therefore an Eri Runtime fact, not a Codex hook: `subagent.queued` commits with the Effect Intent; the provider later commits `subagent.completed|failed|unknown|canceled`; `subagent.resume` claims the encrypted continuation only after the progress Delivery is sent.

## Capability and authority boundary

The model-facing Engineering Team role covers project, code, and data investigation, analysis, implementation, debugging, and verification. The Runtime-only Codex Provider Descriptor declares workspace analysis, reversible implementation, verification, external-data behavior, process recovery, and authority boundaries.

- `read_only`: explicit Codex read-only sandbox; Eri records external model disclosure and may notify after dispatch.
- `workspace_write`: explicit workspace-write sandbox; Eri treats it as reversible and overwrite-capable, so ordinary confirmation is required before dispatch.
- Never granted: direct user contact, approval, Eri Memory/Soul writes, recursive delegation, external communication, credentials, destructive operations, git commit/push, deployment, or authority outside the configured workspace.

Codex output is untrusted evidence with the common model-visible Result shape: `delegation_id`, `assignee`, `status`, `summary`, `evidence`, `changes`, `tests`, `remaining_risks`, and `error_code`. Durable Run and Event records also contain `role_id=engineering_team` and `provider_id=codex` for audit and recovery. Primary Eri reviews the result through the normal Agent Loop and Eval before any user-facing follow-up.

## Recovery rule

Eri never replays a possibly mutating Codex process after an ambiguous interruption. A durable running process handle is inspected and the known orphan is stopped; the result becomes `unknown`. A run that reached `starting` but has no trustworthy runtime handle also becomes `unknown` rather than launching again. The final answer must distinguish confirmed work from effects that require inspection.
