// import { Request, Response } from "express";
import pug from "pug";

export function index(req, res) {
    const tpl = pug.compileFile("templates/page.pug");
    res.send(tpl());
}
