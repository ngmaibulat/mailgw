import db from "../db";

export interface MailAddrRow {
    id: number;
    name: string | null;
    email: string;
}

export async function upsertMailAddr(email: string, name?: string): Promise<number> {
    await db`
        INSERT INTO MailAddrs (email, name, createdAt, updatedAt)
        VALUES (${email}, ${name ?? null}, NOW(), NOW())
        ON DUPLICATE KEY UPDATE updatedAt = NOW()
    `;
    const rows = await db`SELECT id FROM MailAddrs WHERE email = ${email} LIMIT 1` as MailAddrRow[];
    return rows[0].id;
}

export async function linkMailAddrToTransaction(mailAddrId: number, transactionId: number): Promise<void> {
    await db`
        INSERT INTO linkAddrTransactions
            (MailAddrId, TransactionId, createdAt, updatedAt)
        VALUES
            (${mailAddrId}, ${transactionId}, NOW(), NOW())
    `;
}
