import {
    mysqlTable,
    int,
    bigint,
    boolean,
    char,
    varchar,
    text,
    longtext,
    datetime,
} from "drizzle-orm/mysql-core";

// The webui does NOT own or migrate this schema — logservice's SQL migrations
// create these tables. This file just describes the columns Drizzle queries,
// matching those migrations exactly. Do not add DDL/migration tooling here.

// Sequelize used to auto-manage createdAt/updatedAt. The columns are NOT NULL
// with no DB default, so we fill them app-side: $defaultFn on insert,
// $onUpdate on update — the faithful Drizzle equivalent.
const timestamps = {
    createdAt: datetime("createdAt", { mode: "date" })
        .notNull()
        .$defaultFn(() => new Date()),
    updatedAt: datetime("updatedAt", { mode: "date" })
        .notNull()
        .$defaultFn(() => new Date())
        .$onUpdate(() => new Date()),
};

export const users = mysqlTable("Users", {
    id: int("id").autoincrement().primaryKey(),
    email: varchar("email", { length: 255 }).notNull().unique(),
    hash: varchar("hash", { length: 255 }),
    // "admin" | "viewer" (migration 020). Only an admin may approve gateways,
    // deploy configuration or manage users.
    role: varchar("role", { length: 32 }).notNull().default("viewer"),
    ...timestamps,
});

// Sessions moved out of process memory in migration 021 so a restart no longer
// logs every operator out and the console can run more than one replica.
// `id` is the uuid carried by the signed `session` cookie.
export const sessions = mysqlTable("Sessions", {
    id: char("id", { length: 36 }).primaryKey(),
    email: varchar("email", { length: 255 }).notNull(),
    expiresAt: datetime("expiresAt", { mode: "date" }).notNull(),
    createdAt: datetime("createdAt", { mode: "date" })
        .notNull()
        .$defaultFn(() => new Date()),
});

export const relayGroups = mysqlTable("RelayGroups", {
    id: int("id").autoincrement().primaryKey(),
    name: varchar("name", { length: 255 }),
    description: varchar("description", { length: 255 }),
    ...timestamps,
});

export const relays = mysqlTable("Relays", {
    id: int("id").autoincrement().primaryKey(),
    // Keys kept snake_case to match the DB columns, the pug form field names,
    // and `request.body`, so controllers stay drop-in.
    group_id: int("group_id"),
    name: varchar("name", { length: 255 }),
    host: varchar("host", { length: 255 }),
    port: int("port"),
    auth_user: varchar("auth_user", { length: 255 }),
    // AES-256-GCM ciphertext with a "v1:" prefix (src/central/secrets.ts).
    // Rows written before migration 022 are plaintext and read as such.
    auth_pass: text("auth_pass"),
    // Name of an environment variable ON THE GATEWAY holding the password.
    // When set it wins, and the credential never enters the deployed bundle.
    auth_pass_env: varchar("auth_pass_env", { length: 255 }),
    // Outbound TLS for this leg: none | opportunistic | required. NULL means
    // opportunistic, matching mailgw-go's Relay.TLSPolicy() default.
    tls: varchar("tls", { length: 16 }),
    allow_insecure_auth: boolean("allow_insecure_auth"),
    // Treat `host` as a DOMAIN and resolve its MX records at delivery time,
    // trying them in preference order. This is "smarthost named by domain", not
    // direct-to-MX: the recipient's own domain is never consulted.
    use_mx: boolean("use_mx"),
    priority: int("priority"),
    ...timestamps,
});

// Inbound SMTP AUTH credentials, assigned to a gateway as a set. See migration
// 026.
export const credentialSets = mysqlTable("CredentialSets", {
    id: int("id").autoincrement().primaryKey(),
    name: varchar("name", { length: 255 }).notNull(),
    description: varchar("description", { length: 255 }),
    ...timestamps,
});

export const smtpCredentials = mysqlTable("SmtpCredentials", {
    id: int("id").autoincrement().primaryKey(),
    set_id: int("set_id").notNull(),
    username: varchar("username", { length: 255 }).notNull(),
    // bcrypt, cost 10 — NOT ciphertext, and deliberately nothing to do with
    // src/central/secrets.ts. The gateway only ever verifies an inbound
    // credential, so it never needs the plaintext back and no key exists that
    // a leaked bundle could be decrypted with. Contrast Relays.auth_pass,
    // which this console has to be able to present to somebody else.
    hash: varchar("hash", { length: 255 }).notNull(),
    ...timestamps,
});

export const logs = mysqlTable("Logs", {
    id: int("id").autoincrement().primaryKey(),
    url: varchar("url", { length: 2048 }),
    path: varchar("path", { length: 255 }),
    query: varchar("query", { length: 255 }),
    src_ip: varchar("src_ip", { length: 255 }),
    src_port: int("src_port"),
    referer: varchar("referer", { length: 255 }),
    origin: varchar("origin", { length: 255 }),
    method: varchar("method", { length: 255 }),
    user: varchar("user", { length: 255 }),
    userAgent: varchar("userAgent", { length: 255 }),
    ...timestamps,
});

