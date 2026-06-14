const fs = require("fs");

const VERSION = "0.0.21";

// Build the common headers for logservice requests, attaching the API key
// (matching the logservice X-API-Key auth) only when API_KEY is configured.
exports.apiHeaders = function () {
    const headers = {
        Accept: "application/json",
        "Content-Type": "application/json",
    };
    if (process.env.API_KEY) {
        headers["X-API-Key"] = process.env.API_KEY;
    }
    return headers;
};

exports.httplog = function (obj, url) {
    let jsondata = JSON.stringify(obj);
    // let jsondata = JSON.stringify(obj, censor(obj));

    let req = {
        method: "POST",
        headers: exports.apiHeaders(),
        body: jsondata,
    };

    return fetch(url, req).catch((err) => {
        exports.log(err.toString());
    });
};

// Shape the shared per-connection log payload used by the connection / data /
// queue logging hooks. Optional chaining guards against fields that may not
// exist yet at the SMTP stage the hook fires (e.g. hello before EHLO).
exports.buildConnInfo = function (connection) {
    return {
        uuid: connection.uuid,
        dt: connection.start_time,
        state: connection.state,
        encoding: connection.encoding,
        remoteAddr: connection.client?.remoteAddress,
        remotePort: connection.client?.remotePort,
        remote_is_local: connection.remote_is_local,
        remote_is_private: connection.remote_is_private,
        remote_host: connection.remote_host,
        remote_info: connection.remote_info,
        hello_name: connection.hello?.host,
        using_tls: connection.using_tls,
        tran_count: connection.tran_count,
        pipelining: connection.pipelining,
        rcpt_count_accept: connection.rcpt_count?.accept,
        rcpt_count_tempfail: connection.rcpt_count?.tempfail,
        rcpt_count_reject: connection.rcpt_count?.reject,
    };
};

// Shape the per-transaction log payload used by the queue logging hook. Maps
// onto the logservice TransactionRow. Optional chaining/getAddr guards against
// fields that may be absent. dt (data_post_start) is epoch ms; the logservice
// converts it via FROM_UNIXTIME(dt/1000).
exports.buildTxnInfo = function (connection) {
    const txn = connection.transaction;
    if (!txn) return null;

    return {
        uuid: txn.uuid,
        dt: txn.data_post_start,
        action: txn.action,
        encoding: txn.encoding,
        sender: exports.getAddr(txn.mail_from),
        rcpt_list: exports.getAddrList(txn.rcpt_to || []),
        rcpt_count_accept: txn.rcpt_count?.accept,
        rcpt_count_tempfail: txn.rcpt_count?.tempfail,
        rcpt_count_reject: txn.rcpt_count?.reject,
        delay_data_post: txn.data_post_delay,
        data_bytes: txn.data_bytes,
        mime_part_count: txn.mime_part_count,
    };
};

// POST a payload to the logservice and record the outcome to a local logfile.
// Returns the fetch promise so callers/tests can await it.
exports.postWithLogging = function (payload, url, logfile) {
    exports.log(exports.toJson(payload), logfile);

    return exports
        .httplog(payload, url)
        .then((response) => {
            const tm = new Date().toISOString();
            if (response && response.status) {
                exports.log(
                    JSON.stringify({
                        tm,
                        version: VERSION,
                        status: response.status,
                        statusText: response.statusText,
                    }),
                    logfile
                );
            } else {
                exports.log(
                    JSON.stringify({
                        tm,
                        version: VERSION,
                        error: "HTTP Logfail, please review logs on Logger side",
                        logdata: payload,
                    }),
                    logfile
                );
            }
        })
        .catch((error) => {
            const tm = new Date().toISOString();
            exports.log(
                JSON.stringify({
                    tm,
                    version: VERSION,
                    error: "HTTP Connect Error",
                    httperror: error,
                    logdata: payload,
                }),
                logfile
            );
        });
};

exports.getAddr = function (addr) {
    // A null sender (MAIL FROM:<>) or absent address must not crash callers.
    if (!addr) return "";
    return (addr.user || "") + "@" + (addr.host || "");
};

exports.getAddrList = function (arr) {
    let res = "";
    arr.forEach((addr) => {
        if (!res) {
            res += exports.getAddr(addr);
        } else {
            res += "," + exports.getAddr(addr);
        }
    });

    return res;
};

exports.log = function (msg, logfile = "./log/error.log") {
    fs.appendFile(logfile, msg + "\n", (err) => {
        if (err) {
        }
        //file written successfully
    });
};

exports.log_transaction = function (txn, url) {
    let obj;

    if (!txn) {
        module.exports.httplog({ status: "empty" }, url);
        return;
    }

    obj = {
        uuid: txn.uuid,
        dt: txn.data_post_start,
        action: txn.action,
        delay_data_post: txn.data_post_delay,

        encoding: txn.encoding,
        data_bytes: txn.data_bytes,
        mime_part_count: txn.mime_part_count,

        sender: module.exports.getAddr(txn.mail_from),
        // rcpt_list: module.exports.getAddrList(txn.rcpt_to),
        rcpt_to: txn.rcpt_to,
        // rawHeaders: txn.header_lines,

        // config items:
        // parse_body: txn.parse_body,
        // notes: txn.notes,

        rcpt_count_accept: txn.rcpt_count?.accept,
        rcpt_count_tempfail: txn.rcpt_count?.tempfail,
        rcpt_count_reject: txn.rcpt_count?.reject,

        headers: txn.header?.headers_decoded,
    };

    module.exports.httplog(obj, url);
};

exports.log_connection = function (connection, url) {
    const obj = exports.buildConnInfo(connection);
    exports.httplog(obj, url);
};

function censor(censor) {
    var i = 0;

    return function (key, value) {
        if (
            i !== 0 &&
            typeof censor === "object" &&
            typeof value == "object" &&
            censor == value
        )
            return "[Circular]";

        if (i >= 29)
            // seems to be a harded maximum of 30 serialized objects?
            return "[Unknown]";

        ++i; // so we know we aren't using the original object anymore

        return value;
    };
}

function jsonlog(obj) {
    let str = JSON.stringify(obj, censor(obj));
    log(str);
    return str;
}

exports.toJson = function (obj) {
    let str = JSON.stringify(obj, censor(obj));
    return str;
};

function getDomain(addr) {
    let domain = addr.substring(addr.lastIndexOf("@") + 1);
    return domain;
}

/////////////////////////////
