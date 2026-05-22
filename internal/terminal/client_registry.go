package terminal

import (
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ClientRegistry tracks connected WebSocket clients and their session
// state. It owns its own mutex so client add/remove and session
// lookup don't contend with the screen/PTY lock in Handler.
type ClientRegistry struct {
	clients  map[*websocket.Conn]*clientState
	sessions map[string]*sessionState
	mu       sync.Mutex
}

// NewClientRegistry returns an initialized registry.
func NewClientRegistry() *ClientRegistry {
	return &ClientRegistry{
		clients:  make(map[*websocket.Conn]*clientState),
		sessions: make(map[string]*sessionState),
	}
}

// Add registers a new WebSocket connection and returns its state.
func (r *ClientRegistry) Add(ws *websocket.Conn) *clientState {
	state := &clientState{}
	r.mu.Lock()
	r.clients[ws] = state
	r.mu.Unlock()
	return state
}

// Remove unregisters a WebSocket connection.
func (r *ClientRegistry) Remove(ws *websocket.Conn) {
	r.mu.Lock()
	delete(r.clients, ws)
	r.mu.Unlock()
}

// Snapshot returns a map of connected clients to their session ack
// values. The returned map is safe to use without holding the lock.
func (r *ClientRegistry) Snapshot() map[*websocket.Conn]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[*websocket.Conn]uint64, len(r.clients))
	for ws, state := range r.clients {
		var ack uint64
		if sess := state.session.Load(); sess != nil {
			ack = sess.bytesReceived
		}
		m[ws] = ack
	}
	return m
}

// ResolveSession looks up or creates a session for the given ID,
// attaches it to the client state, and returns the session's current
// bytesReceived. Opportunistically GCs sessions idle >10 min.
func (r *ClientRegistry) ResolveSession(state *clientState, sessionID string) uint64 {
	r.mu.Lock()
	sess, ok := r.sessions[sessionID]
	if !ok {
		sess = &sessionState{lastSeen: time.Now()}
		r.sessions[sessionID] = sess
		for id, s := range r.sessions {
			if time.Since(s.lastSeen) > 10*time.Minute {
				delete(r.sessions, id)
			}
		}
	}
	state.session.Store(sess)
	ack := sess.bytesReceived
	r.mu.Unlock()
	return ack
}

// IncrementReceived adds n to the session's bytesReceived counter.
func (r *ClientRegistry) IncrementReceived(state *clientState, n int) {
	if sess := state.session.Load(); sess != nil {
		r.mu.Lock()
		sess.bytesReceived += uint64(n)
		sess.lastSeen = time.Now()
		r.mu.Unlock()
	}
}
