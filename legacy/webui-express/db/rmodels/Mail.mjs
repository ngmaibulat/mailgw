import sequelize from "../config/sequelize.mjs";
import { MailAddr } from "../esmmodels/mailaddr.mjs";
import { Transaction } from "../esmmodels/transaction.mjs";
import { Header } from "../esmmodels/header.mjs";
import { linkAddrTransaction } from "../esmmodels/linkaddrtransaction.mjs";

export class Mail {
    id = 0;
    txn = null;
    rcpts = [];
    rcptsSaved = [];

    constructor(txn) {
        txn.rcpt_list = this.getAddrList(txn.rcpt_to);
        this.rcpts = this.getAddrArray(txn.rcpt_to);
        this.txn = txn;

        MailAddr.belongsToMany(Transaction, {
            through: "linkAddrTransaction",
        });
        Transaction.belongsToMany(MailAddr, {
            through: "linkAddrTransaction",
        });
    }

    getAddr(addr) {
        let res = addr.user + "@" + addr.host;
        return res;
    }

    getAddrList(arr) {
        let res = "";
        arr.forEach((addr) => {
            if (!res) {
                res += this.getAddr(addr);
            } else {
                res += "," + this.getAddr(addr);
            }
        });

        return res;
    }

    getAddrArray(arr) {
        let res = [];

        arr.forEach((addr) => {
            let tmp = {
                name: addr.user,
                email: this.getAddr(addr),
            };
            res.push(tmp);
        });

        return res;
    }

    insertRcpts(rcpts) {
        let promises = [];

        rcpts.forEach((rcpt) => {
            let tmp = MailAddr.upsert(rcpt).then((data) => {
                let tmp = JSON.parse(JSON.stringify(data));
                let id = tmp[0].id;
                this.rcptsSaved.push(id);
            });

            promises.push(tmp);
        });

        return promises;
    }

    saveHeaders() {
        let headers = this.txn.headers;
        let promises = [];
        let names = Object.keys(headers);

        names.forEach((name) => {
            let val = {
                mail_id: this.id,
                name: name,
                value: headers[name][0],
            };

            let promise = Header.create(val);
            promises.push(promise);
        });

        return promises;
    }

    async create(txn, rcpts) {
        let links = [];

        let promises = this.insertRcpts(rcpts);
        await Promise.all(promises);

        let txnSaved = await Transaction.create(txn);
        this.id = txnSaved.id;

        this.rcptsSaved.forEach((addr) => {
            let tmp = {
                MailAddrId: addr,
                TransactionId: txnSaved.id,
            };
            links.push(tmp);
        });

        await linkAddrTransaction.bulkCreate(links);
    }

    async save() {
        await this.create(this.txn, this.rcpts);
        return Promise.all(this.saveHeaders());
    }

    async saveAndClose() {
        await this.create(this.txn, this.rcpts);
        await Promise.all(this.saveHeaders());

        // console.log(this.rcptsSaved);
        // console.log(this.id);

        sequelize.close();
    }
}

// query Rcpts by Transaction
// query Transactions by Rcpts
