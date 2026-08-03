/**
 * The Tier-B harness: a real gateway process, a scriptable relay, a fake
 * logservice, and a typed client for the test-only control API.
 *
 * See tests/README.md for what belongs here and what belongs in Go.
 */

export { binaryPath, haveBinary, GO_MODULE, REPO_ROOT } from "./binary.ts";
export {
    baseline,
    relayEverythingTo,
    relayGroup,
    ruleset,
    BUNDLE_FORMAT,
    type Bundle,
    type BaselineOptions,
    type Relay,
} from "./bundle.ts";
export {
    Gateway,
    startGateway,
    parseExposition,
    splitAddr,
    type GatewayOptions,
    type LogRecord,
} from "./gateway.ts";
export { LogSink, startLogSink, type EventKind, type RecordedRequest } from "./logsink.ts";
export {
    Sink,
    startSink,
    parseHeaders,
    type Reply,
    type SinkMessage,
    type SinkScript,
    type SinkSession,
} from "./sink.ts";
export {
    SmtpClient,
    buildMessage,
    UUID_SCRAPER,
    type MailOptions,
    type SendResult,
    type SmtpReply,
} from "./smtp.ts";
export {
    Testctl,
    TestctlError,
    type Applied,
    type CachedBundle,
    type EnrollRequest,
    type QueueEntry,
    type Status,
} from "./testctl.ts";
