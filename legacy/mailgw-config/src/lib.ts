import { writeFileSync } from 'fs'

const files = [
    'deny_includes_uuid',
    'host_list',
    'me',
    'smtp.ini',
    'smtpgreeting',
]

export function genLogIni() {
    const log_level = process.env.LOG_LEVEL
    const log_timestamps = process.env.LOG_TIMESTAMPS
    const log_format = process.env.LOG_FORMAT

    const res = `
[main]

; level=data, protocol, debug, info, notice, warn, error, crit, alert, emerg
level=${log_level}

; prepend timestamps to log entries? This setting does NOT affect logs emitted
; by logging plugins (like syslog).
timestamps=${log_timestamps}

;  format=default, logfmt, json
format=${log_format}
`

    return res
}

export function genSmtpIni() {
    const cpus = process.env.NODEJS_CPU_CORES
    const port = process.env.SMTP_PORT

    const res = `
; address to listen on (default: all IPv6 and IPv4 addresses, port 25)
; use "[::0]:25" to listen on IPv6 and IPv4 (not all OSes)

listen=0.0.0.0:${port}

; Note you can listen on multiple IPs/ports using commas:
;listen=127.0.0.1:2529,127.0.0.2:2529,127.0.0.3:2530

; public IP address (default: none)
; If your machine is behind a NAT, some plugins (SPF, GeoIP) gain features
; if they know the servers public IP. If 'stun' is installed, Haraka will
; try to figure it out. If that doesn't work, set it here.
;public_ip=N.N.N.N

; Time in seconds to let sockets be idle with no activity
;inactivity_timeout=300

; Drop privileges to this user/group
;user=smtp
;group=smtp

; Don't stop Haraka if plugins fail to compile
;ignore_bad_plugins=0

; Run using cluster to fork multiple backend processes

; nodes=cpus

nodes=${cpus}


; Daemonize
; daemonize=true
; daemon_log_file=/tmp/haraka/haraka.log
; daemon_pid_file=/tmp/haraka/haraka.pid

; Spooling
; Save memory by spooling large messages to disk
;spool_dir=/var/spool/haraka
; Specify -1 to never spool to disk
; Specify 0 to always spool to disk
; Otherwise specify a size in bytes, once reached the
; message will be spooled to disk to save memory.
;spool_after=

; Force Shutdown Timeout
; - Haraka tries to close down gracefully, but if everything is shut down
;   after this time it will hard close. 30s is usually long enough to
;   wait for outbound connections to finish.
;force_shutdown_timeout=30

; SMTP service extensions: https://tools.ietf.org/html/rfc1869
; strict_rfc1869 = false

; Advertise support for SMTPTUF8 (RFC-6531)
;smtputf8=true

[headers]
;add_received=true
;clean_auth_results=true

; replace header_hide_version
;show_version=true

; replace max_header_lines
max_lines=1000

; replace max_received_count
max_received=100
`

    return res
}

export function genDenyIncludesUuid() {
    const value = process.env.DENY_INCLUDES_UUID || '1'
    return value
}

export function genSmtpName() {
    const value = process.env.SMTP_RELAY_NAME || 'smtp-relay'
    return value
}

export function genSmtpGreeting() {
    const value = process.env.SMTP_GREETING || 'NGM Mail Gateway'
    return value
}

export function genSmtpForwardIni() {
    const value = `
enable_outbound=false
host=127.0.0.1
port=2525
connect_timeout=60
timeout=60
max_connections=1000
`
    return value
}

export function genPlugins() {
    const value = `
#ngmfilter

dnsbl
helo.checks
headers

#ngmFilterAttach

npRoute
npLogDelivery
    `

    return value
}

export function genInternalCmdKey() {
    return '231699a65eb9474718b3dd8c18108d550f34279fe86aace09dd04f2b11e61da4'
}

export function genHostList() {
    const value = process.env.ACCEPTED_DOMAINS || 'localhost'
    const lines = value.replaceAll(',', '\n')
    const res = lines + '\n'
    return res
}

export function genConfigs(configdir: string) {
    writeFileSync(`${configdir}/log.ini`, genLogIni())
    writeFileSync(`${configdir}/smtp.ini`, genSmtpIni())
    writeFileSync(`${configdir}/deny_includes_uuid`, genDenyIncludesUuid())
    writeFileSync(`${configdir}/me`, genSmtpName())
    writeFileSync(`${configdir}/smtpgreeting`, genSmtpGreeting())
    writeFileSync(`${configdir}/smtp_forward.ini`, genSmtpForwardIni())
    writeFileSync(`${configdir}/plugins`, genPlugins())
    writeFileSync(`${configdir}/internalcmd_key`, genInternalCmdKey())
    writeFileSync(`${configdir}/host_list`, genHostList())
}
