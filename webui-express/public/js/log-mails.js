$(function () {
    $("#grid").w2grid({
        name: "grid",
        recid: "id",
        url: "/api/queue",
        method: "GET", // need this to avoid 412 error on Safari
        multiSearch: true,

        show: {
            header: true,
            footer: true,
            toolbar: true,
            lineNumbers: true,
        },

        searches: [
            // { type: 'int',  field: 'recid', label: 'ID' },
            { type: "datetime", field: "dt", label: "DT" },
            { type: "text", field: "uuid", label: "UUID" },
            { type: "text", field: "encoding", label: "Encoding" },
            { type: "text", field: "sender", label: "Sender" },
            { type: "text", field: "rcpt_list", label: "Recipients" },

            {
                type: "int",
                field: "rcpt_count_accept",
                label: "Count Accepted",
            },
            {
                type: "int",
                field: "rcpt_count_tempfail",
                label: "Count Tempfail",
            },
            {
                type: "int",
                field: "rcpt_count_reject",
                label: "Count Rejected",
            },

            { type: "float", field: "delay_data_post", label: "Delay" },
            { type: "int", field: "data_bytes", label: "Size" },
            {
                type: "int",
                field: "mime_part_count",
                label: "Count Mime Parts",
            },
        ],

        columns: [
            { field: "dt", text: "DT" },
            { field: "uuid", text: "UUID" },
            { field: "encoding", text: "Encoding" },

            { field: "sender", text: "Sender" },
            { field: "rcpt_list", text: "Rcpt" },

            { field: "rcpt_count_accept", text: "Rcpt Accepted" },
            { field: "rcpt_count_tempfail", text: "Rcpt Templ Fail" },
            { field: "rcpt_count_reject", text: "Rcpt Rejected" },

            { field: "delay_data_post", text: "Delay" },
            { field: "data_bytes", text: "Data Bytes" },
            { field: "mime_part_count", text: "Mime Part Count" },
        ],
    });

    w2ui.grid.hideColumn("uuid");
    w2ui.grid.hideColumn("encoding");

    w2ui.grid.hideColumn("rcpt_count_tempfail");
    w2ui.grid.hideColumn("rcpt_count_reject");

    w2ui.grid.hideColumn("delay_data_post");
    // w2ui.grid.hideColumn('data_bytes');
    w2ui.grid.hideColumn("mime_part_count");
});
