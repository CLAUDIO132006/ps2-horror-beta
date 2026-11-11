// HORROR BETA - SERVIDOR DEDICADO (version Windows / Go).
// Mismo protocolo que horror_server.py (2 a 4 jugadores, relay a todos los de la sala).
//
// Uso: horror_server.exe [-port 7778] [-max-rooms 200] [-quiet] [-debug]
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	srvMagic  = 0x56535248 // 'HRSV' (mensajes jugador <-> servidor)
	gameMagic = 0x52524F48 // 'HORR' (paquetes del juego: se reenvian tal cual)

	svCreate = 1
	svList   = 2
	svJoin   = 3
	svKeep   = 4
	svLeave  = 5

	svRoom    = 16
	svListRes = 17
	svJoined  = 18
	svErr     = 19
	svPong    = 20

	errNotFound   = 1
	errFull       = 2
	errServerFull = 3

	maxPlayers   = 4
	listMax      = 6
	roomTimeout  = 15 * time.Second
	guestTimeout = 6 * time.Second
)

type room struct {
	id      uint32
	name    string
	maxp    int
	slots   map[int]*net.UDPAddr // 0 = anfitrion
	seen    map[int]time.Time
	created time.Time
}

func (r *room) players() int { return len(r.slots) }

func (r *room) freeSlot() int {
	for k := 1; k < r.maxp; k++ {
		if _, ok := r.slots[k]; !ok {
			return k
		}
	}
	return -1
}

type member struct {
	r    *room
	slot int
}

type server struct {
	mu       sync.Mutex
	conn     *net.UDPConn
	rooms    map[uint32]*room
	byAddr   map[string]member
	heard    map[string]bool
	nextID   uint32
	maxRooms int
	quiet    bool
	debug    bool
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

func (s *server) dropAddr(key string, why string) {
	m, ok := s.byAddr[key]
	if !ok {
		return
	}
	delete(s.byAddr, key)
	if m.slot == 0 { // se va el anfitrion: la sala se cierra
		for k, a := range m.r.slots {
			if k != 0 {
				delete(s.byAddr, a.String())
			}
		}
		delete(s.rooms, m.r.id)
		s.logf("sala %d cerrada (%s) motivo: %s", m.r.id, m.r.name, why)
	} else {
		delete(m.r.slots, m.slot)
		delete(m.r.seen, m.slot)
		s.logf("sala %d: el jugador %d salio (motivo: %s)", m.r.id, m.slot+1, why)
	}
}

func (s *server) sendErr(a *net.UDPAddr, code byte) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5] = svErr, code
	s.send(b, a)
}

func (s *server) sendRoom(a *net.UDPAddr, maxp int, id uint32) {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5] = svRoom, byte(maxp)
	binary.LittleEndian.PutUint32(b[8:], id)
	s.send(b, a)
}

func (s *server) sendJoined(a *net.UDPAddr, slot, maxp, players int) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5], b[6], b[7] = svJoined, byte(slot), byte(maxp), byte(players)
	s.send(b, a)
}

