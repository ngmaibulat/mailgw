import fs from 'node:fs'
import isFile from '@aibulat/isfile'
import chalk from 'chalk'
import dotenv from 'dotenv'

const res = await isFile('.env')

if (!res) {
    const msg = chalk.red('File not found: .env\nExiting...')
    console.error(msg)
    process.exit(1)
}

dotenv.config()

if (!process.env.APP_DIR) {
    const msg = chalk.red('APP_DIR env var is not set')
    console.error(msg)
    process.exit(1)
}

const path = process.env.APP_DIR + '/config'

//exit if config dir exists
if (fs.existsSync(path)) {
    const msg = chalk.red(`\n\t folder exists: ${path}\n\t exiting`)
    console.error(msg)
    process.exit(1)
}
