import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

import { and, eq } from "drizzle-orm";

import {
    db,
    closeDb,
    gateways,
    configProfiles,
    gatewayAssignments,
} from "../db/index.ts";

// The one test that proves MariaDB actually rolls back.
//
// src/central.tx.test.ts asserts our handlers route their writes through the
// transaction handle — which is the regression that recurs — but it stubs the
// database, so the rollback itself is simulated. This runs the same sequence
// against a real server.
//
// Opt-in via MAILGW_DB_CHECK, following the convention in tests/README.md:
// `node --test` has to stay runnable without a database.
//
//   docker compose up -d mariadb db-migrator
//   MAILGW_DB_CHECK=1 node --test tests/central.tx.db.test.ts
//
// It also catches a failure mode no stub can: GatewayAssignments being MyISAM
// rather than InnoDB, which would make every one of these transactions a
// silent no-op. Migration 018 has no ENGINE clause and relies on the server
// default.

const enabled = process.env.MAILGW_DB_CHECK === "1";

describe("central management writes roll back for real", { skip: !enabled }, () => {
    // A uid no other test or fixture would collide with.
    const uid = "tx-test-0000-4000-8000-000000000001";
    let gatewayId = 0;
    let profileId = 0;

    before(async () => {
        await db.delete(gateways).where(eq(gateways.gateway_uid, uid));

        await db.insert(gateways).values({
            gateway_uid: uid,
            name: "tx-test",
            fingerprint: `fp-${uid}`,
            pubkey: "AAAA",
            status: "pending",
        });
        const [row] = await db
            .select({ id: gateways.id })
            .from(gateways)
            .where(eq(gateways.gateway_uid, uid))
            .limit(1);
        gatewayId = row.id;

        await db.insert(configProfiles).values({
            name: `tx-test-ruleset-${uid}`,
            kind: "ruleset",
            body: "version: 1\n",
        });
        const [profile] = await db
            .select({ id: configProfiles.id })
            .from(configProfiles)
            .where(eq(configProfiles.name, `tx-test-ruleset-${uid}`))
            .limit(1);
        profileId = profile.id;

        await db.insert(gatewayAssignments).values({
            gateway_id: gatewayId,
            kind: "ruleset",
            profile_id: profileId,
        });
    });

    after(async () => {
        if (gatewayId) {
            await db
                .delete(gatewayAssignments)
                .where(eq(gatewayAssignments.gateway_id, gatewayId));
            await db.delete(gateways).where(eq(gateways.id, gatewayId));
        }
        if (profileId) {
            await db
                .delete(configProfiles)
                .where(eq(configProfiles.id, profileId));
        }
        await closeDb();
    });

    it("uses a transactional storage engine for GatewayAssignments", async () => {
        // A MyISAM table would make every transaction here a silent no-op, so
        // the rest of this file would pass while proving nothing.
        const [row] = (await db.execute(
            "SHOW TABLE STATUS LIKE 'GatewayAssignments'",
        )) as unknown as [{ Engine?: string }[]];
        assert.equal(
            row?.[0]?.Engine,
            "InnoDB",
            "GatewayAssignments is not InnoDB; transactions would be silently ignored",
        );
    });

    it("restores the assignment set when the replace fails part-way", async () => {
        const before = await db
            .select()
            .from(gatewayAssignments)
            .where(eq(gatewayAssignments.gateway_id, gatewayId));
        assert.equal(before.length, 1, "fixture did not take");

        // The exact shape of CtrlGateway.saveAssignments: delete everything,
        // then re-insert. Fail after the delete.
        await assert.rejects(
            db.transaction(async (tx) => {
                await tx
                    .delete(gatewayAssignments)
                    .where(eq(gatewayAssignments.gateway_id, gatewayId));
                throw new Error("simulated failure after the delete");
            }),
            /simulated failure/,
        );

        const after = await db
            .select()
            .from(gatewayAssignments)
            .where(eq(gatewayAssignments.gateway_id, gatewayId));

        assert.equal(
            after.length,
            1,
            "the delete was not rolled back — the gateway was left with no assignments",
        );
        assert.equal(
            after[0].profile_id,
            profileId,
            "the restored assignment is not the original one",
        );
    });

    it("commits the replace when nothing fails", async () => {
        await db.transaction(async (tx) => {
            await tx
                .delete(gatewayAssignments)
                .where(eq(gatewayAssignments.gateway_id, gatewayId));
            await tx.insert(gatewayAssignments).values([
                { gateway_id: gatewayId, kind: "ruleset", profile_id: profileId },
                { gateway_id: gatewayId, kind: "relaygroup", relay_group_id: 1 },
            ]);
        });

        const rows = await db
            .select()
            .from(gatewayAssignments)
            .where(eq(gatewayAssignments.gateway_id, gatewayId));
        assert.equal(rows.length, 2, "the committed replace did not take");

        // Leave the fixture as `before` created it.
        await db
            .delete(gatewayAssignments)
            .where(
                and(
                    eq(gatewayAssignments.gateway_id, gatewayId),
                    eq(gatewayAssignments.kind, "relaygroup"),
                ),
            );
    });
});
