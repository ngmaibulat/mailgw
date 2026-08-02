import { express } from "../adapter.js";

export function formLogin(req, res) {
    // res.send(form);
    res.render("forms/login", {});
}
