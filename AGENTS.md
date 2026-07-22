# Eri Agent Guide

This is the repository-wide collaboration contract for coding agents. Keep it short, stable, and executable. Do not accumulate one-off task notes or temporary preferences here.

## Mission

Eri is a strictly local-first personal Agent Assistant that belongs entirely to its user. A long-running Go daemon maintains one continuous relationship through Web, CLI, and future channels.

Eri exists to understand, prepare, execute, evaluate, follow up, and deliver like a reliable human assistant. It does not exist to showcase agent technology.

## Sources of truth

- Product meaning, scope, and experience: [docs/mvp-product.md](docs/mvp-product.md)
- Architecture, invariants, and engineering boundaries: [docs/mvp-technical.md](docs/mvp-technical.md)
- A current, explicit user instruction overrides repository documentation. Update the relevant source of truth in the same change when the baseline changes.
- Never restore an obsolete design from old chat, deleted documents, competitor code, or personal guesses.

Read only the task-relevant sections before working. The product document explains why and what the experience is; the technical document explains how the system must remain correct.

## Collaboration

1. Confirm the goal, context, constraints, and completion evidence.
2. Confirm changes to product meaning, Soul, architecture, security boundaries, irreversible behavior, or external side effects with the user first.
3. Inside a confirmed boundary, make the smallest reasonable assumptions and state them. Do not block on low-risk details.
4. Plan complex or ambiguous work; execute simple work directly.
5. Deliver the smallest real end-to-end slice. Do not pre-create a future package tree or placeholder abstractions.
6. Run relevant checks, inspect the complete diff, and report evidence.

### Minimal change and no overdesign

- Make the smallest cohesive change that fully satisfies the confirmed requirement. Preserve existing contracts and do not mix opportunistic cleanup, unrelated refactors, or speculative generalization into the task.
- Do not add abstractions, packages, configuration, compatibility layers, extension points, or state for hypothetical future needs. Reuse the current owner and data flow; introduce a new boundary only when the present end-to-end behavior requires it and tests can prove it.

For Agent behavior, first make Context Assembly carry the smallest authoritative context that is sufficient for the decision. Keep the stable Prompt short, general, and explicit about durable capabilities and evidence boundaries. Do not accumulate transcript-specific `case` branches, keyword rules, or examples when one causal invariant, context boundary, or plain instruction solves the class. Prefer the simplest complete design: fewer states, less context, and less latency are part of correctness.

AI-native development does not remove requirements, design constraints, or verification. High autonomy must remain reviewable.

## Architecture guardrails

- One `eri` binary runs the daemon and acts as the CLI client.
- The cognitive core is an LLM-driven Agent Loop. The durable Runtime owns recovery, scheduling, idempotency, and side effects; do not build a fixed Workflow Engine.
- A Skill is an on-demand resource package, not a Runtime, service, or Agent.
- Use a domain-modular Go monolith, a durable Event Spine, and out-of-process Plugins.
- Keep `cmd/eri` thin. Compose only at the composition root. Define interfaces near consumers and return concrete implementations.
- Do not create boundary-free `core`, `common`, `utils`, `types`, `ports`, or `adapters` packages.
- Add packages only when a real vertical slice requires them.
- Conversation Web, CLI, and future Channels share one authoritative conversation. The user always interacts with the primary Eri.
- Conversation Web must not become a Settings, Task, Workflow, Memory, or Artifact administration console.

## Architecture diagram maintenance

README embeds [`docs/assets/eri-architecture-handdrawn.svg`](docs/assets/eri-architecture-handdrawn.svg); the PNG beside it is a fixed-size export. The diagram projects the "Overall architecture" section of `docs/mvp-technical.md` and is not a separate source of truth.

- Update the technical document first, then update SVG and PNG in the same change. Never edit only README copy or the PNG.
- Preserve a `2400 x 1350` white canvas and light regular handwritten style. Use `Comic Sans MS` with `Klee`, `Noteworthy`, and generic handwriting fallbacks. Only titles and section names are bold. Recommended sizes are `48px`, `28px`, `22px`, and at least `18px` for title, section, node, and body text.
- Apply hand-drawn distortion only to boxes and section boundaries, capped near `0.75px`. Never distort text or arrows. Red means the local trust boundary; orange means Runtime and Agent Loop; green means Personal Context; pink means Capability and Safety; blue means Evidence and Observatory; gray means Infrastructure and external dependencies.
- Keep one left-to-right main execution chain, one local Agent Loop cycle, four support cards below it, and out-of-process exits on the right. Main flow is solid black, composition and external dependencies are dashed gray, and evidence edges are blue. Route edges through whitespace; never cross nodes, headings, or body text. Each support card gets at most one aggregate connection.
- Keep short causal edges straight. Use gentle curves only for genuine loops and feedback. Route long cross-boundary protocols through dedicated whitespace rails with long straight runs and rounded corners; never turn every edge into a Bezier curve for style. Every causal edge keeps a single arrowhead. A genuinely bidirectional protocol may use one double-headed line; never use a double-headed line to collapse distinct causal paths such as Tool Call and Receipt.
- The main chain must show `User surfaces <-> Gateway -> Durable Runtime -> Resumable Agent Run -> Delivery / Outbox / Receipt -> Gateway`. Inside Run show `Context Assembly -> LLM-driven Agent Loop -> Eval`; inside Loop show only `Model -> Tool Call -> Governed Observation -> Model`. Support cards are Replaceable Infrastructure, Personal Context, Capability & Safety, and Evidence & Improvement. Out-of-process boundaries include Model Providers, Plugins / MCP, and System Observatory.
- The provider path must show `Model <-> Model Gateway / Adapters <-> Model Providers`. Keep the gateway inside the daemon and providers outside it. Never draw the Agent Loop directly to a provider or leave Model Adapters as a disconnected support-card decoration.
- Replaceable Infrastructure must distinguish `SQLite State + Events` from `Rotating Process Logs`. Safe Event Spine envelopes may be durable in SQLite; daemon, Broker, and bootstrap logs are separate redacted bounded files under `EriDataRoot/logs`. Encrypted Content remains a separate governed store.
- Show only confirmed technical boundaries and causal relations. Never imply private Chain of Thought, direct Model-to-delivery bypass, or a Plugin, Observatory, or second Agent that bypasses the primary Loop.
- Validate with `xmllint --noout docs/assets/eri-architecture-handdrawn.svg`, export the PNG at `2400 x 1350`, inspect the complete image, and verify the SVG renders in README.

