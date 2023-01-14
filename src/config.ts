import dotenv from "dotenv";

import { genConfigs } from "./lib.js";

dotenv.config();

// SMTP_PORT="25"
// SMTP_RELAY_NAME="devbook.local"
// SMTP_GREETING="NGM Mail Gateway"

// DENY_INCLUDES_UUID="1"
// ACCEPTED_DOMAINS="devbook.local,ngm.dev"

// LOG_LEVEL="notice"
// LOG_TIMESTAMPS="true"
// LOG_FORMAT="json"

// NODEJS_CPU_CORES="1"

const files = [
    "deny_includes_uuid",
    "host_list",
    "log.ini",
    "me",
    "smtp.ini",
    "smtpgreeting",
];

console.log(process.env);

genConfigs();
