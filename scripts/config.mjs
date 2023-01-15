import fs from "node:fs";
import fsp from "node:fs/promises";
import dotenv from "dotenv";
import { genConfigs } from "./lib.mjs";
dotenv.config();
//exit if config dir exists
if (fs.existsSync("config")) {
    console.error("config folder exists");
    console.log("exiting...");
    process.exit(1);
}
//mkdir config
await fsp.mkdir("config");
//mkdir queue
await fsp.mkdir("queue");
//mkdir log
await fsp.mkdir("log");
//copy json files from sample folder
// await fsp.copyFile("sample/relays.json", "config/relays.json");
// await fsp.copyFile("sample/routing.json", "config/routing.json");
//generate configs
genConfigs();
