import { EventEmitter } from "node:events";

// The change bus behind GET /agent/ws.
//
// A gateway holds a WebSocket open so a deploy reaches it in milliseconds
// instead of on its next 15-second poll. This is what a deploy, a rollback or
// an approval publishes to.
//
// # Deliberately not a message broker
//
// With more than one console replica, a deploy handled by replica A does not
// reach a gateway whose socket lives on replica B. Rather than introduce Redis
// for it, each live socket ALSO re-reads its own gateway row on a slow timer
// (see src/routes/agent.ts) and pushes on change. Cross-replica deploys
// therefore land within that timer rather than instantly, and same-replica ones
// — the overwhelmingly common case, and the only case in every shipped compose
// stack — are instant.
//
// Underneath all of it the gateway's own poll loop is still running. The socket
// is an optimisation: if it never connects, if the bus never fires, if the
// process is replaced mid-deploy, the configuration still converges. Nothing
// here is allowed to be load-bearing.

const bus = new EventEmitter();

// A fleet is many sockets and one process. The default limit of 10 would print
// a spurious leak warning at the eleventh gateway.
bus.setMaxListeners(0);

function topic(gatewayId: number): string {
    return `gateway:${gatewayId}`;
}

// notifyGateway tells any connected socket for this gateway to re-check its
// status. The payload is deliberately empty: the gateway asks the API what
// changed, so a stale or duplicated notification costs one cheap request and
// can never carry wrong state.
export function notifyGateway(gatewayId: number): void {
    bus.emit(topic(gatewayId));
}

// onGatewayChange subscribes and returns an unsubscribe function. Callers must
// call it when the socket closes, or the emitter grows a listener per
// reconnect for the life of the process.
export function onGatewayChange(gatewayId: number, fn: () => void): () => void {
    bus.on(topic(gatewayId), fn);
    return () => {
        bus.off(topic(gatewayId), fn);
    };
}
