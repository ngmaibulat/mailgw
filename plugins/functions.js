const fs = require('fs');

exports.httplog = function (obj, url)
{
    let jsondata = JSON.stringify(obj);
    // let jsondata = JSON.stringify(obj, censor(obj));

    let req = {
        method: 'POST',
        headers: {
            'Accept': 'application/json',
            'Content-Type': 'application/json'
        },
        body: jsondata
    };

    return fetch(url, req).catch((err) => {});
}


exports.getAddr = function (addr)
{
    let res = addr.user + "@" + addr.host;
    return res;
}

exports.getAddrList = function(arr)
{
    let res = ""
    arr.forEach(addr =>{
        if (!res) {
            res += exports.getAddr(addr);
        }
        else {
            res += "," + exports.getAddr(addr);
        }
    });

    return res;
}


exports.log = function (msg, logfile = '/tmp/haraka/haraka.log')
{
    fs.appendFile(logfile, msg + "\n", err => {
        if (err) {
        }
        //file written successfully
    });    
}
  

exports.log_transaction = function(txn, url)
{
    let obj;

    if (!txn) {
        module.exports.httplog({status: "empty"}, url);
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

        rcpt_count_accept: txn.rcpt_count.accept,
        rcpt_count_tempfail: txn.rcpt_count.tempfail,
        rcpt_count_reject: txn.rcpt_count.reject,

        headers: txn.header.headers_decoded,
    };

    module.exports.httplog(obj, url);
}

exports.log_connection = function (connection, url)
{
    let obj = {
        uuid: connection.uuid,
        dt: connection.start_time,

        // always should be == 5 for hook_data
        // state: connection.state,

        encoding: connection.encoding,
        remoteAddr: connection.client.remoteAddress,
        remotePort: connection.client.remotePort,

        remote_is_local: connection.remote_is_local,
        remote_is_private: connection.remote_is_private,
        // remote_ip: connection.remote_ip,
        // remote_port: connection.remote_port,
        remote_host: connection.remote_host,
        remote_info: connection.remote_info,

        // cfg: connection.cfg,
        // local: connection.local,
        // remote: connection.remote,

        // hello: connection.hello,
        hello_name: connection.hello.host,
        // tls: connection.tls,
        // proxy: connection.proxy,
        using_tls: connection.using_tls,

        /////////////////////////////

        // notes: connection.notes,

        //this one fails for json encoding or being empty
        // transaction: connection.transaction,

        tran_count: connection.tran_count,

        // our config - no need to log
        // ehlo_hello_message: connection.ehlo_hello_message,

        // our config - no need to log
        // connection_close_message: connection.connection_close_message,

        // our config - no need to log
        // banner_includes_uuid: connection.banner_includes_uuid,
        // deny_includes_uuid: connection.deny_includes_uuid,

        // not sure what it is:
        // early_talker: connection.early_talker,
        // pipelining: connection.pipelining,

        // not really usefull info:
        // esmtp: connection.esmtp,
 

        rcpt_count_accept: connection.rcpt_count.accept,
        rcpt_count_tempfail: connection.rcpt_count.tempfail,
        rcpt_count_reject: connection.rcpt_count.reject,

        //not yet having this info or can be misleading:
        // msg_count: connection.msg_count,

        // ignore as of now:
        // errors: connection.errors,
        // last_rcpt_msg: connection.last_rcpt_msg,
    };

    //prepare logging object from connection object!
    module.exports.httplog(obj, url);
}



function censor(censor) {
    var i = 0;
    
    return function(key, value) {
      if(i !== 0 && typeof(censor) === 'object' && typeof(value) == 'object' && censor == value) 
        return '[Circular]'; 
      
      if(i >= 29) // seems to be a harded maximum of 30 serialized objects?
        return '[Unknown]';
      
      ++i; // so we know we aren't using the original object anymore
      
      return value;  
    }
}


function jsonlog(obj)
{
    let str = JSON.stringify(obj, censor(obj));
    log(str);
    return str;
}


function getDomain(addr)
{
    let domain = addr.substring(addr.lastIndexOf('@') + 1);
    return domain;
}


/////////////////////////////
