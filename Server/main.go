// HORROR BETA - SERVIDOR DEDICADO (version Windows / Go). Mismo protocolo que horror_server.py.
//   - Lista de salas por internet (PARTIDAS PUBLICAS).
//   - Relay UDP entre anfitrion e invitado (funciona entre emuladores y detras de routers).
//
// Uso: horror_server.exe [-port 7778] [-max-rooms 200] [-quiet]
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	srvMagic  = 0x56535248 // 'HRSV'
	gameMagic = 0x52524F48 // 'HORR'

	svCreate, svList, svJoin, svKeep, svLeave  = 1, 2, 3, 4, 5
	svRoom, svListRes, svJoined, svErr, svPong = 16, 17, 18, 19, 20
	errNotFound, errFull, errServerFull        = 1, 2, 3

	roomTimeout  = 15 * time.Second
	guestTimeout = 6 * time.Second
	listMax      = 6
)

type room struct {
	id            uint32
	name          string
	host, guest   string // "ip:puerto"
	hostA, guestA *net.UDPAddr
	tHost, tGuest time.Time
	created       time.Time
}

func (r *room) players() byte {
	if r.guest != "" {
		return 2
	}
	return 1
}

type member struct {
	r      *room
	isHost bool
}

type server struct {
	mu       sync.Mutex
	conn     *net.UDPConn
	rooms    map[uint32]*room
	byAddr   map[string]member
	nextID   uint32
	maxRooms int
	quiet    bool
	rx, tx   uint64
}

func (s *server) logf(f string, a ...interface{}) {
	if !s.quiet {
		fmt.Printf("%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(f, a...))
	}
}

func (s *server) send(b []byte, a *net.UDPAddr) {
	s.tx++
	s.conn.WriteToUDP(b, a)
}

func (s *server) dropAddr(key string) {
	m, ok := s.byAddr[key]
	if !ok {
		return
	}
	delete(s.byAddr, key)
	if m.isHost {
		if m.r.guest != "" {
			delete(s.byAddr, m.r.guest)
		}
		delete(s.rooms, m.r.id)
		s.logf("sala %d cerrada (%s)", m.r.id, m.r.name)
	} else {
		m.r.guest, m.r.guestA = "", nil
		s.logf("sala %d: el invitado salio", m.r.id)
	}
}

func (s *server) sendErr(a *net.UDPAddr, code byte) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5] = svErr, code
	s.send(b, a)
}

func (s *server) sendRoom(a *net.UDPAddr, id uint32) {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4] = svRoom
	binary.LittleEndian.PutUint32(b[8:], id)
	s.send(b, a)
}

func (s *server) handle(d []byte, a *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rx++
	if len(d) < 5 {
		return
	}
	magic := binary.LittleEndian.Uint32(d)
	now := time.Now()
	key := a.String()

	if magic == gameMagic { // paquete del juego: reenviar al otro de la sala
		m, ok := s.byAddr[key]
		if !ok {
			return
		}
		if m.isHost {
			m.r.tHost = now
			if m.r.guestA != nil {
				s.send(d, m.r.guestA)
			}
		} else {
			m.r.tGuest = now
			s.send(d, m.r.hostA)
		}
		return
	}
	if magic != srvMagic {
		return
	}
	switch d[4] {
	case svCreate:
		if old, ok := s.byAddr[key]; ok && old.isHost { // CREATE repetido: misma sala
			old.r.tHost = now
			s.sendRoom(a, old.r.id)
			return
		} else if ok {
			s.dropAddr(key)
		}
		if len(s.rooms) >= s.maxRooms {
			s.sendErr(a, errServerFull)
			return
		}
		name := "SALA"
		if len(d) >= 32 {
			raw := d[8:32]
			if i := indexZero(raw); i >= 0 {
				raw = raw[:i]
			}
			if t := strings.TrimSpace(sanitize(raw)); t != "" {
				name = t
			}
		}
		if len(name) > 23 {
			name = name[:23]
		}
		r := &room{id: s.nextID, name: name, host: key, hostA: a, tHost: now, tGuest: now, created: now}
		s.nextID = s.nextID%0x7FFFFFFF + 1
		s.rooms[r.id] = r
		s.byAddr[key] = member{r, true}
		s.sendRoom(a, r.id)
		s.logf("sala %d creada: %s  (%s)", r.id, name, key)

	case svList:
		var avail []*room
		for _, r := range s.rooms {
			avail = append(avail, r)
		}
		// libres primero, mas nuevas primero; maximo listMax
		for i := 0; i < len(avail); i++ {
			for j := i + 1; j < len(avail); j++ {
				ai, aj := avail[i], avail[j]
				if (ai.players() >= 2 && aj.players() < 2) || (ai.players() == aj.players() && ai.created.Before(aj.created)) {
					avail[i], avail[j] = aj, ai
				}
			}
		}
		if len(avail) > listMax {
			avail = avail[:listMax]
		}
		out := make([]byte, 8+32*len(avail))
		binary.LittleEndian.PutUint32(out, srvMagic)
		out[4], out[5] = svListRes, byte(len(avail))
		for i, r := range avail {
			o := 8 + i*32
			binary.LittleEndian.PutUint32(out[o:], r.id)
			out[o+4] = r.players()
			copy(out[o+5:o+32], []byte(r.name))
		}
		s.send(out, a)

	case svJoin:
		if len(d) < 12 {
			return
		}
		rid := binary.LittleEndian.Uint32(d[8:])
		joined := make([]byte, 8)
		binary.LittleEndian.PutUint32(joined, srvMagic)
		joined[4] = svJoined
		if old, ok := s.byAddr[key]; ok && !old.isHost && old.r.id == rid { // JOIN repetido
			old.r.tGuest = now
			s.send(joined, a)
			return
		}
		r, ok := s.rooms[rid]
		if !ok {
			s.sendErr(a, errNotFound)
			return
		}
		if r.guest != "" && r.guest != key {
			s.sendErr(a, errFull)
			return
		}
		if _, ok := s.byAddr[key]; ok {
			s.dropAddr(key)
		}
		r.guest, r.guestA, r.tGuest = key, a, now
		s.byAddr[key] = member{r, false}
		s.send(joined, a)
		s.logf("sala %d: entro un invitado (%s)", r.id, key)

	case svKeep:
		if m, ok := s.byAddr[key]; ok {
			if m.isHost {
				m.r.tHost = now
			} else {
				m.r.tGuest = now
			}
		}
	case svLeave:
		s.dropAddr(key)
	case svPong:
		s.send(d, a)
	}
}

func indexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

func sanitize(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 32 && c < 127 {
			out = append(out, c)
		} else {
			out = append(out, '?')
		}
	}
	return string(out)
}

func (s *server) janitor() {
	last := time.Now()
	for range time.Tick(time.Second) {
		s.mu.Lock()
		now := time.Now()
		for _, r := range s.rooms {
			if now.Sub(r.tHost) > roomTimeout {
				s.dropAddr(r.host)
			} else if r.guest != "" && now.Sub(r.tGuest) > guestTimeout {
				s.dropAddr(r.guest)
			}
		}
		if now.Sub(last) > time.Minute {
			last = now
			s.logf("salas: %d   paquetes recibidos/enviados: %d/%d", len(s.rooms), s.rx, s.tx)
		}
		s.mu.Unlock()
	}
}

func localIPs() []string {
	var out []string
	ifs, _ := net.InterfaceAddrs()
	for _, a := range ifs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

func main() {
	port := flag.Int("port", 7778, "puerto UDP")
	maxRooms := flag.Int("max-rooms", 200, "maximo de salas")
	quiet := flag.Bool("quiet", false, "sin mensajes en pantalla")
	flag.Parse()

	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("0.0.0.0:%d", *port))
	if err == nil {
		var conn *net.UDPConn
		conn, err = net.ListenUDP("udp4", addr)
		if err == nil {
			s := &server{conn: conn, rooms: map[uint32]*room{}, byAddr: map[string]member{}, nextID: 1, maxRooms: *maxRooms, quiet: *quiet}
			fmt.Println("==============================================================")
			fmt.Println(" HORROR BETA - SERVIDOR DEDICADO")
			fmt.Printf(" Escuchando en UDP puerto %d\n", *port)
			ips := localIPs()
			if len(ips) > 0 {
				fmt.Println(" Escribe UNA de estas IP en el juego (ONLINE > IP SERVIDOR)")
				fmt.Println(" si juegan en la misma red WiFi:")
				for _, ip := range ips {
					fmt.Println("     " + ip)
				}
			}
			fmt.Println(" Por internet: abre el puerto UDP en tu router y usa tu IP publica.")
			fmt.Println(" Cierra esta ventana (o Ctrl+C) para apagar el servidor.")
			fmt.Println("==============================================================")
			go s.janitor()
			buf := make([]byte, 2048)
			for {
				n, a, e := conn.ReadFromUDP(buf)
				if e != nil {
					continue
				}
				d := make([]byte, n)
				copy(d, buf[:n])
				s.handle(d, a)
			}
		}
	}
	fmt.Println("ERROR: no se pudo abrir el puerto UDP:", err)
	fmt.Println("(Puede que otro programa lo use. Prueba con:  horror_server.exe -port 7779)")
	fmt.Println("Pulsa ENTER para cerrar...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