export const exceptions = mysqlTable("Exceptions", {
    id: int("id").autoincrement().primaryKey(),
    product: varchar("product", { length: 255 }),
    component: varchar("component", { length: 255 }),
    info: text("info"),
    ...timestamps,
});

// ---------------------------------------------------------------------------
// Central Management (migrations 016–019)
// ---------------------------------------------------------------------------

// A registered mailgw-go instance. Registration is open — anything that can
// reach us may ask — so a new row is always `pending` until an operator
// approves its fingerprint. See migration 016.
export const gateways = mysqlTable("Gateways", {
    id: int("id").autoincrement().primaryKey(),
    gateway_uid: char("gateway_uid", { length: 36 }).notNull().unique(),
    name: varchar("name", { length: 255 }),
    fingerprint: varchar("fingerprint", { length: 128 }).notNull().unique(),
    pubkey: varchar("pubkey", { length: 255 }).notNull(),
    // "pending" | "approved" | "rejected" | "revoked"
    status: varchar("status", { length: 16 }).notNull().default("pending"),

    // Self-declared systeminfo — display only, never a basis for a decision.
    hostname: varchar("hostname", { length: 255 }),
    os: varchar("os", { length: 64 }),
    arch: varchar("arch", { length: 64 }),
    cpus: int("cpus"),
    mem_bytes: bigint("mem_bytes", { mode: "number" }),
    ip_addrs: text("ip_addrs"),
    version: varchar("version", { length: 64 }),

    first_seen: datetime("first_seen", { mode: "date" }),
    last_seen: datetime("last_seen", { mode: "date" }),
    approved_by: varchar("approved_by", { length: 255 }),
    approved_at: datetime("approved_at", { mode: "date" }),

    desired_version_id: int("desired_version_id"),
    applied_version_id: int("applied_version_id"),
    apply_error: text("apply_error"),
    restart_required: boolean("restart_required").notNull().default(false),

    ...timestamps,
});

// Raw config text, reusable across gateways. `kind` is
// "ruleset" (routing.yaml) | "allowlist" (ngmfilter.json) | "server"
// (server.yaml). Relay definitions are not profiles — they stay in
// RelayGroups/Relays. See migration 017.
export const configProfiles = mysqlTable("ConfigProfiles", {
    id: int("id").autoincrement().primaryKey(),
    kind: varchar("kind", { length: 32 }).notNull(),
    name: varchar("name", { length: 255 }).notNull(),
    description: varchar("description", { length: 255 }),
    body: longtext("body").notNull(),
    ...timestamps,
});

// Exactly one of profile_id / relay_group_id / credential_set_id is set,
// selected by `kind`. See migrations 018 and 026.
export const gatewayAssignments = mysqlTable("GatewayAssignments", {
    id: int("id").autoincrement().primaryKey(),
    gateway_id: int("gateway_id").notNull(),
    kind: varchar("kind", { length: 32 }).notNull(),
    profile_id: int("profile_id"),
    relay_group_id: int("relay_group_id"),
    credential_set_id: int("credential_set_id"),
    ...timestamps,
});

// Immutable composed bundles. Never updated — rollback repoints
// Gateways.desired_version_id at an older row. See migration 019.
export const configVersions = mysqlTable("ConfigVersions", {
    id: int("id").autoincrement().primaryKey(),
    gateway_id: int("gateway_id").notNull(),
    version: int("version").notNull(),
    bundle: longtext("bundle").notNull(),
    bundle_sha256: char("bundle_sha256", { length: 64 }).notNull(),
    note: varchar("note", { length: 255 }),
    created_by: varchar("created_by", { length: 255 }),
    createdAt: datetime("createdAt", { mode: "date" })
        .notNull()
        .$defaultFn(() => new Date()),
});

// Deploy/rollback audit trail. See migration 019.
export const configDeployments = mysqlTable("ConfigDeployments", {
    id: int("id").autoincrement().primaryKey(),
    gateway_id: int("gateway_id").notNull(),
    version_id: int("version_id").notNull(),
    // "deploy" | "rollback"
    action: varchar("action", { length: 16 }).notNull(),
    actor: varchar("actor", { length: 255 }),
    note: varchar("note", { length: 255 }),
    createdAt: datetime("createdAt", { mode: "date" })
        .notNull()
        .$defaultFn(() => new Date()),
});

// The latest counter snapshot a gateway reported on its heartbeat — one row
// per gateway, keyed on the gateway (logservice migration 024).
//
// `metrics` holds the object mailgw-go sent, verbatim, because the counter set
// lives in that binary (internal/obs) and a fleet routinely runs mixed
// versions. Read the keys you know and ignore the rest.
export const gatewayMetrics = mysqlTable("GatewayMetrics", {
    gateway_id: int("gateway_id").notNull().primaryKey(),
    metrics: text("metrics").notNull(),
    updated_at: datetime("updated_at", { mode: "date" })
        .notNull()
        .$defaultFn(() => new Date()),
});
