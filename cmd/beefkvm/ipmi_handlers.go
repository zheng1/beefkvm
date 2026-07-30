package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/zheng1/beefkvm/internal/ipmi"
)

// ipmiState is the bridge's IPMI subsystem: lazy-opened session, cached
// SDR list, and a mutex to serialise access. Held as bridge.ipmi.
type ipmiState struct {
	mu      sync.Mutex
	client  *ipmi.Client
	sensors []ipmi.Sensor // SDR cache (populated on first sensor read)
	loaded  bool
	open    bool
	host    string
	user    string
	pass    string
}

func newIPMI(host, user, pass string) *ipmiState {
	return &ipmiState{host: host, user: user, pass: pass}
}

// ensure opens the IPMI session on first use and caches the SDR list.
// Subsequent calls no-op. Returns the client + cached sensors, or an
// error if either step failed.
func (s *ipmiState) ensure() (*ipmi.Client, []ipmi.Sensor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		c, err := ipmi.Open(s.host, s.user, s.pass)
		if err != nil {
			return nil, nil, err
		}
		s.client = c
		s.open = true
	}
	if !s.loaded {
		list, err := s.client.ScanSensors()
		if err != nil {
			// Don't cache on failure; next request retries.
			return s.client, nil, err
		}
		s.sensors = list
		s.loaded = true
	}
	return s.client, s.sensors, nil
}

// readSnapshot pulls Get Sensor Reading for every cached SDR. Returns a
// slice suitable for JSON encoding to the browser.
func (s *ipmiState) readSnapshot() ([]ipmi.Reading, error) {
	c, sensors, err := s.ensure()
	if err != nil {
		return nil, err
	}
	// Bound the sweep: each sensor read retries internally, so an unhealthy BMC
	// could otherwise hold the session mutex for minutes and make every other
	// IPMI endpoint (power, SEL, users) hang along with it.
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.ReadAllDeadline(sensors, time.Now().Add(8*time.Second)), nil
}

// openSOL activates a native Serial-over-LAN session against the BMC using the
// IPMI credentials. It uses a fresh RMCP+ session (separate from the sensor
// client) so serial streaming can't stall sensor/power polling.
func (s *ipmiState) openSOL(onData func([]byte)) (*ipmi.SOL, error) {
	return ipmi.OpenSOL(s.host, s.user, s.pass, onData)
}

// registerIPMIHandlers wires the /api/sensors + /api/sel + /api/power
// endpoints to the given bridge. Called from main() after bridge is built.
func (b *bridge) registerIPMIHandlers(mux *http.ServeMux) {
	if b.ipmi == nil {
		return
	}
	mux.Handle("/api/sensors", b.gate(http.HandlerFunc(b.handleSensors)))
	mux.Handle("/api/sensors/stream", b.gate(http.HandlerFunc(b.handleSensorsStream)))
	mux.Handle("/api/sel", b.gate(http.HandlerFunc(b.handleSEL)))
	mux.Handle("/api/power/status", b.gate(http.HandlerFunc(b.handlePowerStatus)))
	mux.Handle("/api/power", b.gate(http.HandlerFunc(b.handlePower)))
	mux.Handle("/api/power/boot", b.gate(http.HandlerFunc(b.handlePowerBoot)))
	mux.Handle("/api/system", b.gate(http.HandlerFunc(b.handleSystem)))
	mux.Handle("/api/users", b.gate(http.HandlerFunc(b.handleUsers)))
	mux.Handle("/api/network", b.gate(http.HandlerFunc(b.handleNetwork)))
}

// parseIPv4 turns "a.b.c.d" into 4 bytes.
func parseIPv4(s string) ([]byte, error) {
	var a, b, c, d int
	if n, err := fmt.Sscanf(s, "%d.%d.%d.%d", &a, &b, &c, &d); n != 4 || err != nil {
		return nil, fmt.Errorf("invalid IPv4 %q", s)
	}
	for _, v := range []int{a, b, c, d} {
		if v < 0 || v > 255 {
			return nil, fmt.Errorf("invalid IPv4 %q", s)
		}
	}
	return []byte{byte(a), byte(b), byte(c), byte(d)}, nil
}

