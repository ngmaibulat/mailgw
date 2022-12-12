
const fs = require('fs');

const logfile = '/tmp/haraka/haraka.log';

/*

hmail.todo:

{
  "queue_time": 1650968046031,
  "domain": "localhost",
  "rcpt_to": [
    {
      "original": "<addr@localhost>",
      "original_host": "localhost",
      "host": "localhost",
      "user": "addr"
    }
  ],
  "mail_from": {
    "original": "<addr@test.com>",
    "original_host": "test.com",
    "host": "test.com",
    "user": "addr"
  },
  "notes": {
    "skip_plugins": [],
    "authentication_results": []
  },
  "uuid": "FD17BAC6-3692-41C8-B01A-2ED069D3CCD3.1.1"
}


*/

let internalDomains = [
    'ngm.dev',
    'localdomain'
];

let relay_internal = {
    // auth_user: '',
    // auth_pass: '',
    priority: 0,
    exchange: '127.0.0.1',
    port: 2527,
};

let relay_default = {
    // auth_user: '',
    // auth_pass: '',
    priority: 0,
    exchange: '127.0.0.1',
    port: 2526,
};

exports.hook_get_mx = function (next, hmail, domain) {
    
    const cfg = this.config.get('routing.json', 'json');

    jsonlog(cfg.relays);

    jsonlog(cfg.routes);

    let relay;
    let dt = new Date().toISOString();
    let msg = "";

    //if domain is in internal -- return relay_internal
    //otherwise -- return relay_default

    if (internalDomains.includes(domain)) {
        relay = [relay_internal];
        msg = `${dt} ${domain}: internal`;
    }
    else {
        relay = [relay_default];
        msg = `${dt} ${domain}: external`;
    }

    log(msg);

    return next(OK, relay);
}


exports.hook_delivered = function (next, hmail, params) {

    log("some delivered");
    this.lognotice("some delivered");
    return next();
}

// exports.register = function() {

//     log("regsitering hooks");
//     this.lognotice("regsitering hooks");
//     this.register_hook('delivered', 'hook_delivered');
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

