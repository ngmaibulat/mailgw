import fs from 'node:fs'
import path from 'node:path'

import cliTable from 'cli-table3'
import color from '@colors/colors'

import { Filter, MailMetadata } from './types.js'

import { parseFilename } from './queueMetadata.js'

import {
    tstampToDate,
    getTimeDiffSeconds,
    getTimeDiffHours,
} from './utilTime.js'

export function readQueueFile(file: string): MailMetadata {
    const data = fs.readFileSync(file)
    const ln = data.readUInt32BE()
    // const newbuff = data.slice(4, ln + 4)
    const newbuff = data.subarray(4, ln + 4)
    const strVal = newbuff.toString()
    const obj = JSON.parse(strVal)

    obj.dtQueue = new Date()
    obj.dtQueue.setTime(obj.queue_time)

    obj.filename = file
    obj.fileinfo = parseFilename(file)

    return obj as MailMetadata
}

//Update Here!
//Filter items

export function lsQueueDetails(dir: string, filter: Filter): MailMetadata[] {
    const arr = fs.readdirSync(dir)

    const data = arr.map((item) => {
        const filepath = path.resolve(dir, item)
        return readQueueFile(filepath)
    })

    const res = data.filter((item) => {
        //filter by sender
        if (filter.src) {
            const re = new RegExp(filter.src)
            const matched = re.test(item.mail_from.original)

            if (!matched) {
                return false
            }
        }

        //filter by rcpt[0]
        if (filter.dst) {
            const re = new RegExp(filter.dst)
            const matched = re.test(item.rcpt_to[0].original)

            if (!matched) {
                return false
            }
        }

        //filter by domain
        if (filter.domain) {
            const re = new RegExp(filter.domain)
            const matched = re.test(item.domain)

            if (!matched) {
                return false
            }
        }

        if (filter.minage) {
            const age = getTimeDiffHours(item.queue_time)
            const matched = filter.minage <= age

            if (!matched) {
                return false
            }
        }

        if (filter.maxage) {
            const age = getTimeDiffHours(item.queue_time)
            const matched = age <= filter.maxage

            if (!matched) {
                return false
            }
        }

        if (filter.minattempts) {
            console.error('Filter by --minattempts not yet implemented')
            process.exit(10)
        }

        if (filter.datefrom) {
            console.error('Filter by --datefrom not yet implemented')
            process.exit(10)
        }

        if (filter.dateto) {
            console.error('Filter by --dateto not yet implemented')
            process.exit(10)
        }

        return item
    })

    return res
}

export function formatDetailsTable(data: MailMetadata[]): string {
    const table = new cliTable({
        head: [
            color.green('Date'),
            color.green('Time'),
            color.green('Domain'),
            color.green('Sender'),
            color.green('Recipient #1'),
        ],
        colWidths: [25, 20, 20, 20],
    })

    for (const row of data) {
        const arr = [
            color.yellow(row.dtQueue.toDateString()),
            color.yellow(row.dtQueue.toTimeString()),
            color.yellow(row.domain),
            color.yellow(row.mail_from.original),
            color.yellow(row.rcpt_to[0].original),
        ]
        //@ts-ignore
        table.push(arr)
    }

    return table.toString()
}
