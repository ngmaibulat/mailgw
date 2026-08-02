import { Filter } from './types.js'

export function getFilter(values: any): Filter {
    const filter: Filter = {
        src: '',
        dst: '',
        domain: '',
        minage: 0,
        maxage: 0,
        minattempts: 0,
        datefrom: false,
        dateto: false,
    }

    if (values.src) {
        filter.src = values.src
    }

    if (values.dst) {
        filter.dst = values.dst
    }

    if (values.domain) {
        filter.domain = values.domain
    }

    if (values.minage) {
        filter.minage = parseInt(values.minage)
    }

    if (values.maxage) {
        filter.maxage = parseInt(values.maxage)
    }

    if (values.minattempts) {
        filter.minattempts = parseInt(values.minattempts)
    }

    if (values.datefrom) {
        filter.datefrom = new Date()
        //parse Date from string and set
    }

    if (values.dateto) {
        filter.datefrom = new Date()
        //parse Date from string and set
    }

    return filter
}
