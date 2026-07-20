# CI/CD and supply-chain policy

This policy defines GitHub merge gates, continuous verification, and release flow. Agents may autonomously produce candidate changes and evidence, but cannot be their own root of trust. Human Code Owners remain responsible for authority boundaries, test validity, and final delivery.

## 1. Current delivery boundary

Eri is a local-first Go application whose credential isolation currently depends on macOS Keychain. The first public MVP distributes source. macOS `amd64` and `arm64` archives are short-lived release candidates, not public releases.

Before code signing and Apple notarization exist, tag workflows verify and upload seven-day Actions Artifacts only. They do not create a Draft or public GitHub Release. Restoring binary distribution requires a separate design review and human inspection of archives, change notes, provenance, signing, and notarization.

## 2. Trust model

- Treat all PR code as untrusted, including repository branches, forks, Dependabot, and coding-agent output.
- PR workflows use read-only `GITHUB_TOKEN`, no Secrets, no self-hosted runners, and no `pull_request_target` or `workflow_run` execution of PR code.
- Pin every external Action to a full commit SHA. Dependabot raises update PRs that require CI and Code Owner review.
- `go.mod` defines language compatibility; `.go-version` pins the CI and release security patch. Upgrade immediately for reachable standard-library vulnerabilities.
- A green CI result proves only that checks ran on a diff. Agents can edit implementation, tests, and workflows together, so protect `.github/`, `scripts/`, `Makefile`, and `AGENTS.md` through CODEOWNERS and Rulesets.
- Release write permission belongs only behind a protected `release` Environment. PR and ordinary `main` validation have no release authority.
- Never place model API keys, signing credentials, Apple credentials, or user data in ordinary CI. Future signing secrets may exist only in a protected release job.

## 3. Pipelines

| Workflow | Trigger | Checks | Permission |
| --- | --- | --- | --- |
| `CI` | PR, push to `main` | branch/PR policy, `make check`, cross-package coverage floor, race tests, macOS source validation, cross-architecture release build | `contents: read` |
| `Security` | PR, `main`, weekly, manual | dependency review, `govulncheck`, workflow syntax and security policy | `contents: read` |
| `Release candidate build` | protected `vX.Y.Z` tag | tag validation, full checks, two macOS architectures, SHA-256, seven-day artifact | `contents: read` |

Every job has a timeout. Superseded PR runs cancel; release runs do not. `make check` remains the common local and CI floor.

### Required checks

Configure these on the `main` Ruleset with strict up-to-date branches:

```text
Policy
Quality
Race
Coverage
macOS smoke
Release build
Dependency review
Govulncheck
Workflow audit
```

`Dependency review` exists only on PRs. Never require a scheduled-only or tag-only job on `main`.

## 4. Pull requests

1. Use a short-lived branch from [Git conventions](git-conventions.md). A Conventional Commit PR title becomes the squash commit.
2. Disclose coding-agent scope and authority boundaries. Human authors still document evidence and risk.
3. Run candidate code without Secrets and with read-only permission. Fix or explain failures honestly; never delete tests, weaken gates, or add silent fallback to manufacture green status.
4. Code Owners inspect changed tests, snapshots and fixtures, workflow permissions and triggers, dependency provenance, and scope.
5. After required checks and applicable review gates, a human Owner squash-merges. Rulesets block force-push, deletion of `main`, and PR bypass. Agents never merge or enable auto-merge.

## 5. Release candidates

1. Confirm `main` is green and create a protected SemVer tag, currently shaped like `v0.1.0-rc.1`.
2. Verify the tag points at the event commit, run `make check`, and build macOS `amd64` and `arm64` with the same script.
3. Upload archives and `SHA256SUMS` as a seven-day Actions Artifact. The workflow has no GitHub Release write permission.
4. Humans may install-test the candidate but must not present it as a signed public MVP.
5. Add a protected public-release job only after independent review of signing, notarization, and provenance. Never move or reuse a published tag.

```bash
make check
make release-dist VERSION=v0.1.0-rc.1
```

`release-dist` rejects a non-empty `dist/` and a Go toolchain that differs from `.go-version`, preventing stale artifact mixing and toolchain drift.

## 6. GitHub settings

Repository files cannot enforce these settings; an Owner configures them after workflows first run.

### Actions

- Set workflow permissions to read repository content and packages. Disable Actions creating or approving PRs.
- Allow only GitHub-owned Actions. Current workflows install Go tools by exact module version.
- Keep fork tokens read-only and never send Secrets or write authority to fork PRs.

### `main` Ruleset

- Require PRs, required checks, strict up-to-date branches, linear history, and block force-push and deletion.
- Do not give coding agents, GitHub Apps, or normal workflows bypass. Emergency bypass belongs only to the Owner and leaves an audit record.
- With one GitHub identity for both human and agent work, set required approvals to `0`: GitHub cannot let the PR author approve their own PR. The Owner manually inspects and merges while CODEOWNERS marks high-trust paths.
- After adding a separate Agent GitHub App or maintainer, require one Code Owner approval, dismiss stale approvals, and require the last reviewable push to be approved by another identity. The Agent App may write work branches and PRs only—never Ruleset bypass, Environment approval, Secrets, or Release authority.

### Tags, environments, and security

- Protect `v*` tags: maintainer creation only; no update, deletion, or force-update.
- Do not create a `release` Environment until signing and notarization design is approved.
- Enable Dependency graph, Dependabot alerts and security updates, Secret scanning, Push protection, and CodeQL default setup for Go and Actions.
- Add required check names only after GitHub has observed a successful workflow run.

## 7. Maintenance

- Review workflows, Action SHAs, Go security tools, and Rulesets quarterly. Never auto-merge Dependabot PRs.
- Update `.go-version` immediately for a Go security patch or reachable `govulncheck` standard-library finding.
- Justify every new Action, prefer GitHub-maintained Actions, minimize permission, and pin a full SHA.
- CI never calls real model providers, personal Keychain, or production user directories. Use deterministic fakes, fixtures, and eval cases.
- A scheduled security failure is real work. Record reachability analysis and a review date when remediation cannot be immediate; never ignore the exit code.
- Design least-privilege permissions before introducing an Agent GitHub App.

## 8. References

- [GitHub secure use reference](https://docs.github.com/en/actions/reference/security/secure-use)
- [Workflow events and fork permissions](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows)
- [Ruleset rules](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets)
- [CODEOWNERS](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners)
- [Deployment environments](https://docs.github.com/en/actions/reference/workflows-and-actions/deployments-and-environments)
- [Dependency review](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/manage-your-dependency-security/configure-dependency-review-action)
- [CodeQL default setup](https://docs.github.com/en/code-security/how-tos/find-and-fix-code-vulnerabilities/configure-code-scanning/configure-code-scanning)
- [Artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations)
- [Go vulnerability management](https://go.dev/doc/security/vuln/)
