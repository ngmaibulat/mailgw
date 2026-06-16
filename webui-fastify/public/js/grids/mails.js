// Mails grid (Transaction table via /api/queue).
LogGrid.create({
    url: "/api/queue",
    columns: [
        LogGrid.dt("dt", "DT"),
        LogGrid.text("sender", "Sender"),
        LogGrid.text("rcpt_list", "Rcpt"),
        LogGrid.num("rcpt_count_accept", "Rcpt Accepted", { maxWidth: 150 }),
        LogGrid.num("data_bytes", "Data Bytes", { maxWidth: 140 }),
        // Hidden by default — toggle from the column menu.
        LogGrid.text("uuid", "UUID", { hide: true, minWidth: 200 }),
        LogGrid.text("encoding", "Encoding", { hide: true }),
        LogGrid.num("rcpt_count_tempfail", "Rcpt Tempfail", { hide: true }),
        LogGrid.num("rcpt_count_reject", "Rcpt Rejected", { hide: true }),
        LogGrid.num("delay_data_post", "Delay", { hide: true }),
        LogGrid.num("mime_part_count", "Mime Parts", { hide: true }),
    ],
});
