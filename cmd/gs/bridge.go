// The bridge is the Go side of the console's WebSocket transport.
//
// The console is a pure web app: the Go binary serves the embedded frontend
// over HTTP and the operator opens the URL in their own browser. There is no
// embedded browser, no window management and no CGO. The only channel between
// the Go backend and the browser is this WebSocket.
//
// Three message shapes cross it (all JSON):
//
//   - call     {"id", "method", "args"}        frontend -> Go
//   - reply    {"id", "ok", "result"|"error"}  Go -> frontend
//   - event    {"event", "payload"}            Go -> frontend
//
// Calls are correlated by id, the same pattern the OBC uses for commands
// (ARCHITECTURE.md 5.1). Events are pushed unsolicited. The frontend's
// bridge.ts is the other half of this contract.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// callMsg is one frontend -> Go RPC call.
type callMsg struct {
	ID     uint64            `json:"id"`
	Method string            `json:"method"`
	Args   []json.RawMessage `json:"args"`
}

// replyMsg is one Go -> frontend RPC reply.
type replyMsg struct {
	ID     uint64      `json:"id"`
	OK     bool        `json:"ok"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// eventMsg is one Go -> frontend event push.
type eventMsg struct {
	Event   string      `json:"event"`
	Payload interface{} `json:"payload"`
}

// hub tracks the connected browsers and broadcasts events to them. It is
// written by the App (emit) and by the WebSocket handler (add/remove), so it
// is mutex-guarded. A hub with no connections is not an error: the console
// starts before the operator opens the browser, and telemetry flows into an
// empty hub until they do.
type hub struct {
	mu    sync.Mutex
	conns map[*websocket.Conn]chan []byte
}

func newHub() *hub {
	return &hub{conns: make(map[*websocket.Conn]chan []byte)}
}

// add registers a connection and returns its send channel. The channel is
// buffered so a broadcast never blocks the telemetry path; a reader that has
// fallen far enough behind to fill it is dropped by broadcast's non-blocking
// send rather than stalling the frame loop.
func (h *hub) add(c *websocket.Conn) chan []byte {
	ch := make(chan []byte, 256)
	h.mu.Lock()
	h.conns[c] = ch
	h.mu.Unlock()
	return ch
}

func (h *hub) remove(c *websocket.Conn, ch chan []byte) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
	close(ch)
}

// broadcast sends one event to every connected browser. Non-blocking: a
// connection whose send channel is full is skipped, because the telemetry
// path must never wait on a slow reader. The frontend reconnects and the
// next frame catches it up.
func (h *hub) broadcast(event string, payload interface{}) {
	msg, err := json.Marshal(eventMsg{Event: event, Payload: payload})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.conns {
		select {
		case ch <- msg:
		default:
		}
	}
}

// server serves the embedded frontend and the WebSocket bridge on a loopback
// port. It is a plain http.Server: no framework, no middleware, no CGO.
type server struct {
	app      *App
	hub      *hub
	upgrader websocket.Upgrader
	assets   fs.FS
}

func newServer(app *App, assets fs.FS) *server {
	return &server{
		app:      app,
		hub:      newHub(),
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		assets:   assets,
	}
}

// run starts the server and returns the base URL to open. The server runs
// until ctx is done.
func (s *server) run(ctx context.Context) (string, error) {
	sub, err := fs.Sub(s.assets, "frontend/dist")
	if err != nil {
		return "", err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/geojson/", gzipFS(sub))
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/ws", s.handleWS)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}

	httpSrv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = httpSrv.Close()
	}()

	go func() {
		_ = httpSrv.Serve(ln)
	}()

	return "http://" + ln.Addr().String(), nil
}

// gzipFS serves the pre-compressed basemap.
//
// The Natural Earth GeoJSON is embedded gzipped (see scripts/fetch-basemap.py):
// 4.4 MB raw becomes 1.5 MB compressed, and the embedded copy is what ships in
// the binary. A request for /geojson/foo.json is answered from foo.json.gz with
// Content-Encoding: gzip, so the browser decompresses it transparently and the
// frontend still reads it as a plain JSON URL — MapLibre's GeoJSON source takes
// a URL, and this is why it can.
//
// Requests outside /geojson/ never reach here. A path without a .json suffix or
// without its .gz sibling is a 404, which keeps the handler from becoming a
// second, weaker file server for the whole embedded tree.
func gzipFS(fsys fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if !strings.HasSuffix(name, ".json") {
			http.NotFound(w, r)
			return
		}
		f, err := fsys.Open(name + ".gz")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, f)
	}
}

// handleWS runs one browser connection. A writer goroutine drains the send
// channel (gorilla/websocket allows one concurrent writer); the reader loop
// reads calls, dispatches them, and writes the reply to the same channel.
// The loop exits on a read error, which is how a closed browser is noticed.
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ch := s.hub.add(conn)
	defer func() {
		s.hub.remove(conn, ch)
		_ = conn.Close()
	}()

	go func() {
		for msg := range ch {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()

	for {
		var msg callMsg
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		result, err := s.app.Dispatch(msg.Method, msg.Args)
		reply := replyMsg{ID: msg.ID}
		if err != nil {
			reply.OK = false
			reply.Error = err.Error()
		} else {
			reply.OK = true
			reply.Result = result
		}
		data, err := json.Marshal(reply)
		if err != nil {
			continue
		}
		select {
		case ch <- data:
		default:
		}
	}
}

// unmarshalArg decodes one JSON argument into a typed value.
func unmarshalArg(args []json.RawMessage, i int, dst interface{}) error {
	if i >= len(args) {
		return fmt.Errorf("missing argument %d", i)
	}
	if err := json.Unmarshal(args[i], dst); err != nil {
		return fmt.Errorf("argument %d: %w", i, err)
	}
	return nil
}

func argUint(args []json.RawMessage, i int) (uint32, error) {
	var v uint32
	if err := unmarshalArg(args, i, &v); err != nil {
		return 0, err
	}
	return v, nil
}

func argFloat(args []json.RawMessage, i int) (float64, error) {
	var v float64
	if err := unmarshalArg(args, i, &v); err != nil {
		return 0, err
	}
	return v, nil
}

func argString(args []json.RawMessage, i int) (string, error) {
	var v string
	if err := unmarshalArg(args, i, &v); err != nil {
		return "", err
	}
	return v, nil
}

func argBool(args []json.RawMessage, i int) (bool, error) {
	var v bool
	if err := unmarshalArg(args, i, &v); err != nil {
		return false, err
	}
	return v, nil
}
