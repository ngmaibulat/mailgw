//accept: filepath, smtp params

import nodemailer from 'nodemailer'

import { getArgs } from './sendArgs.js'
import { readQueueFile } from './queueDetails.js'

const args = await getArgs()
const file = args.file
const data = readQueueFile(file)

const options = {
    host: args.host,
    port: args.port,
    // secure: true,
    auth: {
        user: args.user,
        pass: args.pass,
    },
    connectionTimeout: 10000,
}

console.log(args)
console.log(data)
console.log(options)

const transporter = nodemailer.createTransport(options)

const envelope = {
    from: 'sender@example.com',
    to: ['recipient@example.com'],
}

const raw = `From: sender@example.com
To: recipient@example.com
Subject: greetings

Salam Aleikum!`

let message = { envelope, raw }

// send email
await transporter.sendMail(message)
