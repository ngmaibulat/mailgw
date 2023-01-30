const fs = require("fs");
// const fetch = require("node-fetch");

const functions = require("./functions");

const logfile = "./log/ngmroute.log";

const cfgrouting = "routing.json";
const cfgrelays = "relays.json";

const Route = require("./Route");
const RoutingTable = require("./RoutingTable");

// import { RoutingTable } from './RoutingTable';

let relays;
let routes;
let rtable;

/**
 *

+ use haraka way to load configs
- how do we find out actual rcpt addr among list of rcpt?

getAddr(hmail.todo.mail_from)
getAddr(hmail.todo.rcpt_to[0])

hmail.todo:

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
  }

 */

exports.hook_get_mx = function (next, hmail, domain) {
    let sender = functions.getAddr(hmail.todo.mail_from);
    let rcpt = functions.getAddr(hmail.todo.rcpt_to[0]);
    let relay = rtable.findRoute(sender, rcpt);

    // httplog(hmail.todo);
    // httplog(relay);

    if (process.env.MODE == "DEV") {
        const result = {
            sender,
            rcpt,
            relay,
        };

        functions.log(JSON.stringify(result), logfile);
    }

    return next(OK, relay);
};

exports.hook_connect = function (next, connection) {
    connection.relaying = true;
    return next(CONT);
};

// exports.hook_queue_outbound = function (next, connection)
// {
//     functions.log_connection(connection, url_conn);
//     functions.log_transaction(connection.transaction, url_queue);
//     return next(CONT);
// }

exports.register = function () {
    relays = exports.getRelays(cfgrelays);
    routes = exports.getRoutes(cfgrouting);
    rtable = new RoutingTable(relays, routes);

    // this.register_hook('delivered', 'hook_delivered');
};

exports.getRelays = function (path) {
    let relays = this.config.get(path, "json");
    return relays;
};

exports.getRoutes = function (path) {
    let cfgobj = this.config.get(path, "json");
    let cfg = Object.values(cfgobj);

    let routes = new Array();

    cfg.forEach((param) => {
        let route = new Route(
            param.relay,
            param.sender,
            param.sender_domain,
            param.rcpt,
            param.rcpt_domain
        );
        routes.push(route);
    });

    return routes;
};
