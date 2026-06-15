import fs from "node:fs";
import path from "node:path";

import { S3Client, PutObjectCommand } from "@aws-sdk/client-s3";
import dotenv from "dotenv";

if (process.argv.length < 3) {
    console.error("filepath required");
    process.exit(1);
}

// const fpath = "../logservice-0.0.9.tgz";
const fpath = process.argv[2];

dotenv.config();

async function putFile(fpath) {
    const client = new S3Client({});
    const fname = path.basename(fpath);

    const params = {
        Bucket: process.env.S3_BUCKET,
        Key: "mailgw/" + fname,
        Body: fs.readFileSync(fpath),
    };

    return client.send(new PutObjectCommand(params));
}

// console.log(process.env);

await putFile(fpath);
