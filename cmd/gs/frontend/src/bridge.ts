/* ROCSAR Ground Station — browser-side bridge.
 *
 * The console is a pure web app: the Go binary serves this file and the
 * operator opens the URL in their own browser. The only channel between the
 * browser and Go is the WebSocket this file wraps. Three message shapes cross
 * it (all JSON):
 *
 *   - call     {id, method, args}        browser -> Go
 *   - reply    {id, ok, result|error}    Go -> browser
 *   - event    {event, payload}          Go -> browser
 *
 * Calls are correlated by id, the same pattern the OBC uses for commands.
 * Events are pushed unsolicited. This file is the other half of the contract
 * that cmd/gs/bridge.go implements; the two must agree.
 *
 * There is no generated code here. The Wails bindings (../wailsjs) are gone;
 * this is hand-written and small enough to read in one sitting.
 */

type ReplyMsg = { id: number; ok: boolean; result?: unknown; error?: string };
type EventMsg = { event: string; payload: unknown };

let ws: WebSocket | null = null;
let nextId = 1;
const pending = new Map<number, { resolve: (v: unknown) => void; reject: (e: Error) => void }>();
const listeners = new Map<string, Set<(payload: any) => void>>();

function handleMessage(msg: ReplyMsg | EventMsg): void {
    if ('id' in msg) {
        const p = pending.get(msg.id);
        if (p) {
            pending.delete(msg.id);
            if (msg.ok) {
                p.resolve(msg.result);
            } else {
                p.reject(new Error(msg.error));
            }
        }
        return;
    }
    const fns = listeners.get(msg.event);
    if (fns) {
        fns.forEach((fn) => fn(msg.payload));
    }
}

/* connect opens the WebSocket and resolves once it is open. On a drop it
 * rejects every pending call (they will never be answered) and reconnects
 * after a backoff, because the console must survive a browser reload during
 * development and a browser restart in the field. */
export function connect(): Promise<void> {
    return new Promise((resolve, reject) => {
        ws = new WebSocket(`ws://${location.host}/ws`);
        ws.onopen = () => resolve();
        ws.onerror = (e) => reject(e);
        ws.onmessage = (ev: MessageEvent) => handleMessage(JSON.parse(ev.data) as ReplyMsg | EventMsg);
        ws.onclose = () => {
            for (const [, p] of pending) {
                p.reject(new Error('connection closed'));
            }
            pending.clear();
            setTimeout(() => {
                void connect();
            }, 1000);
        };
    });
}

/* call sends one RPC and resolves with its result. The method name and args
 * are the contract with App.Dispatch in cmd/gs/app.go; a method renamed on
 * either side is a runtime failure, not a compile error, which is why the
 * names are listed in GUI_ARCHITECTURE.md section 8. */
export function call<T>(method: string, ...args: unknown[]): Promise<T> {
    const id = nextId++;
    return new Promise((resolve, reject) => {
        pending.set(id, { resolve: resolve as (v: unknown) => void, reject });
        if (ws === null) {
            reject(new Error('not connected'));
            return;
        }
        ws.send(JSON.stringify({ id, method, args }));
    });
}

/* on registers a listener for one event. The event names are the contract in
 * GUI_ARCHITECTURE.md section 8.1; a subscription to a name nobody emits is a
 * quiet panel, so a rename here is a silent failure. */
export function on(event: string, fn: (payload: any) => void): void {
    if (!listeners.has(event)) {
        listeners.set(event, new Set());
    }
    listeners.get(event)!.add(fn);
}
