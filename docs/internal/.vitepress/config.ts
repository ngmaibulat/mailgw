import { defineConfig } from "vitepress";

// The engineering site: how the repo is put together, why the load-bearing
// decisions were made, and how to work on it.
//
// It deliberately does NOT re-render plans/ or the TODO backlogs. Those are
// living files that CLAUDE.md, plans/README.md and mailgw-go/TODO.md all
// cross-reference by relative path; copying them here would give two versions
// that drift, and rewiring those paths to point at a docs site would break the
// indexes that make them navigable in an editor. This site links out to them
// instead, and covers what they do not: the shape of the whole thing.
export default defineConfig({
    title: "mailgw internals",
    description:
        "Architecture, conventions and working practices for the mailgw monorepo. Internal.",
    lang: "en-GB",
    cleanUrls: true,
    lastUpdated: true,

    themeConfig: {
        nav: [
            { text: "Architecture", link: "/architecture/overview" },
            { text: "Packages", link: "/packages/mailgw-go" },
            { text: "Working on it", link: "/dev/getting-started" },
            { text: "History", link: "/history/milestones" },
        ],

        sidebar: [
            {
                text: "Architecture",
                items: [
                    { text: "Overview", link: "/architecture/overview" },
                    { text: "Repository map", link: "/architecture/repo-map" },
                    {
                        text: "The mail path",
                        link: "/architecture/mail-path",
                    },
                    {
                        text: "Central Management",
                        link: "/architecture/central-management",
                    },
                    {
                        text: "Standing decisions",
                        link: "/architecture/decisions",
                    },
                ],
            },
            {
                text: "Packages",
                items: [
                    { text: "mailgw-go", link: "/packages/mailgw-go" },
                    { text: "logservice", link: "/packages/logservice" },
                    { text: "webui-fastify", link: "/packages/webui-fastify" },
                    { text: "legacy/", link: "/packages/legacy" },
                ],
            },
            {
                text: "Working on it",
                items: [
                    { text: "Getting started", link: "/dev/getting-started" },
                    { text: "Testing", link: "/dev/testing" },
                    { text: "Conventions", link: "/dev/conventions" },
                    { text: "Release", link: "/dev/release" },
                ],
            },
            {
                text: "History",
                items: [
                    { text: "Milestones", link: "/history/milestones" },
                    {
                        text: "Known gaps",
                        link: "/history/known-gaps",
                    },
                ],
            },
        ],

        outline: { level: [2, 3] },
        search: { provider: "local" },
        footer: {
            message: "Internal engineering documentation",
            copyright: "mailgw",
        },
    },
});
