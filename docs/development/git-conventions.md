# Git conventions

These rules apply to human developers and coding agents. They keep `main` runnable and testable, and make branches and commits reviewable, reversible, and machine-readable.

## 1. Workflow

Eri uses lightweight GitHub Flow:

1. Create a short-lived branch from current `main`.
2. Complete one cohesive change and split it into logical atomic commits.
3. Run relevant tests, then `make check`, and review the complete diff.
4. Open a PR to `main`; use Draft for complex or incomplete work.
5. Merge after review and required checks, then delete the branch.

There are no long-lived `develop`, `release`, or environment branches. `main` is the only long-lived development branch and must always build, run, and pass repository checks. Known-failing or experimental states never merge.

Only a trivial edit that changes no runtime behavior, tests, build, dependency, configuration, protocol, or data may be made directly on `main` by a maintainer. A coding agent still needs explicit user authorization to commit.

Squash merge is the default so one cohesive PR becomes one Conventional Commit on `main`. Preserve a full commit sequence only when every commit is independently reviewable and worth retaining.

## 2. Branch names

```text
<type>/<short-description>
<type>/<issue-id>-<short-description>
```

- Use lowercase ASCII, digits, and hyphens, with one slash after the type.
- Use short, concrete kebab-case that describes the outcome.
- Use the real repository issue number when applicable.
- One branch carries one problem or end-to-end vertical slice and is deleted after merge.

| Type | Purpose |
| --- | --- |
| `feat` | User or system capability |
| `fix` | Incorrect behavior |
| `refactor` | Code structure without external behavior change |
| `docs` | Documentation only |
| `test` | Test additions or corrections |
| `chore` | Non-product maintenance |
| `build` | Build, dependency, or packaging |
| `ci` | CI configuration and scripts |
| `perf` | Performance |
| `hotfix` | Urgent production repair; commits still use `fix` |

Examples:

```text
feat/tool-add-file-search
fix/123-recover-outbox-delivery
refactor/agent-split-usage-accounting
docs/git-conventions
hotfix/redact-provider-token
```

Avoid unsupported types, uppercase, underscores, vague names, or long-lived release branches.

## 3. Commit messages

Follow Conventional Commits 1.0.0:

```text
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

Supported types are `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `build`, `ci`, `perf`, and `revert`.

Scope is optional and names the primary existing domain boundary:

| Scope | Boundary |
| --- | --- |
| `agent` | Provider-neutral Agent Loop |
| `model`, `ollama`, `deepseek` | Model gateway or provider |
| `tool` | Tool Gateway and built-ins |
| `policy`, `approval` | Safety, authorization, approval, and resume |
| `runtime` | Workers, recovery, scheduler, and durable execution |
| `channel` | Authoritative conversation ingress |
| `delivery`, `eval` | Evaluation, Outbox, and delivery |
| `content`, `store` | Encrypted content, SQLite, Events, and Outbox persistence |
| `identity`, `config` | Eri identity, Soul, and runtime configuration |
| `daemon`, `cli`, `localapi` | Process composition and local clients |
| `conversation`, `observatory` | Embedded Web surfaces |
| `repo` | Repository scripts, Makefile, and root configuration |

Omit scope for a genuinely cross-domain change. Do not use `core`, `common`, or `misc`, and do not name target packages that do not exist.

Descriptions use concise English imperative mood, lowercase initial, no terminal period, and describe the applied change. Keep the description near 50 characters and the full subject at most 72. Avoid `update code`, `fix bug`, `misc changes`, `WIP`, or describing only that tests pass.

Good examples:

```text
feat(tool): add workspace file search
fix(delivery): prevent duplicate outbox sends
refactor(agent): isolate usage aggregation
test(policy): cover overwrite approval expiry
docs: define local provider setup
ci: run repository checks on pull requests
```

## 4. Body and footer

Add a body when rationale or constraints are not obvious, or when changing behavior, protocol, durable data, security, privacy, cost, recovery, compatibility, idempotency, or concurrency. Explain the previous problem, why the design is correct, and the impact; do not narrate code that already explains itself. Wrap near 72 columns.

Use trailers for references and attribution:

```text
Refs: #123
Closes: #456
Co-authored-by: Name <email@example.com>
```

## 5. Breaking changes

Mark a breaking change when existing users must change calls, configuration, data, or integration. Examples include incompatible CLI, Local API, Plugin/Tool, public configuration, schema, Event, ContentRef, security, authorization, or delivery semantics without transparent migration.

Use `!` and a `BREAKING CHANGE:` footer with impact and migration:

```text
feat(localapi)!: replace status response fields

Align the response with the durable task state model and remove the
temporary `busy` field.

BREAKING CHANGE: clients must read `run_status` instead of `busy`.
```

Internal refactors, compatible additions, and transparent migrations are not breaking.

## 6. Atomicity and pre-commit checks

Before committing:

1. Inspect `git status --short` and preserve unrelated or parallel work.
2. Stage exact files or hunks and inspect `git diff --cached`.
3. Run relevant tests and `make check`; never use `--no-verify`.
4. Write the message from the staged diff, not a chat plan or branch name.
5. Check for secrets, tokens, cookies, personal data, `.env`, local databases, editor state, and generated debris.

If a subject needs "and" to connect independently reversible goals, split it. Tests and documentation that make a behavior valid belong with that implementation, not in mechanical file-type commits.

## 7. Coding agents

Agents follow all human standards and additionally:

- Never create or switch branches, commit, push, or open a PR without explicit user authorization.
- Never commit directly to `main` without explicit instruction.
- Never force-push, rebase, amend, reset, rewrite, or delete another developer's published history.
- Never use `git add .` or `git add -A`; stage only confirmed task paths or hunks.
- Preserve unrelated work and never discard it to obtain a clean tree.
- Never skip checks or describe failure as success; report unrun checks exactly.
- Never commit credentials, local environment files, personal data, or unchecked generated output.
- Re-read the staged diff and report whether commit, push, or PR actually completed.

When file ownership or parallel work is unclear, preserve the state and ask instead of cleaning, stashing, or rewriting history.

## 8. References

- [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/)
- [GitHub Flow](https://docs.github.com/en/get-started/using-github/github-flow)
- [Angular commit message guidelines](https://github.com/angular/angular/blob/main/contributing-docs/commit-message-guidelines.md)
- [GitLab branch documentation](https://docs.gitlab.com/user/project/repository/branches/)
- [How to Write a Git Commit Message](https://cbea.ms/git-commit/)
