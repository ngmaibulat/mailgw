import fsp from 'node:fs/promises'

import { genConfigs } from './lib.js'
import { helpGenJson, helpContainers } from './gensh.js'

import './init.js'

const path_config = process.env.APP_DIR + '/config'
const path_log = process.env.APP_DIR + '/log'
const path_queue = process.env.APP_DIR + '/queue'

//mkdir config
await fsp.mkdir(path_config)

//mkdir queue
await fsp.mkdir(path_queue)

//mkdir log
await fsp.mkdir(path_log)

//generate configs
genConfigs(path_config)

//show help
helpGenJson()