// handleNetwork reads BMC LAN config (GET) or applies changes (POST). Writes
// are inherently risky — changing the IP of the channel you're connected
// through drops the session — so the browser gates POST behind confirmation.
func (b *bridge) handleNetwork(w http.ResponseWriter, r *http.Request) {
	client, _, err := b.ipmi.ensure()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	b.ipmi.mu.Lock()
	defer b.ipmi.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		cfg, err := client.GetLANConfig()
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, cfg)
	case http.MethodPost:
		var req struct {
			IPSource   *string `json:"ip_source"` // "static" | "dhcp"
			IP         *string `json:"ip"`
			SubnetMask *string `json:"subnet_mask"`
			Gateway    *string `json:"gateway"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		// Apply source first (static vs dhcp), then addresses.
		if req.IPSource != nil {
			src := byte(1)
			if *req.IPSource == "dhcp" {
				src = 2
			}
			if err := client.SetIPSource(src); err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, err)
				return
			}
		}
		for _, f := range []struct {
			val   *string
			param byte
		}{{req.IP, 3}, {req.SubnetMask, 6}, {req.Gateway, 12}} {
			if f.val == nil || *f.val == "" {
				continue
			}
			ipb, perr := parseIPv4(*f.val)
			if perr != nil {
				writeJSONError(w, http.StatusBadRequest, perr)
				return
			}
			if err := client.SetLANParam(f.param, ipb); err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, err)
				return
			}
		}
		cfg, _ := client.GetLANConfig()
		writeJSON(w, cfg)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleUsers lists BMC users (GET) or applies changes to one slot (POST).
// POST body: {id, name?, password?, privilege?, enabled?}. Only the provided
// fields are applied, in a safe order (name → privilege → password → enable).
func (b *bridge) handleUsers(w http.ResponseWriter, r *http.Request) {
	client, _, err := b.ipmi.ensure()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	b.ipmi.mu.Lock()
	defer b.ipmi.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		users, err := client.ListUsers()
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, users)
	case http.MethodPost:
		var req struct {
			ID        byte    `json:"id"`
			Name      *string `json:"name"`
			Password  *string `json:"password"`
			Privilege *byte   `json:"privilege"`
			Enabled   *bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if req.ID < 1 || req.ID > 63 {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid user id %d", req.ID))
			return
		}
		if req.Name != nil {
			if err := client.SetUserName(req.ID, *req.Name); err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, err)
				return
			}
		}
		if req.Privilege != nil {
			if err := client.SetUserPrivilege(req.ID, *req.Privilege); err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, err)
				return
			}
		}
		if req.Password != nil && *req.Password != "" {
			if err := client.SetUserPassword(req.ID, *req.Password); err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, err)
				return
			}
		}
		if req.Enabled != nil {
			var e error
			if *req.Enabled {
				e = client.EnableUser(req.ID)
			} else {
				e = client.DisableUser(req.ID)
			}
			if e != nil {
				writeJSONError(w, http.StatusServiceUnavailable, e)
				return
			}
		}
		users, _ := client.ListUsers()
		writeJSON(w, users)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSystem returns read-only BMC identity + FRU inventory for the System
// tab: firmware/IPMI versions, manufacturer/product IDs, and board/product
// FRU fields. Individual pieces degrade gracefully if the BMC rejects them.
func (b *bridge) handleSystem(w http.ResponseWriter, r *http.Request) {
	client, _, err := b.ipmi.ensure()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	out := map[string]any{}
	b.ipmi.mu.Lock()
	defer b.ipmi.mu.Unlock()
	if dev, err := client.GetDeviceID(); err != nil {
		out["device_error"] = err.Error()
	} else {
		out["device"] = dev
	}
	if fru, err := client.ReadFRU(0); err != nil {
		out["fru_error"] = err.Error()
	} else {
		out["fru"] = fru
	}
	writeJSON(w, out)
}

// handleSensors returns one snapshot of every sensor reading.
func (b *bridge) handleSensors(w http.ResponseWriter, r *http.Request) {
	readings, err := b.ipmi.readSnapshot()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, readings)
}

// handleSensorsStream pushes a snapshot every 2s over SSE until the
// client disconnects. The SSE format is: "event: sensors\ndata: <json>\n\n".
func (b *bridge) handleSensorsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() {
		readings, err := b.ipmi.readSnapshot()
		if err != nil {
			// Keep the connection alive on transient errors — the BMC may
			// come back on the next tick. The browser hides the tile grid
			// and shows the error string.
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			flusher.Flush()
			return
		}
		buf, err := json.Marshal(readings)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: sensors\ndata: %s\n\n", buf)
		flusher.Flush()
	}
	// Push once immediately so the UI paints without waiting the full 2s.
	send()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			send()
		}
	}
}

// handleSEL routes GET (list, paginated) and DELETE (clear).
func (b *bridge) handleSEL(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		b.handleSELGet(w, r)
	case http.MethodDelete:
		b.handleSELClear(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *bridge) handleSELGet(w http.ResponseWriter, r *http.Request) {
	limit := 100
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	client, _, err := b.ipmi.ensure()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	info, err := client.GetSELInfo()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	// Fetch offset+limit entries then slice — the SEL is small enough
	// (max ~3000 records) that a full walk is cheap versus tracking
	// per-request record-ID cursors on the BMC.
	entries, err := client.ScanSEL(offset + limit)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	if offset > len(entries) {
		entries = nil
	} else {
		entries = entries[offset:]
	}
	writeJSON(w, map[string]any{
		"info":    info,
		"entries": entries,
	})
}

func (b *bridge) handleSELClear(w http.ResponseWriter, r *http.Request) {
	client, _, err := b.ipmi.ensure()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	resv, err := client.ReserveSEL()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	if err := client.ClearSEL(resv); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, map[string]any{"cleared": true})
}

// handlePowerStatus returns the current chassis power state.
func (b *bridge) handlePowerStatus(w http.ResponseWriter, r *http.Request) {
	client, _, err := b.ipmi.ensure()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	b.ipmi.mu.Lock()
	st, err := client.PowerStatus()
	b.ipmi.mu.Unlock()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	state := "off"
	if st.PowerOn {
		state = "on"
	}
	writeJSON(w, map[string]any{
		"power":            state,
		"last_power_event": st.LastEvent,
	})
}

// handlePower issues a chassis control command.
func (b *bridge) handlePower(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	client, _, err := b.ipmi.ensure()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	b.ipmi.mu.Lock()
	defer b.ipmi.mu.Unlock()
	switch req.Action {
	case "on":
		err = client.PowerOn()
	case "off":
		err = client.PowerOff()
	case "cycle":
		err = client.PowerCycle()
	case "reset":
		err = client.PowerReset()
	case "soft":
		err = client.PowerSoftShutdown()
	default:
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("unknown action %q", req.Action))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "action": req.Action})
}

// handlePowerBoot sets the one-time boot device for the next boot.
func (b *bridge) handlePowerBoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Device string `json:"device"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	client, _, err := b.ipmi.ensure()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	b.ipmi.mu.Lock()
	err = client.SetOneTimeBoot(req.Device)
	b.ipmi.mu.Unlock()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "device": req.Device})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("beefkvm: json encode: %v", err)
	}
}

func writeJSONError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
