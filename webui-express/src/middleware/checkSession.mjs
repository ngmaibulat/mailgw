// import { Request, Response, NextFunction } from "express";
import { sessions } from "../globals.mjs";

export function checkSession(req, res, next) {
    const sessionID = req.signedCookies.session;

    if (!sessions[sessionID]) {
        return res.redirect("/login");
    }

    const email = sessions[sessionID].email;
    if (!email) {
        return res.redirect("/login");
    }

    // console.log(sessions[sessionID]);
    next();
}
