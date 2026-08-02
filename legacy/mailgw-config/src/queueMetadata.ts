// $arrival_$nextattempt_$attempts_$pid_$uniquetag_$counter_$host

// 1676349832351_1676369986649_7_35_fl96Rc_8_c21194a1a5c3

import fs from 'node:fs'
import cliTable from 'cli-table3'
import color from '@colors/colors'

import { QueueElement } from './types.js'

import {
    tstampToDate,
    getTimeDiffSeconds,
    getTimeDiffHours,
} from './utilTime.js'

export function parseFilename(filename: string): QueueElement {
    if (!filename) {
        throw new Error('No filename provided')
    }

    let p = filename.split('_')

    const res: QueueElement = {
        dtArrival: tstampToDate(parseInt(p[0])),
        dtNextAttempt: tstampToDate(parseInt(p[1])),
        attempts: +p[2],
        pid: +p[3],
        uniq: p[4],
        counter: +p[5],
        host: p[6],
        ageSeconds: getTimeDiffSeconds(parseInt(p[0])),
        ageHours: getTimeDiffHours(parseInt(p[0])),
    }

    return res
}

export function lsQueue(dir: string): QueueElement[] {
    const arr = fs.readdirSync(dir)
    const res = arr.map((item) => parseFilename(item))
    return res
}

export function formatTable(data: QueueElement[]): string {
    const table = new cliTable({
        head: [
            color.green('Arrival Date'),
            color.green('Arrival Time'),
            color.green('Next Attempt'),
            color.green('Attempts'),
            color.green('Age in sec'),
            color.green('Age in hours'),
        ],
        colWidths: [25, 25, 20, 20, 20],
    })

    for (const row of data) {
        const arr = [
            color.yellow(row.dtArrival.toDateString()),
            color.yellow(row.dtArrival.toTimeString()),
            color.yellow(row.dtNextAttempt.toTimeString()),
            color.red(`${row.attempts}`),
            color.yellow(row.ageSeconds.toString()),
            color.red(row.ageHours.toString()),
        ]
        //@ts-ignore
        table.push(arr)
    }

    return table.toString()
}
