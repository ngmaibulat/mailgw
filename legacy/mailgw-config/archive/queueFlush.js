import path from 'node:path'

const dir = '../mailgw'

const dirPlugins = dir + '/plugins'

const routePluginPath = path.resolve('.', dirPlugins, 'npRoute.js')

const routePlugin = await import(routePluginPath)

console.log(routePluginPath)
console.log(routePlugin)

const ctx = {
    config: {
        get(path, type) {
            return 'smth'
        },
    },
}

routePlugin.register.bind(ctx)
