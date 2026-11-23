package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	maxMessage   = 4096
	pingInterval = 30 * time.Second
	saveInterval = 30 * time.Second
	writeTimeout = 5 * time.Second
)

var (
	errInvalid = errors.New("INVALID")
	errOffline = errors.New("OFFLINE")

	idPattern     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	secretPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Hub connects each board with the apps watching it.
type Hub struct {
	store  *Store
	mu     sync.Mutex
	boards map[string]*boardState // only boards that are online or watched
}

type boardState struct {
	conn   *websocket.Conn // nil while the board is offline
	status map[string]json.RawMessage
	seen   time.Time
	saved  time.Time
	apps   map[*appConn]struct{}
}

type appConn struct {
	conn *websocket.Conn
	send chan []byte
}

func newHub(store *Store) *Hub {
	return &Hub{store: store, boards: map[string]*boardState{}}
}

// withState runs fn with the board's state while holding the lock, loading the last
// status from the database if the board is not in memory yet.
func (h *Hub) withState(id string, fn func(*boardState)) error {
	h.mu.Lock()
	st := h.boards[id]
	if st != nil {
		fn(st)
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	raw, seen, err := h.store.loadStatus(id)
	if err != nil {
		return err
	}
	status := map[string]json.RawMessage{}
	if raw != nil {
		json.Unmarshal(raw, &status)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if st = h.boards[id]; st == nil {
		st = &boardState{status: status, seen: seen, saved: time.Now(), apps: map[*appConn]struct{}{}}
		h.boards[id] = st
	}
	fn(st)
	return nil
}

// releaseLocked drops a board from memory once nothing uses it. Its status is in the database by then.
func (h *Hub) releaseLocked(id string, st *boardState) {
	if st.conn == nil && len(st.apps) == 0 && h.boards[id] == st {
		delete(h.boards, id)
	}
}

func (h *Hub) broadcastLocked(st *boardState, msg []byte) {
	for a := range st.apps {
		a.push(msg)
	}
}

// MARK: - Board

func (h *Hub) serveBoard(w http.ResponseWriter, r *http.Request) {
	id, ok := h.authorize(w, r, true)
	if !ok {
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(maxMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	err = h.withState(id, func(st *boardState) {
		if st.conn != nil {
			st.conn.CloseNow() // a reconnect replaces the old connection
		}
		st.conn = c
		st.seen = time.Now()
		h.broadcastLocked(st, onlineMessage(true))
	})
	if err != nil {
		log.Printf("board %s: %v", id, err)
		return
	}
	defer h.detachBoard(id, c)

	go ping(ctx, c)
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		h.boardMessage(id, data)
	}
}

// boardMessage merges a status packet into the last status and passes it on to the apps.
func (h *Hub) boardMessage(id string, data []byte) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(data, &msg) != nil {
		return
	}
	now := time.Now()
	var raw []byte
	h.mu.Lock()
	st := h.boards[id]
	if st == nil {
		h.mu.Unlock()
		return
	}
	for k, v := range msg {
		st.status[k] = v
	}
	st.seen = now
	h.broadcastLocked(st, data)
	if now.Sub(st.saved) >= saveInterval {
		st.saved = now
		raw, _ = json.Marshal(st.status)
	}
	h.mu.Unlock()
	if raw != nil {
		h.save(id, raw, now)
	}
}

func (h *Hub) detachBoard(id string, c *websocket.Conn) {
	h.mu.Lock()
	st := h.boards[id]
	if st == nil || st.conn != c {
		h.mu.Unlock()
		return
	}
	st.conn = nil
	st.seen = time.Now()
	h.broadcastLocked(st, onlineMessage(false))
	raw, _ := json.Marshal(st.status)
	seen := st.seen
	h.releaseLocked(id, st)
	h.mu.Unlock()
	h.save(id, raw, seen)
}

func (h *Hub) save(id string, raw []byte, seen time.Time) {
	if err := h.store.saveStatus(id, raw, seen); err != nil {
		log.Printf("board %s: save status: %v", id, err)
	}
}

// command forwards a CMD or SET message from an app to the board.
func (h *Hub) command(ctx context.Context, id string, data []byte) error {
	var msg map[string]json.RawMessage
	if json.Unmarshal(data, &msg) != nil {
		return errInvalid
	}
	_, isCmd := msg["CMD"]
	_, isSet := msg["SET"]
	if !isCmd && !isSet {
		return errInvalid
	}
	var c *websocket.Conn
	h.mu.Lock()
	if st := h.boards[id]; st != nil {
		c = st.conn
	}
	h.mu.Unlock()
	if c == nil {
		return errOffline
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := c.Write(wctx, websocket.MessageText, data); err != nil {
		return errOffline
	}
	return nil
}

// MARK: - App

func (h *Hub) serveApp(w http.ResponseWriter, r *http.Request) {
	id, ok := h.authorize(w, r, false)
	if !ok {
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(maxMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	a := &appConn{conn: c, send: make(chan []byte, 32)}
	err = h.withState(id, func(st *boardState) {
		st.apps[a] = struct{}{}
		a.send <- snapshotLocked(st) // first message, before any update can be queued
	})
	if err != nil {
		log.Printf("app for %s: %v", id, err)
		return
	}
	defer h.detachApp(id, a)

	go a.writeLoop(ctx)
	go ping(ctx, c)
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if err := h.command(ctx, id, data); err != nil {
			a.push(errorMessage(err))
		}
	}
}

func (h *Hub) detachApp(id string, a *appConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.boards[id]; st != nil {
		delete(st.apps, a)
		h.releaseLocked(id, st)
	}
}

// push queues a message without blocking. An app that cannot keep up is disconnected and reconnects.
func (a *appConn) push(msg []byte) {
	select {
	case a.send <- msg:
	default:
		a.conn.CloseNow()
	}
}

func (a *appConn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-a.send:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := a.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				a.conn.CloseNow()
				return
			}
		}
	}
}

// MARK: - HTTP

func (h *Hub) serveStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := h.authorize(w, r, false)
	if !ok {
		return
	}
	var snap []byte
	err := h.withState(id, func(st *boardState) {
		snap = snapshotLocked(st)
		h.releaseLocked(id, st)
	})
	if err != nil {
		log.Printf("status for %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(snap)
}

func (h *Hub) serveCommand(w http.ResponseWriter, r *http.Request) {
	id, ok := h.authorize(w, r, false)
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessage))
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch err := h.command(r.Context(), id, data); {
	case errors.Is(err, errInvalid):
		w.WriteHeader(http.StatusBadRequest)
		w.Write(errorMessage(err))
	case errors.Is(err, errOffline):
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(errorMessage(err))
	default:
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("{}"))
	}
}

