/**
 * The configuration the compose stack runs.
 *
 * One definition, imported by tests/provision.ts and by the Tier-A suites, so a
 * test cannot assert against a configuration the provisioning script does not
 * actually deploy.
 *
 * # It is NOT the same as tests/harness/bundle.ts's baseline
 *
 * The divergence is one key and it is deliberate: `outbound.spool_dir` is pinned
 * to /opt/mailgw-go/queue here because that is the path the compose file
 * bind-mounts, and omitted in the Tier-B baseline because a host process must
 * get a per-instance directory from node.SpoolFallback. Sharing one constant
 * would give whichever tier lost the argument an unwritable spool and a gateway
 * that fails its first apply.
 */

export const CONSOLE_URL = process.env.CONSOLE_URL ?? "https://localhost:4000";
export const CONSOLE_EMAIL = process.env.CONSOLE_EMAIL ?? "admin@lab.example";
export const CONSOLE_PASSWORD = process.env.CONSOLE_PASSWORD ?? "labpassword1";

/** The engineering build's control API. Probed, never required. */
export const TESTCTL_URL = process.env.TESTCTL_URL ?? "http://127.0.0.1:9090";

/**
 * The console as the GATEWAY sees it, which is not the one a test uses: the
 * gateway dials from inside the compose network, where "localhost" is itself.
 */
export const GATEWAY_CENTRAL_URL = process.env.GATEWAY_CENTRAL_URL ?? "https://webui:4000";

export const SMTP_PORT = Number(process.env.SMTP_PORT ?? "2525");
export const RELAY_HOST = process.env.RELAY_HOST ?? "mailhog";
export const RELAY_PORT = process.env.RELAY_PORT ?? "1025";

/** MailHog's HTTP API, for asserting that mail actually arrived. */
export const MAILHOG_URL = process.env.MAILHOG_URL ?? "http://127.0.0.1:8025";

export const PROFILE_RULESET = "e2e-rules";
export const PROFILE_ALLOWLIST = "e2e-allowlist";
export const PROFILE_SERVER = "e2e-server";
export const RELAY_GROUP = "Outbound";

export const RULESET = `version: 1
routes:
    - name: Default
      match: {always: true}
      then: [{action: relay, relay: ${RELAY_GROUP}}]
`;

/**
 * allow_all, because the gateway sees connections from the docker bridge and
 * from the host, and pinning those addresses would make the suite depend on the
 * runtime's networking. On a real node this would be a specific list.
 */
export const ALLOWLIST = `{"allowed": [], "allow_all": true}`;

export const SERVER = `hostname: devbook.local
local_domains: [devbook.local, ngm.dev]
listen:
    - addr: "0.0.0.0:2525"
outbound:
    spool_dir: /opt/mailgw-go/queue
`;
