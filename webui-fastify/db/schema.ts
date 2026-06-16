import {
    mysqlTable,
    int,
    varchar,
    text,
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
    ...timestamps,
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
    auth_pass: varchar("auth_pass", { length: 255 }),
    priority: int("priority"),
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
