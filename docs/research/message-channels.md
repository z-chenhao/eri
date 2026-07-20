# Eri Message Channel research

> Status: decision record; the first owner-bound Lark/Feishu vertical slice is implemented
> Research date: 2026-07-18
> Scope: remote Message Channel selection and implementation preparation
> Current baselines remain [MVP product](../mvp-product.md) and [MVP technical design](../mvp-technical.md).

## 1. Goal

Choose a daily messaging channel where a user can converse with the same long-lived Eri in a familiar app and receive proactive reminders, suggestions, and deliverables.

"Like a real person" means a natural one-to-one relationship, continuous identity, good timing, and appropriate proactivity. It never means impersonating a human or hiding automation.

## 2. Constraints

- Identity, Soul, Memory, and authoritative Conversation stay in local Core; a Channel only sends and receives.
- A Channel cannot create a second personality, memory, or task system.
- Prioritize macOS while allowing other open-source users to self-host.
- Prefer official, stable APIs that do not risk account suspension.
- External platforms receive the minimum message data.
- Passwords, OAuth access/refresh tokens, cookies, API secrets, and session grants never enter Eri configuration, database, logs, Memory, Episodes, or datasets.
- Proactive messages use one preferred Channel and deduplicate against the same Web/CLI timeline.

## 3. Recommendation

The first formal remote Channel is a **Feishu/Lark application bot**.

Keep three later paths:

1. **Feishu application bot:** first stable, official reference for mainland China.
2. **Telegram Bot:** second official reference for international open-source users.
3. **iMessage Bridge:** experimental macOS path with the strongest native-contact feeling.

Keep **Matrix** as the long-term open and self-hosted sovereignty candidate. Do not prioritize personal WeChat automation, WhatsApp Business, or Signal.

## 4. Comparison

| Platform | Personal-contact feel | Official two-way API | Proactive messages | Local-first/open | Cost to maintain | Recommendation |
| --- | --- | --- | --- | --- | --- | --- |
| iMessage | Highest | Incomplete | Possible through fragile paths | Good inside Apple ecosystem | High and brittle | Experiment |
| Feishu | High, but visibly a bot | Complete | Yes | Cloud platform | Medium | First formal implementation |
| Telegram | Medium, visibly a bot | Complete | After user initiates | Easy protocol, cloud transit | Low | International reference |
| Matrix | High with an independent account | Open and complete | Yes | Strongest, self-hostable | High | Long-term sovereignty |
| Personal WeChat | Most familiar locally | No official personal bot API | Unofficial only | Closed | Extreme account and maintenance risk | Do not integrate |
| WhatsApp | High but business identity | Complete with business rules | Template/window restrictions | Commercial cloud | High | Poor fit |
| Signal | High | No official bot API | Unofficial client | Strong privacy | High compatibility burden | Do not integrate |

Personal-contact appearance and official automation maturity conflict structurally. Platforms that look like ordinary contacts usually restrict automation; complete Bot APIs expose an App/Bot identity. Product quality therefore depends more on language, timing, proactivity, and memory continuity than on hiding the bot label.

## 5. Candidate analysis

### 5.1 Feishu application bot

Feishu supports a custom name and avatar, direct chat, inbound events, proactive push, text, rich text, images, files, audio, cards, replies, edits, recalls, reactions, history, and read state. Its long-lived event connection lets a local daemon avoid a public webhook. Message IDs, reply relations, and receipts map well to Eri Interaction and Delivery.

Advantages:

- Mature mainland China desktop and mobile clients.
- Complete official two-way messaging, proactive push, and attachments.
- Structured cards fit approval and option comparison.
- Existing Agent-app setup flows reduce open-source onboarding cost.

Limitations:

- Visible bot identity and workplace aesthetic.
- Application, permission, and publication configuration.
- App credentials require the isolated credential boundary in section 6.

Conclusion: **implemented as the first formal Channel**.

### 5.2 iMessage

iMessage most closely supports "Eri is a person in Contacts." A separate Apple ID, avatar, and contact profile can create a native one-to-one thread.

Apple's Messages Framework is for iMessage apps, stickers, media, and interactive extensions—not a general background bot API. A local inspection of the macOS `Messages.app` scripting dictionary on 2026-07-18 found commands to send text/files to participants and chats, and to read accounts, contacts, chats, and transfers, but no public ordinary inbound-message object or event.

A full bridge would likely observe `~/Library/Messages/chat.db`, use UI Automation, or rely on another private behavior. That introduces Full Disk Access, Accessibility permission, private schema, and OS-upgrade risk. An independent Eri Apple ID also requires a durable Messages login managed by the OS, not by Eri.

Conclusion: **best experience target, experimental macOS Channel only; never the sole open-source reference**.

### 5.3 Telegram Bot

Telegram Bot API offers HTTPS, long polling, webhooks, private chat, replies, attachments, typing status, and streaming draft replies. Long polling suits a local daemon without a public callback.

