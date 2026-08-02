import { defineConfig } from "vitepress";

// The customer-facing site: what mailgw does, how to run it, and how to
// configure it. Everything here describes behaviour an operator can observe.
//
// What must NOT land here: milestone plans, audit findings, defect histories,
// and the "what was built differently" notes. Those are engineering context
// and live on the internal site — see docs/internal. The distinction matters
// because several of them describe security defects and the reasoning behind
// credential handling.
export default defineConfig({
    title: "mailgw",
    description:
        "An SMTP gateway that routes mail by declarative rules, spools it durably, and reports what it did.",
    lang: "en-GB",
    cleanUrls: true,
    lastUpdated: true,

    themeConfig: {
        nav: [
            { text: "Guide", link: "/guide/what-is-mailgw" },
            { text: "Configuration", link: "/config/overview" },
            { text: "Routing rules", link: "/rules/overview" },
            { text: "Operations", link: "/operations/observability" },
        ],

        sidebar: [
            {
                text: "Guide",
                items: [
                    {
                        text: "What is mailgw?",
                        link: "/guide/what-is-mailgw",
                    },
                    { text: "How a message flows", link: "/guide/message-flow" },
                    { text: "Installation", link: "/guide/installation" },
                    {
                        text: "Central Management",
                        link: "/guide/central-management",
                    },
                ],
            },
            {
                text: "Configuration",
                items: [
                    { text: "Overview", link: "/config/overview" },
                    { text: "server.yaml", link: "/config/server" },
                    { text: "Relays", link: "/config/relays" },
                    { text: "Allowlist", link: "/config/allowlist" },
                    { text: "TLS", link: "/config/tls" },
                    { text: "Inbound AUTH", link: "/config/auth" },
                    { text: "Message authentication", link: "/config/msgauth" },
                    { text: "Rate limits", link: "/config/ratelimit" },
                ],
            },
            {
                text: "Routing rules",
                items: [
                    { text: "Overview", link: "/rules/overview" },
                    { text: "Matching", link: "/rules/matching" },
                    { text: "Actions", link: "/rules/actions" },
                    { text: "Field reference", link: "/rules/fields" },
                ],
            },
            {
                text: "Operations",
                items: [
                    { text: "The queue", link: "/operations/queue" },
                    {
                        text: "Delivery status notifications",
                        link: "/operations/dsn",
                    },
                    {
                        text: "Observability",
                        link: "/operations/observability",
                    },
                    {
                        text: "Command reference",
                        link: "/operations/cli",
                    },
                    {
                        text: "Troubleshooting",
                        link: "/operations/troubleshooting",
                    },
                ],
            },
        ],

        outline: { level: [2, 3] },
        search: { provider: "local" },
        footer: {
            message: "MIT licensed",
            copyright: "mailgw",
        },
    },
});
