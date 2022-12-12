module.exports = class Route
{
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

    getDomain(addr)
    {
        let domain = addr.substring(addr.lastIndexOf('@') + 1);
        return domain;
    }

    match(sender, rcpt)
    {
        let senderdomain = this.getDomain(sender);
        let rcptdomain = this.getDomain(rcpt);

        let res = this.checkSender(sender) &&
                this.checkSenderDomain(senderdomain) &&
                this.checkRcpt(rcpt) &&
                this.checkRcptDomain(rcptdomain);
        
        return res;
    }

    getCheckerFunction(param)
    {
        param = param.toString();

        if (param) {
            return function(val) {
                if (val == param) {
                    return true;
                }
                return false;
            }
        }
        else {
            return function(val) {
                return true;
            }            
        }
    }

    constructor(relay, sender, sender_domain, rcpt, rcpt_domain)
    {
        this.relay = relay;
        this.checkSender = this.getCheckerFunction(sender);
        this.checkSenderDomain = this.getCheckerFunction(sender_domain);
        this.checkRcpt = this.getCheckerFunction(rcpt);
        this.checkRcptDomain = this.getCheckerFunction(rcpt_domain);
    }
}
