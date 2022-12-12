'use strict';

const path = require('path');
const functions = require("./functions");
const AttachChecker = require("../plugins/AttachChecker");


const url_conn = "http://localhost:3000/api/connection";
const url_queue = "http://localhost:3000/api/queue";
const url_filter = "http://localhost:3000/filter/md5";


exports.register = function()
{
    const plugin = this;
};


exports.get_tmp_file = function (transaction)
{
    const plugin = this;
    const tmpdir  = plugin?.cfg?.main?.tmpdir || '/tmp';
    return path.join(tmpdir, `${transaction.uuid}.tmp`);
}

exports.hook_data_post = async function (next, connection)
{
    const plugin = this;

    if (!connection?.transaction) {
        return next();
    }

    let uuid = connection.transaction.uuid;

    const url = url_filter;
    const checker = new AttachChecker(url, uuid);
    
    let result = null;
    await checker.check(connection.transaction.message_stream).then(res => result = res);

    connection.transaction.action = result;
    functions.log_connection(connection, url_conn);
    functions.log_transaction(connection.transaction, url_queue);

    if (result == "allow") {
        return next();
    }
    else {
        return next(DENYSOFT, 'Blocked: Attach Scan');
    }
}
