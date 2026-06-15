import { RelayGroup } from "../../db/esmmodels/relaygroup.mjs";
import { Relay } from "../../db/esmmodels/relay.mjs";

export class CtrlRelayGroup {
    async create(req, res) {
        let params = {
            action: "Create",
            data: {
                name: "",
                description: "",
            },
        };
        res.render("routing/relaygrp-form", params);
    }

    async createHandle(req, res) {
        RelayGroup.create(req.body).then((inp) => {
            let url = "/config/relaygrp";
            res.redirect(url);
        });
    }

    async edit(req, res) {
        let id = +req.params.id;

        RelayGroup.findByPk(id).then((data) => {
            let params = {
                action: "Update",
                data: data,
            };

            res.render("routing/relaygrp-form", params);
        });
    }

    async editHandle(req, res) {
        let id = +req.params.id;

        RelayGroup.findByPk(id)
            .then(async (data) => {
                await data.update(req.body);
                await data.save();
            })
            .then((inp) => {
                let url = "/config/relaygrp";
                res.redirect(url);
            });
    }

    async delete(req, res) {
        let id = +req.params.id;

        RelayGroup.findByPk(id).then((data) => {
            let params = {
                data: data,
            };

            res.render("routing/relaygrp-delete", params);
        });
    }

    async deleteHandle(req, res) {
        let id = +req.params.id;

        RelayGroup.findByPk(id)
            .then(async (data) => {
                await data.destroy();
            })
            .then((inp) => {
                let url = "/config/relaygrp";
                res.redirect(url);
            });
    }

    async details(req, res) {
        let id = +req.params.id;

        let op = {
            where: {
                group_id: id,
            },
        };

        let relays = await Relay.findAll(op);

        RelayGroup.findByPk(id).then((data) => {
            let params = {
                data: data,
                relays: relays,
            };

            res.render("routing/relaygrp-details", params);
        });
    }

    async index(req, res) {
        RelayGroup.findAll().then((data) => {
            let params = {
                data: data,
            };

            res.render("routing/index", params);
        });
    }
}
