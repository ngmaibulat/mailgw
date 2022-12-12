
module.exports.routes = [];
module.exports.relays = [];

module.exports = class RoutingTable
{
    routes = [];
    relays = [];

    constructor(relays, routes)
    {
        this.relays = relays;
        this.routes = routes;

        module.exports.relays = relays;
        module.exports.routes = routes;
    }


    findRoute(sender, rcpt)
    {
        function findFn(route)
        {
            let matched = route.match(sender, rcpt);
            return matched;
        };

        let foundRoute = this.routes.find(findFn);

        if (!foundRoute) {
            return false;
        }

        let relayname = foundRoute.relay;
        let relayexists = relayname in this.relays;

        if (!relayexists) {
            console.error("Configuration Error!");
            console.error(`Relay "${relayname}" defined in Routing \nBut cannot be found among relays \nPlease review configuration!\n`);
            return false;
        }
        else {
            let foundRelay = this.relays[relayname];
            return foundRelay;
        }
    }
}
