import { express, uuidv4 } from "../adapter.js";

// import { v4 as uuidv4 } from "uuid";
import { checkAuth } from "./util.mjs";
import { sessions } from "../globals.mjs";
import { AuthInfo } from "../validation/login.mjs";

export async function login(req, res) {
    const body = AuthInfo.safeParse(req.body);

    if (!body.success) {
        const str = body.error.message;
        const msg = encodeURIComponent(str);
        return res.redirect(`/login?msg=ValidationError`);
    }

    const { email, pass } = body.data;
    const authResult = await checkAuth(email, pass);

    if (!authResult) {
        // return res.status(401).send("invalid authentication\n");
        return res.redirect("/login?msg=InvalidAuth");
    }

    const sessionid = uuidv4();
    sessions[sessionid] = { email };

    res.cookie("session", sessionid, {
        maxAge: 8 * 60 * 60 * 1000,
        signed: true,
        path: "/",
        httpOnly: true,
        sameSite: "strict",
        secure: true,
    });

    // console.log("sessions", sessions);

    res.redirect("/");
}
