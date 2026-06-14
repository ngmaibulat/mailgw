module.exports = class Route {
    //should have a constructor
    //and 4 variables of function type
    //variables are created in consctructor
    //based on route details.
    //if predicate is falsy - it means it is not checked and the corresponding funciton just returns true
    //if predicate is true - create function which check the predicate

    relay = "";
    checkSender;
    checkSenderDomain;
    checkRcpt;
    checkRcptDomain;

    getDomain(addr) {
        let domain = addr.substring(addr.lastIndexOf("@") + 1);
        return domain;
    }

    match(sender, rcpt) {
        let senderdomain = this.getDomain(sender);
        let rcptdomain = this.getDomain(rcpt);

        let res =
            this.checkSender(sender) &&
            this.checkSenderDomain(senderdomain) &&
            this.checkRcpt(rcpt) &&
            this.checkRcptDomain(rcptdomain);

        return res;
    }

    getCheckerFunction(param) {
        // A missing field (null/undefined) is treated as a wildcard, same as
        // an empty string — guard against calling .toString() on it.
        param = param == null ? "" : param.toString();

        if (param) {
            // Match case-insensitively: email domains (and, in practice,
            // addresses) compare without regard to case, so a rule like
            // "Example.com" must match "example.com".
            const expected = param.toLowerCase();
            return function (val) {
                return String(val).toLowerCase() === expected;
            };
        } else {
            return function (val) {
                return true;
            };
        }
    }

    constructor(relay, sender, sender_domain, rcpt, rcpt_domain) {
        this.relay = relay;
        this.checkSender = this.getCheckerFunction(sender);
        this.checkSenderDomain = this.getCheckerFunction(sender_domain);
        this.checkRcpt = this.getCheckerFunction(rcpt);
        this.checkRcptDomain = this.getCheckerFunction(rcpt_domain);
    }
};
