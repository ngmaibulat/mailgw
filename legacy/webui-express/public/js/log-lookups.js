$(function () {
    $("#grid").w2grid({
        name: "grid",
        recid: "id",
        url: "/api/hashlookups",
        method: "GET", // need this to avoid 412 error on Safari
        multiSearch: true,

        show: {
            header: true,
            footer: true,
            toolbar: true,
            lineNumbers: true,
        },

        // dt: 2022-06-08T09:12:34.000Z,
        // txn_action: 'block',
        // encoding: 'utf8',
        // sender: 'sender@ngm.dev',
        // rcpt_list: 'rcpt@ngm.dev',
        // rcpt_count_accept: 1,
        // rcpt_count_tempfail: 0,
        // rcpt_count_reject: 0,
        // delay_data_post: 0,
        // data_bytes: 651562,
        // mime_part_count: 0,

        searches: [
            // { type: 'int',  field: 'recid', label: 'ID' },
            { type: "datetime", field: "createdAt", label: "DT" },
            { type: "text", field: "md5", label: "MD5" },
            { type: "int", field: "size", label: "Size" },

            { type: "text", field: "contentType", label: "Content Type" },
            { type: "text", field: "filename", label: "Filename" },
            { type: "text", field: "action", label: "Action" },
        ],

        columns: [
            { field: "createdAt", text: "DT" },
            { field: "sender", text: "Sender" },
            { field: "rcpt_list", text: "Rcpt" },

            { field: "md5", text: "MD5" },
            { field: "contentType", text: "contentType" },

            { field: "filename", text: "filename" },
            { field: "size", text: "size" },
            { field: "action", text: "action" },
        ],
    });

    // w2ui.grid.hideColumn('mime_part_count');
});
