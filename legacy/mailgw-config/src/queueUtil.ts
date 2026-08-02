import fs from 'node:fs/promises'

import { lsQueue, formatTable } from './queueMetadata.js'
import { lsQueueDetails, formatDetailsTable } from './queueDetails.js'

import { getFilter } from './queueFilter.js'
import { getArgs } from './queueArgs.js'

//index files to DB: Queue Watcher Tool
//with list of files: copy, move, run command with a template: somecmd -somearg $fname $hash $size $createdate

//show statistics
//filter
//perform action of filelist

//!flush items
//separate NPM package

//Queue Web UI
//Mail Trap
//plugins: deferred, bounced

const args = getArgs()
const dir = args.dir as string
const filter = getFilter(args)

// console.log(filter)

if (args.meta) {
    const tbl = formatTable(lsQueue(dir))
    console.log('\n Metadata:')
    console.log(tbl)
}

const data = lsQueueDetails(dir, filter)
const tblDetails = formatDetailsTable(data)

console.log('\n Details:')
console.log(tblDetails)

if (args.out && typeof args.out == 'string') {
    //Output list of files to a file
    console.log('\nOutput to file:', args.out)
    const filenames = data.map((item) => item.filename)
    const content = filenames.join('\n')
    // console.log(content)

    try {
        await fs.writeFile(args.out, content + '\n')
    } catch (err) {
        console.error('Error writing to file:', args.out)
        process.exit(1)
    }
}
