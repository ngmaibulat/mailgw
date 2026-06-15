import { express } from "../adapter.js";

export function logout(req, res) {
    res.clearCookie("session");
    res.redirect("/");
}