Limitations: the platform shows a bot label; a bot cannot initiate the first conversation; a durable Bot Token is required; mainland China network availability cannot be assumed. Use only the official Bot API, never user-account automation.

Conclusion: **second formal reference for international users**.

### 5.4 Matrix

Matrix Client-Server API is open and supports independent accounts, profiles, bidirectional sync, history, and idempotent transaction IDs. Users may choose a public or fully self-hosted homeserver.

It best supports protocol and data sovereignty, but requires another client and potentially homeserver operations. Device access/refresh tokens require an independent Auth Broker under Eri's no-persisted-session-grant boundary.

Conclusion: **long-term sovereign Channel and protocol reference**.

### 5.5 Personal WeChat

There is no official personal-account bot send/receive API. Official Tencent surfaces are service accounts, Mini Programs, WeCom, and customer service, not ordinary personal contacts. Client injection, reverse protocols, hooks, or simulated login risk suspension, privacy loss, persistent personal sessions, and constant maintenance.

Conclusion: **do not implement unless Tencent introduces a suitable official capability**.

### 5.6 WhatsApp Business

Cloud API requires a WhatsApp Business account and business number. Outside the 24-hour customer-service window, proactive messaging normally uses pre-approved templates. That turns Eri into a customer-service identity and constrains natural contextual proactivity.

Conclusion: **not a priority for personal Eri**.

### 5.7 Signal

Signal has no official Bot API. Community `signal-cli` is explicitly unofficial, stores account/encryption material, and must track server compatibility changes.

Conclusion: **strong privacy, but fails official stability and no-session-persistence requirements**.

## 6. Credential boundary

Feishu, Telegram, and Matrix all need a durable identity credential for unattended background operation. Any implementation must satisfy:

- Channel secrets never enter Git, Eri configuration, SQLite, Content Store, Memory, Events, logs, Episodes, datasets, or exports.
- Secrets enter at startup only from process environment or an External Auth Broker; a Manifest names environment variables, never values.
- Short-lived access tokens remain in memory and disappear on exit.
- Logs, errors, and Observatory redact secrets, tokens, and authentication response bodies.
- Disconnect removes Core identity bindings and Capability Handles; the credential owner or broker revokes the platform grant.

If "never persist credentials" also forbids OS Keychain and an independent broker, a daemon cannot restore an official remote Channel after restart. Confirm the allowed external credential custodian before implementation.

## 7. Suggested phases

### Phase 1: Channel protocol vertical slice — current

The current Feishu/Lark slice validates configured owner binding, direct-chat sender verification, durable external-message deduplication, serialized ingress, one authoritative Web/CLI/Lark Conversation, text/replies/attachments, Eval/Outbox/idempotent send/actual Receipt, and foreground secret injection with redacted logs. Decision-card rendering, presence projection, and an independent credential broker remain later increments rather than hidden claims of this slice.

### Phase 2: second platform

Add Telegram to prove the Channel Plugin contract does not leak Feishu fields or authentication into Core. Verify long polling, reconnect, format degradation, and international deployment guidance.

### Phase 3: iMessage experiment

Prototype a separate Eri Apple ID, inbound observation and required permissions, OS-upgrade compatibility, proactive text/attachments/replies, deduplication, receipts, and whether `chat.db` access can remain read-only, minimal, and auditable.

Promote it only after stability and privacy evidence are acceptable.

## 8. Revalidate before implementation

Platform APIs, pricing, proactive-message rules, and account policies change. Recheck official API/SDK versions and licenses, direct-chat support, network requirements, proactive windows/templates/rates/pricing, token lifecycle and revocation, current AI automation terms, attachment and message limits, editing/retraction/read/reply support, and target-region availability.

## 9. Primary references

- Feishu: [Messaging overview](https://open.feishu.cn/document/server-docs/im-v1/introduction?lang=en-US)
- Feishu: [Bot guide](https://open.feishu.cn/document/client-docs/bot-v3/how-to-use-bot-in-feishu)
- Feishu: [Messaging FAQ](https://open.feishu.cn/document/server-docs/im-v1/faq?lang=en-US)
- Feishu: [Agent application integration](https://open.feishu.cn/document/mcp_open_tools/integrating-agents-with-feishu/overview)
- Apple: [Messages Framework](https://developer.apple.com/documentation/messages)
- Apple: [Mac Automation Scripting Guide](https://developer.apple.com/library/archive/documentation/LanguagesUtilities/Conceptual/MacAutomationScriptingGuide/)
- Telegram: [Bots introduction](https://core.telegram.org/bots)
- Telegram: [Bot API](https://core.telegram.org/bots/api)
- Matrix: [Client-Server API v1.14](https://spec.matrix.org/v1.14/client-server-api/)
- WhatsApp: [Business Messaging Policy](https://whatsappbusiness.com/policy/)
- Signal community implementation: [signal-cli](https://github.com/AsamK/signal-cli)
