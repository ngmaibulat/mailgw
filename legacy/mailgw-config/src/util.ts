export function debug(msg: string) {
    if (process.env.DEBUG) {
        console.log(msg)
    }
}
