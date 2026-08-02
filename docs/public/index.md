---
layout: home
hero:
    name: mailgw
    text: An SMTP gateway you can reason about
    tagline: Declarative routing rules, a durable outbound queue, and an audit trail for every message it touches.
    actions:
        - theme: brand
          text: What is mailgw?
          link: /guide/what-is-mailgw
        - theme: alt
          text: Install it
          link: /guide/installation
features:
    - title: Rules, not code
      details: Routing and policy are an ordered list of typed predicates. Every field name is checked when the configuration loads, so a typo is a startup error rather than a rule that silently never fires.
    - title: Per-recipient decisions
      details: A message addressed to three places is split into three envelopes, each routed on its own. A rejection reaches the client on its own RCPT TO line when the rules allow it.
    - title: Nothing is lost quietly
      details: Mail is spooled before it is acknowledged, retried on a schedule, and bounced when it finally fails. Audit events that cannot be delivered are parked on disk and replayed.
    - title: Zero-configuration nodes
      details: An edge gateway ships with no environment, no arguments and no files. It registers itself, an operator approves it, and every setting arrives as a versioned configuration bundle.
---
