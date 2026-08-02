import fs from 'node:fs'
import { parseArgs } from 'node:util'
import color from '@colors/colors'

const argOptions = {
    options: {
        dir: {
            type: 'string',
            short: 'd',
        },
        src: {
            type: 'string',
        },
        dst: {
            type: 'string',
        },
        domain: {
            type: 'string',
        },
        minage: {
            type: 'string',
        },
        maxage: {
            type: 'string',
        },
        minattempts: {
            type: 'string',
        },
        datefrom: {
            type: 'string',
        },
        dateto: {
            type: 'string',
        },
        out: {
            type: 'string',
            short: 'o',
        },
        help: {
            type: 'boolean',
            short: 'h',
        },
        meta: {
            type: 'boolean',
            short: 'm',
        },
    },
}

function help() {
    const header1 = color.yellow('Common options:')
    const header2 = color.yellow('Filtering Options:')
    const header3 = color.yellow('Output Options:')

    const msg = `

        ${header1}

    --dir <path>: path to queue directory
    --help: show help


        ${header2}

    --src: sender address
    --dst: recipient address
    --domain: recipient domain
    --minage: minimum age, in hours
    --maxage: maximum age, in hours
    --minattempts: minimum attempts
    --datefrom: filter by arrival date/time, starting with
    --dateto: filter by arrival date/time, ending


        ${header3}

    --meta: show metadata based on filenames
    --out: output filenames to file
    `

    console.log(msg)
}

export function getArgs() {
    let args = null

    if (process.argv.length < 3) {
        console.error('required option: --dir <path>')
        process.exit(1)
    }

    try {
        // @ts-ignore
        args = parseArgs(argOptions)
    } catch (err) {
        help()
        process.exit(1)
    }

    const dir = args.values.dir as string
    const hlp = args.values.help as boolean

    if (!dir) {
        help()
        process.exit(1)
    }

    if (hlp) {
        help()
        process.exit(1)
    }

    if (!fs.existsSync(dir)) {
        console.log(`Cannot open directory: ${dir}`)
        process.exit(2)
    }

    const stat = fs.lstatSync(dir)

    // Is it a directory?
    if (!stat.isDirectory()) {
        console.log(`Is not a directory: ${dir}`)
        process.exit(3)
    }

    return args.values
}
