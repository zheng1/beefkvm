package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// serveSOL bridges a browser WebSocket to a native IPMI Serial-over-LAN session.
// Binary WS frames in either direction carry raw serial bytes; a leading text
// frame from the server reports status/errors to the terminal. Only one SOL
// session may be active on the BMC at a time.
func (b *bridge) serveSOL(w http.ResponseWriter, r *http.Request) {
	if !b.checkToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if b.ipmi == nil {
		http.Error(w, "IPMI not configured (no --ipmi-user)", http.StatusServiceUnavailable)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: sameOrigin}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()

	var wmu sync.Mutex // gorilla conns allow only one concurrent writer
	writeText := func(s string) {
		wmu.Lock()
		_ = c.WriteMessage(websocket.TextMessage, []byte(s))
		wmu.Unlock()
	}
	writeBin := func(p []byte) {
		wmu.Lock()
		_ = c.WriteMessage(websocket.BinaryMessage, p)
		wmu.Unlock()
	}

	sol, err := b.ipmi.openSOL(func(data []byte) { writeBin(data) })
	if err != nil {
		writeText("\r\n*** SOL activate failed: " + err.Error() + " ***\r\n")
		log.Printf("bridge: SOL activate failed: %v", err)
		return
	}
	log.Printf("bridge: SOL session opened for browser")
	writeText("\r\n*** Serial-over-LAN connected. If the screen stays blank, the target OS has no serial console (getty) on this port. ***\r\n")
	defer func() {
		sol.Close()
		log.Printf("bridge: SOL session closed")
	}()

	// Browser → serial: read WS frames and forward bytes to the BMC.
	for {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.BinaryMessage || mt == websocket.TextMessage {
			if werr := sol.Write(msg); werr != nil {
				writeText("\r\n*** SOL write error: " + werr.Error() + " ***\r\n")
				return
			}
		}
	}
}