// serveRekey replaces the secret of a board. The app authorizes with the old secret and
// sends the new one it got from the board over BLE: {"SECRET":"<new secret>"}.
func (h *Hub) serveRekey(w http.ResponseWriter, r *http.Request) {
	id, ok := h.authorize(w, r, false)
	if !ok {
		return
	}
	var body struct{ SECRET string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessage)).Decode(&body); err != nil || !secretPattern.MatchString(body.SECRET) {
		http.Error(w, "invalid secret", http.StatusBadRequest)
		return
	}
	if err := h.store.rekeyBoard(id, body.SECRET); err != nil {
		log.Printf("rekey %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorize checks "Authorization: Board <id>:<secret>". Only the board itself may register a new id.
func (h *Hub) authorize(w http.ResponseWriter, r *http.Request, register bool) (string, bool) {
	cred, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Board ")
	id, secret, _ := strings.Cut(cred, ":")
	if !ok || !idPattern.MatchString(id) || !secretPattern.MatchString(secret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	valid, err := h.store.checkBoard(id, secret, register)
	if err != nil {
		log.Printf("authorize %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", false
	}
	if !valid {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	return id, true
}

// shutdown saves the status of every connected board and closes all connections.
func (h *Hub) shutdown() {
	type pending struct {
		id   string
		raw  []byte
		seen time.Time
	}
	var saves []pending
	h.mu.Lock()
	for id, st := range h.boards {
		if st.conn != nil {
			raw, _ := json.Marshal(st.status)
			saves = append(saves, pending{id, raw, st.seen})
			st.conn.CloseNow()
		}
		for a := range st.apps {
			a.conn.CloseNow()
		}
	}
	h.mu.Unlock()
	for _, p := range saves {
		h.save(p.id, p.raw, p.seen)
	}
}

// MARK: - Helpers

// snapshotLocked is the full last status plus ONLINE and SEEN (Unix seconds, 0 if never seen).
func snapshotLocked(st *boardState) []byte {
	out := make(map[string]any, len(st.status)+2)
	for k, v := range st.status {
		out[k] = v
	}
	out["ONLINE"] = st.conn != nil
	var seen int64
	if !st.seen.IsZero() {
		seen = st.seen.Unix()
	}
	out["SEEN"] = seen
	raw, _ := json.Marshal(out)
	return raw
}

func onlineMessage(online bool) []byte {
	if online {
		return []byte(`{"ONLINE":true}`)
	}
	return []byte(`{"ONLINE":false}`)
}

func errorMessage(err error) []byte {
	raw, _ := json.Marshal(map[string]string{"ERR": err.Error()})
	return raw
}

func ping(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, 2*writeTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				c.CloseNow()
				return
			}
		}
	}
}
