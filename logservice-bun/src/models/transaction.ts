import db from "../db";

export interface TransactionRow {
    uuid: string;
    dt: number;
    action: string | null;
    encoding: string | null;
    sender: string;
    rcpt_list: string;
    rcpt_count_accept: number;
    rcpt_count_tempfail: number;
    rcpt_count_reject: number;
    delay_data_post: number | null;
    data_bytes: number | null;
    mime_part_count: number | null;
}

export async function insertTransaction(t: TransactionRow): Promise<number> {
    const rows = await db`
        INSERT INTO Transaction
            (uuid, dt, action, encoding, sender, rcpt_list,
             rcpt_count_accept, rcpt_count_tempfail, rcpt_count_reject,
             delay_data_post, data_bytes, mime_part_count, createdAt, updatedAt)
        VALUES
            (${t.uuid}, ${t.dt}, ${t.action}, ${t.encoding}, ${t.sender}, ${t.rcpt_list},
             ${t.rcpt_count_accept}, ${t.rcpt_count_tempfail}, ${t.rcpt_count_reject},
             ${t.delay_data_post}, ${t.data_bytes}, ${t.mime_part_count},
             NOW(), NOW())
    ` as any;
    return rows.insertId;
}
