/**
 * Locating the engineering binary.
 *
 * Tier B drives `cmd/mailgw-go-test` as a real OS process, so it needs a real
 * executable. There are two ways to get one and they are ordered by which is
 * more likely to be right:
 *
 *   1. MAILGW_GO_TEST_BIN, used verbatim. This is what CI sets after building
 *      once, and what a developer sets to test a binary they built by hand.
 *   2. `go build`, run once per `bun test` invocation.
 *
 * There is deliberately no staleness check. Go's own build cache makes a no-op
 * rebuild take a couple of hundred milliseconds, and a hand-rolled mtime
 * comparison is exactly the kind of cleverness that silently tests yesterday's
 * binary.
 */

import { mkdir, rename } from "node:fs/promises";
import path from "node:path";

/** The repo root, derived from this file rather than from process.cwd(). */
export const REPO_ROOT = path.resolve(import.meta.dir, "..", "..");
export const GO_MODULE = path.join(REPO_ROOT, "mailgw-go");

/**
 * Set MAILGW_REQUIRE_TIER_B=1 to turn "no Go toolchain" from a skip into a
 * failure.
 *
 * CI sets it. A tier that can silently skip itself in CI is a tier that will
 * one day be skipped entirely, and nobody will notice until it matters.
 */
export const REQUIRED = process.env.MAILGW_REQUIRE_TIER_B === "1";

/** Memoised for the process: Bun runs every test file in one process. */
let building: Promise<string> | null = null;

export function binaryPath(): Promise<string> {
    building ??= build();
    return building;
}

/**
 * True when a gateway can be started at all, for `describe.skipIf`.
 *
 * Resolves rather than throws so a suite can skip cleanly, unless
 * MAILGW_REQUIRE_TIER_B says otherwise.
 */
export async function haveBinary(): Promise<boolean> {
    try {
        await binaryPath();
        return true;
    } catch (e) {
        if (REQUIRED) throw e;
        console.error(
            `\n[tier-b] skipping: ${(e as Error).message}\n` +
                "  Build it once and re-run, or set MAILGW_GO_TEST_BIN:\n" +
                "    pnpm build:mailgw-go:test:bin\n",
        );
        return false;
    }
}

async function build(): Promise<string> {
    const preset = process.env.MAILGW_GO_TEST_BIN;
    if (preset) {
        if (!(await Bun.file(preset).exists())) {
            throw new Error(`MAILGW_GO_TEST_BIN=${preset} does not exist`);
        }
        return preset;
    }

    const outDir = path.join(GO_MODULE, "bin");
    await mkdir(outDir, { recursive: true });

    // Built to a pid-suffixed name and renamed into place, so two `bun test`
    // shells against one checkout cannot write the same path at the same time.
    const final = path.join(outDir, "mailgw-go-test");
    const tmp = `${final}.${process.pid}`;

    const proc = Bun.spawn(["go", "build", "-trimpath", "-o", tmp, "./cmd/mailgw-go-test"], {
        cwd: GO_MODULE,
        stdout: "pipe",
        stderr: "pipe",
    });
    const [code, stderr] = await Promise.all([proc.exited, new Response(proc.stderr).text()]);
    if (code !== 0) {
        // The compiler's own output, verbatim: a compile error in the gateway
        // must read as a compile error, not as "the gateway would not start".
        throw new Error(`go build ./cmd/mailgw-go-test failed (exit ${code}):\n${stderr}`);
    }

    await rename(tmp, final);
    return final;
}
