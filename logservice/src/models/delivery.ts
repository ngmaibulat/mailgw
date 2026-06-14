import db from "../db";
import type { Delivery } from "../validation/delivery";

export async function insertDelivery(d: Delivery): Promise<void> {
    await db`
        INSERT INTO Delivery
            (uuid, dt, sender, rcpt_domain, rcpt_list, rcpt_accepted,
             tls_forced, tls, auth, host, ip, port, response, delay,
             createdAt, updatedAt)
        VALUES
            (${d.uuid}, FROM_UNIXTIME(${d.dt} / 1000), ${d.sender}, ${d.rcpt_domain}, ${d.rcpt_list},
             ${d.rcpt_accepted}, ${d.tls_forced}, ${d.tls}, ${d.auth},
             ${d.host}, ${d.ip}, ${d.port}, ${d.response}, ${d.delay},
             NOW(), NOW())
    `;
}
