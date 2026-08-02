import fs from 'node:fs'
import { parseArgs } from 'node:util'

import color from '@colors/colors'
import isFile from '@aibulat/isfile'

import { ArgsSendMail } from './types.js'
import { debug } from './util.js'

const argOptions = {
    options: {
        file: {
            type: 'string',
            short: 'f',
        },
        list: {
            type: 'string',
            short: 'l',
        },
        host: {
            type: 'string',
            short: 'h',
        },
        port: {
            type: 'string',
            short: 'p',
        },
        user: {
            type: 'string',
            short: 'u',
        },
        pass: {
            type: 'string',
        },
        env: {
            type: 'boolean',
            short: 'e',
        },
        debug: {
            type: 'boolean',
            short: 'd',
        },
    },
}

function help() {
    const header0 = color.yellow('Generic Options:')
    const header1 = color.yellow('Mails to send:')
    const header2 = color.yellow('SMTP Server:')
    const header3 = color.yellow('Optional Auth:')

    const msg = `

        ${header0}

    --env: load settings from env vars
    --debug: enable debug output

        ${header1}

    --file <path>: file from queue to send
    --list <path>: a text file with list of files to send


        ${header2}

    --host: SMTP server host/ip
    --port: SMTP server port


        ${header3}

    --user: SMTP username
    --pass: SMTP password
    `

    console.log(msg)
}

export async function getArgs(): Promise<ArgsSendMail> {
    let args = null

    if (process.argv.length < 4) {
        console.error('Not enough arguments')
        help()
        process.exit(1)
    }

    try {
        // @ts-ignore
        args = parseArgs(argOptions)
    } catch (err) {
        console.log('Error parsing arguments')
        console.log(err)
        help()
        process.exit(1)
    }

    if (args.values.debug) {
        process.env.DEBUG = 'enabled'
    }

    const list = args.values.list as string
    const file = args.values.file as string

    const pathsProvided = list || file

    if (!pathsProvided) {
        console.error('You should provide one of options: --file or --list')
        process.exit(1)
    }

    if (list) {
        debug(`Using list of files from: ${list}`)

        const found = await isFile(list)
        if (!found) {
            console.error(`File not found/readable: ${list}`)
            process.exit(2)
        }
    } else {
        debug(`Sending file: ${file}`)

        const found = await isFile(file)
        if (!found) {
            console.error(`File not found/readable: ${file}`)
            process.exit(2)
        }
    }

    if (!args.values.host) {
        console.error('Required option: --host')
        process.exit(3)
    }

    let port = 25

    if (args.values.port && typeof args.values.port == 'string') {
        port = parseInt(args.values.port)
    }

    const res: ArgsSendMail = {
        file: args.values.file as string,
        list: args.values.list as string,
        host: args.values.host as string,
        port: port,
        user: args.values.user as string,
        pass: args.values.pass as string,
        env: args.values.env as boolean,
        debug: args.values.debug as boolean,
    }

    return res
}
