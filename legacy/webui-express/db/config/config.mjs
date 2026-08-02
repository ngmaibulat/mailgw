// import dotenv from "dotenv";
import { dotenv } from "../../src/adapter.js";

dotenv.config({ quiet: true }); // quiet:true suppresses dotenv v17's stdout tip banner

const configs = {
    development: {
        username: process.env.DB_USER,
        password: process.env.DB_PASS,
        database: process.env.DB_NAME,
        host: process.env.DB_HOST,
        dialect: process.env.DB_DRIVER,
        logging: true,
    },
    production: {
        username: process.env.DB_USER,
        password: process.env.DB_PASS,
        database: process.env.DB_NAME,
        host: process.env.DB_HOST,
        dialect: process.env.DB_DRIVER,
        logging: false,
    },
};

const env = process.env.NODE_ENV || "development";
const config = configs[env];

export default config;
