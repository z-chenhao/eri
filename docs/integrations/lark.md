# Lark / Feishu Channel

> Status: owner-bound application bot configured and live Conversation delivery exercised
> Scope: one trusted owner's direct conversation with the same local Eri

Eri connects a self-built Lark or Feishu application bot through the platform's official Go SDK and WebSocket long connection. The adapter accepts only direct (`p2p`) messages from one configured owner Open ID. Inbound text, images, and files enter the same encrypted, durable Conversation used by Web and CLI. Replies still pass the ordinary Agent Loop, Eval, Outbox, idempotent delivery, and Receipt path. Assistant Markdown is converted to a native `post` with a governed `md` node; Runtime diagnostic JSON is never forwarded to the chat.

## 1. Install the official CLI

The official installer is:

```bash
npx @larksuite/cli@latest install
lark-cli --version
```

On this machine the CLI is installed and reports `1.0.72`. The installer reached its QR-code application setup, but scanning and authorization were deliberately skipped. Eri does not read the CLI profile; the CLI is only an official setup and diagnostic tool.

## 2. Create or select the application

When ready to authorize:

```bash
lark-cli config init
```

Scan the QR code and create or select a self-built application. Enable the Bot capability. In the developer console, grant only the application permissions required for this slice:

- `im:message.p2p_msg:readonly` — receive direct messages sent to the bot.
- `im:message:send_as_bot` — send and reply as the bot.
- `im:message:readonly` — read message resources referenced by inbound events.
- `im:resource` — upload and download message images and files.

Subscribe to `im.message.receive_v1`, select the WebSocket/long-connection delivery mode, and publish the application version. User OAuth login is not part of the bot Channel; `lark-cli auth login` is unnecessary for Eri's application-identity connection.

## 3. Bind the owner

After the application is active, consume one direct-message event and send the bot a test message:

```bash
lark-cli event consume im.message.receive_v1 --max-events 1 --timeout 2m --as bot --jq '.sender_id'
```

Use the returned `ou_...` Open ID as the sole owner binding. Treat it as a personal identifier: do not commit it, paste it into issues, or place it in logs.

## 4. Start the foreground daemon

Copy the App ID and App Secret from the developer console. Bind them only in the protected current shell; hidden input avoids placing the secret in shell history:

```bash
export LARK_ERI_API_KEY=cli_...
read -s "LARK_ERI_API_SECRET?Lark App Secret: "
echo
export LARK_ERI_API_SECRET
export LARK_ERI_OWNER_OPEN_ID=ou_... # required for the first binding; later starts recover the unique confirmed local binding
export LARK_ERI_BRAND=feishu  # use lark for the international Lark domain
./bin/eri daemon
```

The App key and secret are always required together. `LARK_ERI_OWNER_OPEN_ID` is required for the first trusted binding. After Eri has accepted messages from that explicit owner, later foreground starts may recover the binding only when local channel history contains exactly one distinct inbound sender; zero or multiple senders fail closed and require an explicit owner value. `eri doctor` reports only whether the Channel credentials are configured and which domain is selected; it does not print App ID, owner ID, or secrets.

Do not put the App Secret in `.env`, Eri configuration, command arguments, launchd plists, logs, or source control. `eri install` intentionally does not copy this transient environment binding, so the current Lark slice is foreground-only. Unattended background startup requires a future independent Channel credential broker; it must not be approximated by persisting the secret in Core.

## 5. Live acceptance

Once the user completes application authorization, verify:

1. A direct owner message appears once in Web/CLI even if Lark redelivers the event.
2. A non-owner or group message creates no Eri Interaction.
3. Text, image, and file inputs are available to the ordinary Agent Loop under existing content policy.
4. Eri's evaluated response replies in the same Lark chat and stores the returned platform message ID as Receipt evidence.
5. Restart recovery does not create a second logical delivery.

Owner-bound text delivery and real platform Receipt capture have been exercised. Automated contract tests cover binding, deduplication, attachment, target, idempotency, Markdown-to-`post` presentation, Runtime-error redaction, and Receipt boundaries without using real credentials. Group/non-owner rejection, attachment round trips, and restart redelivery remain explicit live regression checks for a release candidate.

## Official references

- [Lark CLI installation and setup](https://open.feishu.cn/document/mcp_open_tools/feishu-cli-let-ai-actually-do-your-work-in-feishu)
- [Integrating an Agent with Feishu](https://open.feishu.cn/document/mcp_open_tools/integrating-agents-with-feishu/overview)
- [Receive message event](https://open.larksuite.com/document/server-docs/im-v1/message/events/receive)
- [Application permission list](https://open.larksuite.com/document/ukTMukTMukTM/uYTM5UjL2ETO14iNxkTN/scope-list?fb=2&lang=en-US)
- [Official Go SDK](https://github.com/larksuite/oapi-sdk-go)
- [Official Go SDK long-connection guide](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/doc/channel.zh.md)
