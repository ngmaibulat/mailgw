import chalk from 'chalk'

const help = `
    npx envsub --env-file .env templates/routing.json config/routing.json
    npx envsub --env-file .env templates/relays.json config/relays.json
    npx envsub --env-file .env templates/logging.json config/logging.json
`

export function helpGenJson() {
    const header = chalk.yellowBright(
        '\n\tRun the following commands to generate json configs:'
    )
    const msg = chalk.green(help)

    console.log(header)
    console.log(msg)
}

export function helpContainers() {
    const header = chalk.yellowBright('\n\tContainers\n')
    const msg = chalk.green('Review/Run: config.sh\n')

    console.log(header)
    console.log(msg)
}
