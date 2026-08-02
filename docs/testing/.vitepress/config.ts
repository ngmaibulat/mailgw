import { defineConfig } from "vitepress";

// Manual test plans — the things the automated suites cannot check.
//
// Everything here is meant to be executed by a person against a running stack
// and recorded. Each plan states its preconditions, numbered steps, and the
// expected result of each step, so two people running it get the same answer
// and a failure is reportable without a conversation.
//
// If a plan here could be a `go test`, it should be one instead. These exist
// because they need a real network, a real relay, a real browser, or a
// judgement call about what an operator sees.
export default defineConfig({
    title: "mailgw test plans",
    description:
        "Manual test plans for the mailgw stack: preconditions, steps, expected results.",
    lang: "en-GB",
    cleanUrls: true,
    lastUpdated: true,

    themeConfig: {
        nav: [
            { text: "How to use these", link: "/how-to-use" },
            { text: "Plans", link: "/plans/tp-01-smoke" },
            { text: "Release checklist", link: "/release-checklist" },
        ],

        sidebar: [
            {
                text: "Before you start",
                items: [
                    { text: "How to use these", link: "/how-to-use" },
                    { text: "Test environment", link: "/environment" },
                ],
            },
            {
                text: "Test plans",
                items: [
                    {
                        text: "TP-01 · Smoke test",
                        link: "/plans/tp-01-smoke",
                    },
                    {
                        text: "TP-02 · IP allowlist",
                        link: "/plans/tp-02-allowlist",
                    },
                    {
                        text: "TP-03 · Routing rules",
                        link: "/plans/tp-03-routing",
                    },
                    {
                        text: "TP-04 · Inbound TLS",
                        link: "/plans/tp-04-tls",
                    },
                    {
                        text: "TP-05 · Inbound AUTH",
                        link: "/plans/tp-05-auth",
                    },
                    {
                        text: "TP-06 · Queue and retries",
                        link: "/plans/tp-06-queue",
                    },
                    {
                        text: "TP-07 · Bounces and DSN",
                        link: "/plans/tp-07-dsn",
                    },
                    {
                        text: "TP-08 · Claim and provisioning",
                        link: "/plans/tp-08-provisioning",
                    },
                    {
                        text: "TP-09 · Deploy and rollback",
                        link: "/plans/tp-09-deploy-rollback",
                    },
                    {
                        text: "TP-10 · Observability",
                        link: "/plans/tp-10-observability",
                    },
                ],
            },
            {
                text: "Sign-off",
                items: [
                    {
                        text: "Release checklist",
                        link: "/release-checklist",
                    },
                    { text: "Reporting a failure", link: "/reporting" },
                ],
            },
        ],

        outline: { level: [2, 3] },
        search: { provider: "local" },
        footer: {
            message: "Manual test plans — internal",
            copyright: "mailgw",
        },
    },
});
