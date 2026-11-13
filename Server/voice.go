package main

// CHAT DE VOZ DE PROXIMIDAD.
// El juego manda su posicion (paquete svPos). El celular abre https://IP:8443 y manda su microfono por WebSocket.
// El servidor empareja el celular con el jugador por la IP, y reenvia la voz a los demas de la sala
// con volumen y panoramizacion segun la distancia y hacia donde mira cada uno.

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed voice.html
var voiceHTML []byte

const (
	svPos       = 6
	voiceRange  = 25.0 // metros: mas lejos no se oye
	voicePort   = 8443
	posStaleSec = 3
)

type posInfo struct {
	x, z, rx, rz float32
	layer        byte
	t            time.Time
	code         uint16
}

// ---------- WebSocket minimo ----------
type wsConn struct {
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || !strings.Contains(strings.ToLower(r.Header.Get("Upgrade")), "websocket") {
		http.Error(w, "websocket", 400)
		return nil, errors.New("no ws")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("no hijack")
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(h[:]))
	rw.Flush()
	return &wsConn{c: c, r: rw.Reader}, nil
}

func (w *wsConn) write(op byte, p []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	h := []byte{0x80 | op}
	switch {
	case len(p) < 126:
		h = append(h, byte(len(p)))
	default:
		h = append(h, 126, byte(len(p)>>8), byte(len(p)))
	}
	w.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := w.c.Write(append(h, p...)); err != nil {
		return err
	}
	return nil
}

func (w *wsConn) read() (byte, []byte, error) {
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.r, h[:]); err != nil {
			return 0, nil, err
		}
		op := h[0] & 15
		n := uint64(h[1] & 127)
		if n == 126 {
			var b [2]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		} else if n == 127 {
			var b [8]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 1<<16 {
			return 0, nil, errors.New("frame grande")
		}
		var mk [4]byte
		masked := h[1]&128 != 0
		if masked {
			if _, err := io.ReadFull(w.r, mk[:]); err != nil {
				return 0, nil, err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(w.r, p); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range p {
				p[i] ^= mk[i&3]
			}
		}
		switch op {
		case 8:
			return 0, nil, io.EOF
		case 9:
			w.write(10, p)
			continue
		case 10:
			continue
		}
		return op, p, nil
	}
}

// ---------- Cliente de voz ----------
type vclient struct {
	ws     *wsConn
	ip     string
	room   *room // protegido por s.mu
	slot   int
	asked  bool
	status string
	want   [2]int // seleccion manual (sala, jugador); -1 = ninguna
	code   int    // codigo de 4 cifras que muestra el juego (OPCIONES > SONIDO); 0 = emparejar por IP
}

type voiceHub struct {
	s  *server
	mu sync.Mutex
	cl map[*vclient]bool
}

func (h *voiceHub) candidates(ip string, code int) []member {
	var out []member
	now := time.Now()
	for _, m := range h.s.byAddr {
		p := m.r.slots[m.slot]
		if p == nil {
			continue
		}
		if code != 0 {
			if pi, ok := h.s.pos[p.key]; ok && int(pi.code) == code && now.Sub(pi.t) < 10*time.Second {
				out = append(out, m)
			}
		} else if p.addr.IP.String() == ip {
			out = append(out, m)
		}
	}
	return out
}

// cada segundo: empareja el celular con su jugador y avisa de cambios
func (h *voiceHub) pairLoop(c *vclient, done chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-time.After(time.Second):
		}
		h.s.mu.Lock()
		if c.room != nil {
			if h.s.rooms[c.room.id] != c.room || c.room.slots[c.slot] == nil {
				c.room = nil
			}
		}
		msg := ""
		if c.room == nil {
			cands := h.candidates(c.ip, c.code)
			var pick *member
			if len(cands) == 1 {
				pick = &cands[0]
			} else if len(cands) > 1 && c.want[0] >= 0 {
				for i := range cands {
					if int(cands[i].r.id) == c.want[0] && cands[i].slot == c.want[1] {
						pick = &cands[i]
					}
				}
			}
			if pick != nil {
				c.room, c.slot, c.asked = pick.r, pick.slot, false
				msg = fmt.Sprintf(`{"t":"ok","room":%d,"slot":%d}`, c.room.id, c.slot)
			} else if len(cands) > 1 {
				if !c.asked {
					c.asked = true
					o := ""
					for i, m := range cands {
						if i > 0 {
							o += ","
						}
						o += fmt.Sprintf(`{"room":%d,"slot":%d}`, m.r.id, m.slot)
					}
					msg = `{"t":"pick","opts":[` + o + `]}`
				}
			} else if c.status != "wait" {
				c.status = "wait"
				msg = `{"t":"wait"}`
			}
			if c.room != nil {
				c.status = "ok"
			}
		}
		h.s.mu.Unlock()
		if msg != "" {
			if c.ws.write(1, []byte(msg)) != nil {
				return
			}
		}
	}
}

