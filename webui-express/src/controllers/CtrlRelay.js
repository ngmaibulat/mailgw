import { Relay } from "../../db/esmmodels/relay.mjs";

export class CtrlRelay {
    async create(req, res) {
        let id = +req.params.group_id;

        let params = {
            action: "Create",
            group_id: id,
            data: {
                host: "",
                port: 25,
                priority: 10,
                auth_user: "",
                auth_pass: "",
            },
        };

        res.render("routing/relay-form", params);
    }

    async createHandle(req, res) {
        let group_id = +req.params.group_id;
        let url = `/config/relaygrp/${group_id}`;

        Relay.create(req.body).then((inp) => {
            res.redirect(url);
        });
    }

    async edit(req, res) {
        let id = +req.params.id;

        const data = await Relay.findByPk(id);

        const params = {
            action: "Update",
            data: data,
            group_id: data.group_id,
        };

        res.render("routing/relay-form", params);
    }

    async editHandle(req, res) {
        let id = +req.params.id;

        Relay.findByPk(id)
            .then(async (data) => {
                await data.update(req.body);
                await data.save();
                return data;
            })
            .then((data) => {
                let url = `/config/relaygrp/${data.group_id}`;
                res.redirect(url);
            });
    }

    async delete(req, res) {
        let id = +req.params.id;

        Relay.findByPk(id).then((data) => {
            let params = {
                data: data,
            };

            res.render("routing/relay-delete", params);
        });
    }

    async deleteHandle(req, res) {
        let id = +req.params.id;

        Relay.findByPk(id)
            .then(async (data) => {
                let group_id = data.group_id;
                await data.destroy();
                return group_id;
            })
            .then((group_id) => {
                let url = `/config/relaygrp/${group_id}`;
                res.redirect(url);
            });
    }
}
