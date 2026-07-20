# Third-party notices

Eri-owned code and documentation use Apache-2.0; project attribution is in `NOTICE`. The following software retains its own license and does not become Apache-2.0 through use by Eri.

## Direct dependencies

### github.com/creack/pty v1.1.24

- Purpose: create a private pseudo-terminal for macOS `security`, keeping Keychain data out of command arguments and logs.
- Source: <https://pkg.go.dev/github.com/creack/pty>
- License: MIT
- Local-first impact: small pure-Go PTY library with no network, telemetry, or resident service; used only for short-lived Keychain writes.

### github.com/modelcontextprotocol/go-sdk v1.6.0

- Purpose: MCP stdio client/server protocol for the Eri Plugin Host and Google Workspace Plugin.
- Source: <https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk>
- License: transition from MIT to Apache-2.0; original MIT or Apache-2.0 applies per file, with some documentation under CC-BY-4.0. Distribution retains the complete module `LICENSE`.

### github.com/larksuite/oapi-sdk-go/v3 v3.9.9

- Purpose: official Lark/Feishu application-bot WebSocket events, message APIs, attachment transfer, token handling, and request signing for the remote Lark Channel.
- Source: <https://github.com/larksuite/oapi-sdk-go>
- License: MIT.
- Maintenance: official Lark Open Platform SDK. The dependency is network-active only when the explicitly configured Lark Channel is enabled; it has no telemetry or resident process of its own. App credentials remain process-memory-only and message content still passes through Eri's governed local boundaries.

### golang.org/x/term v0.45.0

- Purpose: detect interactive terminals and read a DeepSeek API key without echo during first-run setup.
- Source: <https://pkg.go.dev/golang.org/x/term>
- License: BSD-3-Clause
- Maintenance: official Go Project extension repository; adds no network service, telemetry, or resident process.

### gopkg.in/yaml.v3 v3.0.1

- Purpose: parse YAML frontmatter in standard Agent Skill `SKILL.md` files.
- Source: <https://pkg.go.dev/gopkg.in/yaml.v3>
- License: MIT and Apache-2.0, as marked per file.

### modernc.org/sqlite v1.54.0

- Purpose: CGO-free SQLite driver for local current state, Event Spine, and Transactional Outbox.
- Source: <https://pkg.go.dev/modernc.org/sqlite>
- License: BSD-3-Clause
- Copyright (c) 2017 The Sqlite Authors.

Redistribution must retain the complete license, copyright, conditions, and disclaimer supplied with each module.

## Distribution requirement

`go.sum` is an integrity manifest, not a license manifest. Prebuilt archives use `scripts/notices` to generate `THIRD_PARTY_LICENSES.txt` from the actual dependency graph of all final binaries; the build fails when a linked module lacks a top-level License or Notice. This file, the project license, and the generated license bundle ship in every archive.
