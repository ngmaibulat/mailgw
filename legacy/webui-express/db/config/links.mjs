import Transaction from "../esmmodels/transaction.mjs";
import MailAddr from "../esmmodels/mailaddr.mjs";

export function initModelLinks() {
    MailAddr.belongsToMany(Transaction, {
        through: "linkAddrTransaction",
    });

    Transaction.belongsToMany(MailAddr, {
        through: "linkAddrTransaction",
    });
}
