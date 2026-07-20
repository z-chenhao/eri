---
name: news-monitoring
description: Consent-based recurring news monitoring, event clustering, and useful notification. Use when the user wants ongoing tracking, a recurring domain digest, alerts, daily news, or changes followed over time.
---

# News monitoring and push

1. Do not create ongoing monitoring until the user agrees to scope and cadence. Define topic boundaries, trusted source types, relevance threshold, delivery cadence, and urgent exceptions.
2. Prefer first-party announcements and primary records. Cluster reports about the same underlying event; do not count syndication as independent confirmation.
3. Track event time, official release time, and report time separately. Compare with the previous delivered state so unchanged items are suppressed.
4. Each item must answer: what changed, why it matters to this user, confidence/source quality, and whether action is worthwhile.
5. Notify immediately only for high-value narrow windows. Put the rest into a concise periodic digest. Silence is not negative feedback.
6. Respect background cloud budgets; narrow scope, defer, or use a local model when the next run would exceed its mandate or resource ceiling.