func (s *server) handle(d []byte, a *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rx++
	key := a.String()
	if s.debug {
		s.logf("RX %s  %d bytes", key, len(d))
	}
	ipStr := a.IP.String()
	if !s.heard[ipStr] {
		s.heard[ipStr] = true
		s.logf("LLEGO UN PAQUETE de %s", ipStr)
	}
	if len(d) < 5 {
		return
	}
	magic := binary.LittleEndian.Uint32(d)
	now := time.Now()

	if magic == gameMagic { // paquete del juego: reenviar a TODOS los demas de la sala
		m, ok := s.byAddr[key]
		if !ok {
			return
		}
		m.r.seen[m.slot] = now
		for k, addr := range m.r.slots {
			if k != m.slot {
				s.send(d, addr)
			}
		}
		return
	}
	if magic != srvMagic {
		if s.debug {
			s.logf("paquete ignorado de %s (magic desconocido %08x)", key, magic)
		}
		return
	}

	switch d[4] {
	case svCreate:
		maxp := 2
		if len(d) > 5 && d[5] != 0 {
			maxp = int(d[5])
		}
		if maxp < 2 {
			maxp = 2
		}
		if maxp > maxPlayers {
			maxp = maxPlayers
		}
		if old, ok := s.byAddr[key]; ok {
			if old.slot == 0 { // CREATE repetido (se perdio la respuesta): misma sala
				old.r.seen[0] = now
				s.sendRoom(a, old.r.maxp, old.r.id)
				return
			}
			s.dropAddr(key, "el mismo cliente creo otra sala")
		}
		if len(s.rooms) >= s.maxRooms {
			s.sendErr(a, errServerFull)
			return
		}
		name := "SALA"
		if len(d) > 8 {
			end := 32
			if len(d) < end {
				end = len(d)
			}
			raw := d[8:end]
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
		r := &room{id: s.nextID, name: name, maxp: maxp,
			slots: map[int]*net.UDPAddr{0: a}, seen: map[int]time.Time{0: now}, created: now}
		s.nextID = s.nextID%0x7FFFFFFF + 1
		s.rooms[r.id] = r
		s.byAddr[key] = member{r, 0}
		s.sendRoom(a, maxp, r.id)
		s.logf("sala %d creada: %s  para %d jugadores (%s)", r.id, name, maxp, key)

	case svList:
		var all []*room
		for _, r := range s.rooms {
			all = append(all, r)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].created.Before(all[j].created) })
		if len(all) > listMax {
			all = all[len(all)-listMax:]
		}
		sort.SliceStable(all, func(i, j int) bool { // libres primero, mas nuevas primero
			fi, fj := all[i].players() >= all[i].maxp, all[j].players() >= all[j].maxp
			if fi != fj {
				return !fi
			}
			return all[i].created.After(all[j].created)
		})
		out := make([]byte, 8+32*len(all))
		binary.LittleEndian.PutUint32(out, srvMagic)
		out[4], out[5] = svListRes, byte(len(all))
		for i, r := range all {
			o := 8 + i*32
			binary.LittleEndian.PutUint32(out[o:], r.id)
			out[o+4] = byte(r.players())
			nm := []byte(r.name)
			if len(nm) > 23 {
				nm = nm[:23]
			}
			copy(out[o+5:o+31], nm)
			out[o+31] = byte(r.maxp) // ultimo byte de la entrada = capacidad de la sala
		}
		s.send(out, a)

	case svJoin:
		if len(d) < 12 {
			return
		}
		rid := binary.LittleEndian.Uint32(d[8:])
		if old, ok := s.byAddr[key]; ok && old.slot != 0 && old.r.id == rid { // JOIN repetido
			old.r.seen[old.slot] = now
			s.sendJoined(a, old.slot, old.r.maxp, old.r.players())
			return
		}
		r, ok := s.rooms[rid]
		if !ok {
			s.sendErr(a, errNotFound)
			return
		}
		if _, ok := s.byAddr[key]; ok {
			s.dropAddr(key, "el mismo cliente entro a otra sala")
		}
		slot := r.freeSlot()
		if slot < 0 {
			s.sendErr(a, errFull)
			return
		}
		r.slots[slot] = a
		r.seen[slot] = now
		s.byAddr[key] = member{r, slot}
		s.sendJoined(a, slot, r.maxp, r.players())
		s.logf("sala %d: entro el jugador %d (%s)", r.id, slot+1, key)

	case svKeep:
		if m, ok := s.byAddr[key]; ok {
			m.r.seen[m.slot] = now
			if m.slot == 0 && len(d) > 5 {
				n := int(d[5])
				if n >= 2 && n <= maxPlayers && n >= m.r.players() && m.r.maxp != n {
					m.r.maxp = n
					s.logf("sala %d: ahora para %d jugadores", m.r.id, n)
				}
			}
		}

	case svLeave:
		s.dropAddr(key, "el cliente mando LEAVE")

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
			if now.Sub(r.seen[0]) > roomTimeout {
				s.dropAddr(r.slots[0].String(), fmt.Sprintf("el anfitrion no manda senales hace %ds (timeout)", int(roomTimeout.Seconds())))
				continue
			}
			for k, a := range r.slots {
				if k != 0 && now.Sub(r.seen[k]) > guestTimeout {
					s.dropAddr(a.String(), "invitado sin senales (timeout)")
				}
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
	debug := flag.Bool("debug", false, "muestra cada paquete recibido")
	flag.Parse()

	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("0.0.0.0:%d", *port))
	if err == nil {
		var conn *net.UDPConn
		conn, err = net.ListenUDP("udp4", addr)
		if err == nil {
			s := &server{conn: conn, rooms: map[uint32]*room{}, byAddr: map[string]member{},
				heard: map[string]bool{}, nextID: 1, maxRooms: *maxRooms, quiet: *quiet, debug: *debug}
			fmt.Println("==============================================================")
			fmt.Println(" HORROR BETA - SERVIDOR DEDICADO")
			fmt.Printf(" Escuchando en UDP puerto %d\n", *port)
			ips := localIPs()
			if len(ips) > 0 {
				fmt.Println(" IPs de este equipo (si juegan en la misma WiFi usa la 192.168...):")
				for _, ip := range ips {
					fmt.Println("     " + ip)
				}
				fmt.Println(" Si hay Hamachi/Radmin/VPN, desactivalo para probar.")
			}
			fmt.Println(" Por internet: abre el puerto UDP en tu router y usa tu IP publica.")
			fmt.Println(" Cuando un jugador conecte veras: LLEGO UN PAQUETE de <su IP>")
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
