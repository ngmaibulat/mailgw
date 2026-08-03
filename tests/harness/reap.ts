/**
 * Killing gateways the suite forgot.
 *
 * Bun's child processes usually die with their parent. "Usually" is not a
 * teardown: a test file that throws outside afterAll, or a suite killed with
 * ^C, leaks a gateway that holds a temp directory and three sockets, and the
 * next run inherits the mess.
 */

const alive = new Set<{ kill: (sig?: number | NodeJS.Signals) => void }>();
let installed = false;

export function track(proc: { kill: (sig?: number | NodeJS.Signals) => void }): void {
    install();
    alive.add(proc);
}

export function untrack(proc: { kill: (sig?: number | NodeJS.Signals) => void }): void {
    alive.delete(proc);
}

function install(): void {
    if (installed) return;
    installed = true;

    const reap = () => {
        for (const proc of alive) {
            try {
                proc.kill("SIGKILL");
            } catch {
                // Already gone, which is the outcome we wanted.
            }
        }
        alive.clear();
    };

    process.on("exit", reap);
    // The signal handlers re-raise so the exit code still says "interrupted".
    for (const sig of ["SIGINT", "SIGTERM"] as const) {
        process.on(sig, () => {
            reap();
            process.exit(sig === "SIGINT" ? 130 : 143);
        });
    }
}