func (h *voiceHub) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	defer ws.c.Close()
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	c := &vclient{ws: ws, ip: host, want: [2]int{-1, -1}}
	h.mu.Lock()
	h.cl[c] = true
	h.mu.Unlock()
	h.s.logf("VOZ: se conecto un celular desde %s", host)
	done := make(chan struct{})
	go h.pairLoop(c, done)
	defer func() {
		close(done)
		h.mu.Lock()
		delete(h.cl, c)
		h.mu.Unlock()
		h.s.logf("VOZ: se desconecto %s", host)
	}()
	for {
		ws.c.SetReadDeadline(time.Now().Add(30 * time.Second))
		op, p, err := ws.read()
		if err != nil {
			return
		}
		if op == 1 {
			if strings.HasPrefix(string(p), "code:") {
				n, _ := strconv.Atoi(strings.TrimSpace(string(p)[5:]))
				h.s.mu.Lock()
				c.code, c.room, c.status, c.asked = n, nil, "", false
				h.s.mu.Unlock()
			} else if strings.HasPrefix(string(p), "sel:") {
				f := strings.Split(string(p), ":")
				if len(f) == 3 {
					a, _ := strconv.Atoi(f[1])
					b, _ := strconv.Atoi(f[2])
					h.s.mu.Lock()
					c.want = [2]int{a, b}
					c.room = nil
					h.s.mu.Unlock()
				}
			}
			continue
		}
		h.relay(c, p)
	}
}

// reenvia la voz de c a los demas de su sala, con volumen y panoramizacion por jugador
func (h *voiceHub) relay(c *vclient, pcm []byte) {
	if len(pcm) == 0 || len(pcm) > 4000 {
		return
	}
	type tgt struct {
		cl     *vclient
		gain   byte
		pan    byte
		flags  byte
		sender byte
	}
	var out []tgt
	h.mu.Lock()
	cls := make([]*vclient, 0, len(h.cl))
	for o := range h.cl {
		cls = append(cls, o)
	}
	h.mu.Unlock()
	now := time.Now()
	h.s.mu.Lock()
	room := c.room
	if room != nil && room.slots[c.slot] != nil {
		sp, ok := h.s.pos[room.slots[c.slot].key]
		if ok && now.Sub(sp.t) < posStaleSec*time.Second {
			for _, o := range cls {
				if o == c || o.room != room || room.slots[o.slot] == nil {
					continue
				}
				lp, ok2 := h.s.pos[room.slots[o.slot].key]
				if !ok2 || now.Sub(lp.t) > posStaleSec*time.Second {
					continue
				}
				dx, dz := float64(sp.x-lp.x), float64(sp.z-lp.z)
				d := math.Hypot(dx, dz)
				var flags byte
				lay := 1.0
				if sp.layer != lp.layer {
					lay = 0.35
					flags |= 1
					d += 4
				}
				if d >= voiceRange {
					continue
				}
				g := 1 - d/voiceRange
				g = g * g * lay
				pan := 0.0
				if d > 0.6 {
					rl := math.Hypot(float64(lp.rx), float64(lp.rz))
					if rl > 0.001 {
						pan = (dx*float64(lp.rx) + dz*float64(lp.rz)) / (d * rl)
					}
					pan *= math.Min(1, (d-0.6)/2)
				}
				out = append(out, tgt{o, byte(math.Round(g * 255)), byte(math.Round(pan*127) + 128), flags, byte(c.slot)})
			}
		}
	}
	h.s.mu.Unlock()
	for _, t := range out {
		b := make([]byte, 4+len(pcm))
		b[0], b[1], b[2], b[3] = t.sender, t.gain, t.pan, t.flags
		copy(b[4:], pcm)
		t.cl.ws.write(2, b)
	}
}

// ---------- Certificado autofirmado (se guarda para aceptarlo una sola vez) ----------
func loadCert() (tls.Certificate, error) {
	if c, err := tls.LoadX509KeyPair("voice_cert.pem", "voice_key.pem"); err == nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "horror-voice"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(5 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	for _, ip := range localIPs() {
		tpl.IPAddresses = append(tpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	cp := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kp := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	os.WriteFile("voice_cert.pem", cp, 0600)
	os.WriteFile("voice_key.pem", kp, 0600)
	return tls.X509KeyPair(cp, kp)
}

func (s *server) startVoice() {
	cert, err := loadCert()
	if err != nil {
		fmt.Println(" AVISO: chat de voz desactivado (certificado):", err)
		return
	}
	h := &voiceHub{s: s, cl: map[*vclient]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.serveWS)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(voiceHTML)
	})
	srv := &http.Server{Addr: fmt.Sprintf(":%d", voicePort), Handler: mux,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}, ErrorLog: log.New(io.Discard, "", 0)}
	go func() {
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			fmt.Println(" AVISO: chat de voz desactivado (puerto TCP", voicePort, "ocupado?):", err)
		}
	}()
}
