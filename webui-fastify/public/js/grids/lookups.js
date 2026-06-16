// Hash lookups grid (HashLookups LEFT JOIN Transaction via /api/hashlookups).
// `sender`/`rcpt_list` come from the JOINed Transaction and are NOT in
// logservice's searchable HashLookups field allowlist, so they are display-only
// (no server-side sort/filter). The DT column is the lookup's own `createdAt`.
LogGrid.create({
    url: "/api/hashlookups",
    columns: [
        LogGrid.dt("createdAt", "DT"),
        LogGrid.display("sender", "Sender"),
        LogGrid.display("rcpt_list", "Rcpt"),
        LogGrid.text("md5", "MD5", { minWidth: 200 }),
        LogGrid.text("contentType", "Content Type"),
        LogGrid.text("filename", "Filename"),
        LogGrid.num("size", "Size", { maxWidth: 130 }),
        LogGrid.text("action", "Action", { maxWidth: 120 }),
    ],
});
