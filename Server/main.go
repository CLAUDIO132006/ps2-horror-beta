// HORROR BETA - SERVIDOR DEDICADO (version Windows / Go). Mismo protocolo que horror_server.py (2 a 4 jugadores).
//   - Lista de salas por internet (PARTIDAS PUBLICAS).
//   - Relay UDP entre TODOS los jugadores de la sala (funciona entre emuladores y detras de routers).
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
	"sort"
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
	maxPlayers   = 4
)

type peer struct {
	key  string
	addr *net.UDPAddr
	seen time.Time
}

type room struct {
	id      uint32
	name    string
	maxp    int
	slots   map[int]*peer // 0 = anfitrion
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
	seenIP   map[string]bool
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
	if m.slot == 0 { // se va el anfitrion: la sala se cierra
		for k, p := range m.r.slots {
			if k != 0 {
				delete(s.byAddr, p.key)
			}
		}
		delete(s.rooms, m.r.id)
		s.logf("sala %d cerrada (%s)", m.r.id, m.r.name)
	} else {
		delete(m.r.slots, m.slot)
		s.logf("sala %d: el jugador %d salio", m.r.id, m.slot+1)
	}
}

func (s *server) sendErr(a *net.UDPAddr, code byte) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5] = svErr, code
	s.send(b, a)
}

func (s *server) sendRoom(a *net.UDPAddr, r *room) {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5] = svRoom, byte(r.maxp)
	binary.LittleEndian.PutUint32(b[8:], r.id)
	s.send(b, a)
}

func (s *server) sendJoined(a *net.UDPAddr, slot int, r *room) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5], b[6], b[7] = svJoined, byte(slot), byte(r.maxp), byte(r.players())
	s.send(b, a)
}

func (s *server) handle(d []byte, a *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rx++
	ip := a.IP.String()
	if !s.seenIP[ip] { // primera vez que llega algo de esta IP: sirve para comprobar que el router/firewall dejan pasar
		s.seenIP[ip] = true
		s.logf("LLEGO UN PAQUETE de %s  (la conexion con esa IP funciona)", ip)
	}
	if len(d) < 5 {
		return
	}
	magic := binary.LittleEndian.Uint32(d)
	now := time.Now()
	key := a.String()

	if magic == gameMagic { // paquete del juego: reenviar a TODOS los demas de la sala
		m, ok := s.byAddr[key]
		if !ok {
			return
		}
		if p := m.r.slots[m.slot]; p != nil {
			p.seen = now
		}
		for k, p := range m.r.slots {
			if k != m.slot {
				s.send(d, p.addr)
			}
		}
		return
	}
	if magic != srvMagic {
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
				old.r.slots[0].seen = now
				s.sendRoom(a, old.r)
				return
			}
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
		r := &room{id: s.nextID, name: name, maxp: maxp, slots: map[int]*peer{0: {key, a, now}}, created: now}
		s.nextID = s.nextID%0x7FFFFFFF + 1
		s.rooms[r.id] = r
		s.byAddr[key] = member{r, 0}
		s.sendRoom(a, r)
		s.logf("sala %d creada: %s  para %d jugadores (%s)", r.id, name, maxp, key)

	case svList:
		avail := make([]*room, 0, len(s.rooms))
		for _, r := range s.rooms {
			avail = append(avail, r)
		}
		sort.Slice(avail, func(i, j int) bool { // libres primero, mas nuevas primero
			fi, fj := avail[i].players() >= avail[i].maxp, avail[j].players() >= avail[j].maxp
			if fi != fj {
				return !fi
			}
			return avail[i].created.After(avail[j].created)
		})
		if len(avail) > listMax {
			avail = avail[:listMax]
		}
		out := make([]byte, 8+32*len(avail))
		binary.LittleEndian.PutUint32(out, srvMagic)
		out[4], out[5] = svListRes, byte(len(avail))
		for i, r := range avail {
			o := 8 + i*32
			binary.LittleEndian.PutUint32(out[o:], r.id)
			out[o+4] = byte(r.players())
			copy(out[o+5:o+28], []byte(r.name))
			out[o+31] = byte(r.maxp)
		}
		s.send(out, a)

	case svJoin:
		if len(d) < 12 {
			return
		}
		rid := binary.LittleEndian.Uint32(d[8:])
		if old, ok := s.byAddr[key]; ok && old.slot != 0 && old.r.id == rid { // JOIN repetido: ya esta dentro
			old.r.slots[old.slot].seen = now
			s.sendJoined(a, old.slot, old.r)
			return
		}
		r, ok := s.rooms[rid]
		if !ok {
			s.sendErr(a, errNotFound)
			return
		}
		if _, ok := s.byAddr[key]; ok {
			s.dropAddr(key)
			if r2, ok2 := s.rooms[rid]; ok2 {
				r = r2
			} else {
				s.sendErr(a, errNotFound)
				return
			}
		}
		slot := r.freeSlot()
		if slot < 0 {
			s.sendErr(a, errFull)
			return
		}
		r.slots[slot] = &peer{key, a, now}
		s.byAddr[key] = member{r, slot}
		s.sendJoined(a, slot, r)
		s.logf("sala %d: entro el jugador %d (%s)", r.id, slot+1, key)

	case svKeep:
		if m, ok := s.byAddr[key]; ok {
			m.r.slots[m.slot].seen = now
			if m.slot == 0 && len(d) > 5 && d[5] >= 2 && d[5] <= maxPlayers && int(d[5]) >= m.r.players() && m.r.maxp != int(d[5]) {
				m.r.maxp = int(d[5]) // el anfitrion cambio los jugadores de la sala
				s.logf("sala %d: ahora para %d jugadores", m.r.id, m.r.maxp)
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
			if h := r.slots[0]; h == nil || now.Sub(h.seen) > roomTimeout {
				if h != nil {
					s.dropAddr(h.key)
				}
				continue
			}
			for k, p := range r.slots {
				if k != 0 && now.Sub(p.seen) > guestTimeout {
					s.dropAddr(p.key)
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
	flag.Parse()

	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("0.0.0.0:%d", *port))
	if err == nil {
		var conn *net.UDPConn
		conn, err = net.ListenUDP("udp4", addr)
		if err == nil {
			s := &server{conn: conn, rooms: map[uint32]*room{}, byAddr: map[string]member{}, seenIP: map[string]bool{}, nextID: 1, maxRooms: *maxRooms, quiet: *quiet}
			fmt.Println("==============================================================")
			fmt.Println(" HORROR BETA - SERVIDOR DEDICADO (2 a 4 jugadores)")
			fmt.Printf(" Escuchando en UDP puerto %d\n", *port)
			ips := localIPs()
			if len(ips) > 0 {
				fmt.Println(" Escribe UNA de estas IP en el juego (ONLINE > IP SERVIDOR)")
				fmt.Println(" si juegan en la misma red WiFi (el mismo PC: 127.0.0.1):")
				for _, ip := range ips {
					fmt.Println("     " + ip)
				}
			}
			fmt.Println(" Por internet: abre el puerto UDP en tu router y usa tu IP publica.")
			fmt.Println(" Cuando un jugador consiga conectar veras aqui: LLEGO UN PAQUETE de <su IP>.")
			fmt.Println(" Si nunca aparece esa linea, el router o el firewall lo estan bloqueando.")
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
