const fs = require('fs');

const logfile = '/tmp/haraka/haraka-conn.log';


/*

https://haraka.github.io/core/Connection/

connection.remote:

{
    "ip":"127.0.0.1",
    "port":54279,
    "host":"Unknown",
    "info":"Unknown",
    "closed":false,
    "is_private":true,
    "is_local":true
}

connection.local:

{
    "ip":"127.0.0.1",
    "port":25,
    "host":"devbook.local",
    "info":"Haraka/2.8.28"
}

connection.transaction: would not be available till MAIL FROM

cfg: that is whatever json you put in config/ngmipfilter

{
    "accesskeyid": "access key id",
    "allowed": ["127.0.0.1", "10.0.0.10"]
}

hook_rcpt.params:

[{"original":"<addr@ngm.com>","original_host":"ngm.com","host":"ngm.com","user":"addr"},{}]

connection.transaction.mail_from

{"original":"<addr@test.com>","original_host":"test.com","host":"test.com","user":"addr"}

*/

exports.hook_connect = function (next, connection)
{
    const cfg = this.config.get('ngmfilter.json', 'json');

    // let data = JSON.stringify(cfg.allowed) + "\n\n\n";
    // log(data);

    let allowed = cfg.allowed.includes(connection.remote.ip);

    if (allowed) {
        connection.relaying = true;
        let msg = `${connection.uuid}: allow: ${connection.remote.ip}`;
        log(msg);
        return next(OK);
    }
    else {
        let msg = `${connection.uuid}: deny: ${connection.remote.ip}`;
        log(msg);
        return next(DENYDISCONNECT);
    }
}

exports.hook_rcpt = function (next, connection, params)
{
    if (Array.isArray(params)) {
        
        params.forEach(rcpt => {

            let domain  = rcpt.host;
            if (domain) {
                log(`${connection.uuid}: ${domain}`);
            }
            
        });
    }

    return next(OK);
    // return next();
};

exports.hook_queue_outbound = function(next, connection)
{
    log("hook_queue_outbound: " + connection.transaction.uuid);

    jsonlog(connection.transaction.rcpt_to);

    return next(CONT);
}


exports.hook_send_email = function(next, hmail)
{
    log("send mail: " + hmail.todo.uuid + " to: " + hmail.todo.rcpt_to[0].original);
    // jsonlog(hmail.todo.rcpt_to[0]);
    return next(DENY);
}

// exports.register = function()
// {
//     this.lognotice("registering hooks");
//     this.register_hook('connect', 'hook_connect');
//     this.register_hook('rcpt', 'hook_rcpt');
// };


function log(msg)
{
    fs.appendFile(logfile, msg + "\n", err => {
        if (err) {
        }
        //file written successfully
    });    
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
