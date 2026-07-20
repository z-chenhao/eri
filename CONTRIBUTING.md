# Contributing to Eri

Eri uses an AI-native collaboration model: people own intent, value judgments, and final authority; AI researches, implements, verifies, and delivers autonomously inside confirmed boundaries. High autonomy does not lower the standard.

Read [AGENTS.md](AGENTS.md) first, then the relevant sections of [MVP product](docs/mvp-product.md) or [MVP technical design](docs/mvp-technical.md).

## 1. State the task contract

Use this minimal shape when practical:

```text
Goal:
Context:
Constraints:
Done:
```

No formal form is required. An agent should infer known facts from natural language and the repository, and ask only questions that materially change the solution, risk, or outcome.

Confirm these before implementation:

- Product meaning, Eri's Soul, or the user relationship.
- Architecture boundaries, public protocols, durable data models, or irreversible migration.
- Privacy, authorization, cost, safety, or external side effects.
- Major dependencies, cloud services, or anything that weakens local-first behavior.
- Decisions where several choices are valid but long-term cost differs substantially.

Do not repeatedly interrupt the user for low-risk naming, local decomposition, or small test-design details inside a confirmed boundary.

## 2. Development loop

1. **Inspect** the worktree, sources of truth, relevant code, and existing tests. Do not guess from memory.
2. **Clarify** only unknowns that change the outcome.
3. **Plan** independently verifiable steps for complex work; execute simple work directly.
4. **Slice** the smallest real user value or system closed loop.
5. **Implement** cohesively and avoid speculative abstractions.
6. **Verify** the most relevant tests first, then `make check`; verify behavior, not only compilation.
7. **Review** the full diff, failure paths, privacy, lineage, and documentation impact.
8. **Deliver** outcome, evidence, risks, and next steps without flooding the user with process logs.

## 3. Change size

- One change solves one clear problem or one end-to-end vertical slice.
- Separate architecture refactors from behavior changes when practical.
- Record and report unrelated problems instead of expanding scope.
- Large work may be staged, but every stage must remain verifiable and state what is not yet real.

## 4. Git workflow

Eri uses lightweight GitHub Flow, without long-lived `develop` or `release` branches:

1. Except for a trivial non-behavioral edit, branch from current `main`.
2. Name branches `<type>/<short-description>` or `<type>/<issue-id>-<short-description>` with lowercase kebab-case detail.
3. Use `<type>(<scope>): <description>` Conventional Commits and keep each commit atomic.
4. Run relevant tests and `make check`, merge through a PR, then delete the branch.

`main` must remain runnable and testable. Explain rationale and impact in a commit body when needed. Mark incompatibility with `!` and a `BREAKING CHANGE:` footer. See [Git conventions](docs/development/git-conventions.md).

## 5. Tests and evidence

Minimum expectations:

- New behavior covers the normal path, critical boundaries, and failure path.
- A bug fix starts with a reproducer or minimal evidence.
- Critical Agent Loop behavior uses stable fixtures, fake boundaries, or versioned eval cases; ordinary tests do not depend on live cloud models.
- Tool, Plugin, Delivery, and external side-effect tests cover idempotency, timeout, cancellation, unknown outcomes, and recovery.
- Log, Episode, Memory, and Dataset tests verify redaction, provenance, and deletion propagation.

```bash
make fmt       # Format Go source
make test      # Run Go tests
make vet       # Run go vet
make check     # Validate repository, scripts, formatting, vet, and tests
```

See [CI/CD and supply chain](docs/development/ci-cd.md). Agent-assisted PRs disclose the agent's scope. Green checks are evidence, not a replacement for Code Owner review of test validity, high-trust files, and the final diff.

## 6. Documentation

- Product behavior belongs in `docs/mvp-product.md`.
- Technical boundaries belong in `docs/mvp-technical.md`.
- Do not duplicate the same fact across design documents; README is an entrypoint and navigation layer.
- Temporary research, plans, and experiment data do not automatically become durable policy.
- Add a new design document only when the existing sources of truth cannot reasonably hold the material and the user confirms it should be maintained.
- Repository-owned source, tests, documentation, and UI copy are English. Runtime assistant output follows the user's language.

## 7. Pull requests

A PR description should make human and agent review efficient:

- The user or system problem.
- Why the solution matches product and technical baselines.
- Exact changed and unchanged scope.
- Commands run and their results.
- Risk, migration, privacy, and rollback impact.
- Screenshots for UI changes and reproducible evidence for behavior changes.

Use [.github/pull_request_template.md](.github/pull_request_template.md).

## 8. Evolving the rules

Only promote a rule into `AGENTS.md`, tests, lint, hooks, or a Skill after a problem repeats, a workflow stabilizes, or mechanical enforcement proves necessary.

Prefer tests and automated checks, then concise repository rules, then temporary prompting. Do not create unverified, unmaintained templates merely to appear AI-native.

## 9. License and provenance

Unless explicitly stated otherwise, contributions use [Apache License 2.0](LICENSE), SPDX `Apache-2.0`, under a lightweight inbound-equals-outbound rule without a separate CLA.

Contributors confirm they have the right to submit and sublicense their contribution. Record the source and license of third-party code, models, data, Plugins, and visual assets; preserve copyright and notices; verify current use and redistribution rights. Do not submit unknown, incompatible, employer-restricted, or otherwise unauthorized material.