## Safety and data invariants

- Strict local-first: external services receive only the minimum task data.
- Never persist passwords, tokens, cookies, API secrets, or session grants, and never place them in Memory, Episodes, datasets, or ordinary logs. Explicit local `ERI_DEBUG_LOG=1` may reproduce values already present in provider bodies; it never records authorization headers and makes the whole process log sensitive.
- A Model may return candidate text or request native Tool Calls. It cannot grant authority, write databases directly, or bypass Policy.
- Persist an intent and idempotency key before any external side effect. Dangerous actions require strong approval.
- Every outward delivery passes Eval and is sent and reconciled through the Outbox.
- Preserve provider-required continuation state such as `reasoning_content` with its assistant Message inside encrypted Agent checkpoints for exact replay and inside the encrypted user-owned Run record for retention, export, and deletion. Safe Trace projections must omit it; never promote it into Delivery, Observatory, Memory, Episodes, datasets, or evolution. The sole log exception is explicit local developer mode `ERI_DEBUG_LOG=1`, which records raw provider request and response bodies in the bounded process log for diagnosis and must never be enabled in shareable or production-like runs.
- Memory, Eval, Episode, evolution, and observability changes must preserve source, version, deletion lineage, and privacy boundaries.

## Engineering

- Go is the primary backend language. Follow standard-library conventions, `gofmt`, and consumer-side interfaces.
- Explain every new dependency's necessity, maintenance status, license, and local-first impact.
- Eri-owned code and documentation use Apache-2.0. Preserve the source and license of third-party code, models, data, and assets.
- Fix root causes. Avoid compatibility shims, silent fallbacks, and false success.
- Test behavior near its owner: unit tests for domains, integration tests across boundaries, and reproducible eval fixtures for critical Agent Loop behavior.
- Redact logs and errors by default. Explicit `ERI_DEBUG_LOG=1` provider diagnostics are intentionally raw and local-only; fixtures must not contain real credentials or personal data.
- Repository-owned source, documentation, tests, and UI copy are English. Eri's runtime output follows the user's language.

Stable commands:

```bash
make fmt
make test
make vet
make check
```

`make check` is the minimum pre-commit gate. A missing `go.mod` or Go source must produce an explicit skip, not a fake pass.

## Change discipline

- Keep changes small and cohesive. Do not fix unrelated issues opportunistically.
- Preserve user changes and never perform destructive Git operations.
- Follow [docs/development/git-conventions.md](docs/development/git-conventions.md).
- Do not create or switch branches, commit, push, or open a PR without explicit user authorization. Never commit directly to `main` unless explicitly requested.
- Stage only confirmed task paths or hunks, review the staged diff, and write an accurate Conventional Commit. Never use `git add .` or mix unrelated work.
- Never force-push, rewrite another contributor's published history, bypass checks, or commit credentials, local environment files, personal data, or unchecked generated output.
- CI/CD, `CODEOWNERS`, release permissions, and Secrets are security boundaries. Confirm authorization, require a human Code Owner review, and never give PR code Secrets, write permissions, `pull_request_target`, `workflow_run`, or self-hosted runners.
- Update tests and the unique source of truth whenever behavior, protocol, data, or boundaries change.
- Add nested `AGENTS.md` files only when a subtree truly has different commands or rules.
- Add Skills, hooks, or `.codex/config.toml` only after proving they solve a repeated workflow problem. Never put personal models, providers, secrets, or approval preferences in project configuration.

## Definition of done

A change is complete only when:

- The user goal and acceptance conditions are met.
- Relevant tests and `make check` ran, or the exact blocker is documented.
- The complete diff has no unrelated files, credentials, generated debris, or duplicate documents.
- Product or technical source-of-truth documents are synchronized when their baseline changes.
- Delivery states what changed, how it was verified, and any real remaining risk or decision.
