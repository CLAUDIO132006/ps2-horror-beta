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
	"math"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	srvMagic  = 0x56535248 // 'HRSV'
	gameMagic = 0x52524F48 // 'HORR'

	svCreate, svList, svJoin, svKeep, svLeave             = 1, 2, 3, 4, 5
	svRoom, svListRes, svJoined, svErr, svPong, svKeepAck = 16, 17, 18, 19, 20, 21
	errNotFound, errFull, errServerFull                   = 1, 2, 3

	roomTimeout  = 90 * time.Second // el anfitrion puede tardar (emulador lento / pausa): la sala no se cierra por poco
	guestTimeout = 25 * time.Second
	listFresh    = 30 * time.Second // en la lista solo salen salas cuyo anfitrion dio senales hace poco
	listMax      = 6
	maxPlayers   = 4
)

type peer struct {
	key  string
	addr *net.UDPAddr
	conn *net.UDPConn // el socket (IP local) por el que llego: se le responde SIEMPRE por el mismo, asi la IP de origen de la respuesta es la que el jugador escribio
	seen time.Time
	tok  uint32 // token de sesion del jugador (sobrevive a un cambio de puerto)
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
	conns    map[string]*net.UDPConn // IP local -> socket
	port     int
	rooms    map[uint32]*room
	byAddr   map[string]member
	byTok    map[uint32]member
	lastSeq  map[uint32]uint32
	warned   map[string]time.Time
	verbose  bool
	pos      map[string]posInfo
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

func (s *server) send(c *net.UDPConn, b []byte, a *net.UDPAddr) {
	s.tx++
	if _, err := c.WriteToUDP(b, a); err != nil {
		s.logf("AVISO: no se pudo responder a %s: %v", a, err)
	}
}

func (s *server) dropAddr(key, why string) {
	m, ok := s.byAddr[key]
	if !ok {
		return
	}
	delete(s.byAddr, key)
	delete(s.pos, key)
	if p := m.r.slots[m.slot]; p != nil && p.tok != 0 {
		delete(s.byTok, p.tok)
	}
	if m.slot == 0 { // se va el anfitrion: la sala se cierra
		for k, p := range m.r.slots {
			if k != 0 {
				delete(s.byAddr, p.key)
				delete(s.pos, p.key)
				if p.tok != 0 {
					delete(s.byTok, p.tok)
				}
			}
		}
		delete(s.rooms, m.r.id)
		s.logf("sala %d cerrada (%s): %s", m.r.id, m.r.name, why)
	} else {
		delete(m.r.slots, m.slot)
		s.logf("sala %d: el jugador %d salio (%s)", m.r.id, m.slot+1, why)
	}
}

func (s *server) sendErr(c *net.UDPConn, a *net.UDPAddr, code byte) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5] = svErr, code
	s.send(c, b, a)
}

func (s *server) sendRoom(c *net.UDPConn, a *net.UDPAddr, r *room) {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5] = svRoom, byte(r.maxp)
	binary.LittleEndian.PutUint32(b[8:], r.id)
	s.send(c, b, a)
}

func (s *server) sendJoined(c *net.UDPConn, a *net.UDPAddr, slot int, r *room) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, srvMagic)
	b[4], b[5], b[6], b[7] = svJoined, byte(slot), byte(r.maxp), byte(r.players())
	s.send(c, b, a)
}

func (s *server) handle(d []byte, a *net.UDPAddr, c *net.UDPConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rx++
	ip := a.IP.String()
	if !s.seenIP[ip] { // primera vez que llega algo de esta IP: sirve para comprobar que el router/firewall dejan pasar
		s.seenIP[ip] = true
		s.logf("LLEGO UN PAQUETE de %s hacia %s  (la conexion con esa IP funciona)", ip, c.LocalAddr())
	}
	if len(d) < 5 {
		return
	}
	magic := binary.LittleEndian.Uint32(d)
	now := time.Now()
	key := a.String()

	var tok, seq uint32
	if magic == srvMagic && len(d) >= 40 {
		tok, seq = binary.LittleEndian.Uint32(d[32:]), binary.LittleEndian.Uint32(d[36:])
	}
	if tok != 0 { // el jugador cambio de IP:puerto (el emulador / NAT abrio un socket nuevo): se le sigue reconociendo por su token
		if m, ok := s.byTok[tok]; ok {
			if p := m.r.slots[m.slot]; p != nil && p.key != key && (p.addr.IP.Equal(a.IP) || now.Sub(p.seen) > 3*time.Second) {
				s.rebind(m, p, a, c, key, "token")
			}
		}
	}

	if magic == gameMagic { // paquete del juego: reenviar a TODOS los demas de la sala
		m, ok := s.byAddr[key]
		if !ok { // puerto nuevo sin avisar: si de esa IP solo falta un jugador con ese numero de jugador (byte 28), es el
			if len(d) > 28 {
				var cand []member
				for _, mm := range s.byAddr {
					if p := mm.r.slots[mm.slot]; p != nil && mm.slot == int(d[28]) && p.addr.IP.Equal(a.IP) && now.Sub(p.seen) > time.Second {
						cand = append(cand, mm)
					}
				}
				if len(cand) == 1 {
					s.rebind(cand[0], cand[0].r.slots[cand[0].slot], a, c, key, "IP y numero de jugador")
					m, ok = cand[0], true
				}
			}
			if !ok {
				s.warnOnce(key, "paquete de juego de %s que no esta en ninguna sala (la sala se cerro o el servidor se reinicio)", key)
				return
			}
		}
		if p := m.r.slots[m.slot]; p != nil {
			p.seen = now
		}
		for k, p := range m.r.slots {
			if k != m.slot {
				s.send(p.conn, d, p.addr)
			}
		}
		return
	}
	if magic != srvMagic {
		return
	}
	if tok != 0 { // orden de los paquetes: un SALIR que llega tarde (despues de un CREAR nuevo) no cierra nada
		last, seen := s.lastSeq[tok]
		stale := seen && int32(seq-last) <= 0
		if !stale || !seen {
			s.lastSeq[tok] = seq
		}
		if stale && d[4] == svLeave {
			s.logf("(ignoro un SALIR atrasado de %s)", key)
			return
		}
		if len(s.lastSeq) > 20000 {
			s.lastSeq = map[uint32]uint32{tok: seq}
		}
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
				s.sendRoom(c, a, old.r)
				return
			}
			s.dropAddr(key, "empezo otra sala")
		}
		if len(s.rooms) >= s.maxRooms {
			s.sendErr(c, a, errServerFull)
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
		r := &room{id: s.nextID, name: name, maxp: maxp, slots: map[int]*peer{0: {key: key, addr: a, conn: c, seen: now, tok: tok}}, created: now}
		s.nextID = s.nextID%0x7FFFFFFF + 1
		s.rooms[r.id] = r
		s.byAddr[key] = member{r, 0}
		if tok != 0 {
			s.byTok[tok] = member{r, 0}
		}
		s.sendRoom(c, a, r)
		s.logf("sala %d creada: %s  para %d jugadores (%s)  [respondo desde %s]", r.id, name, maxp, key, c.LocalAddr())

	case svList:
		avail := make([]*room, 0, len(s.rooms))
		for _, r := range s.rooms {
			if h := r.slots[0]; h != nil && time.Since(h.seen) < listFresh {
				avail = append(avail, r)
			}
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
		s.send(c, out, a)

	case svJoin:
		if len(d) < 12 {
			return
		}
		rid := binary.LittleEndian.Uint32(d[8:])
		if old, ok := s.byAddr[key]; ok && old.slot != 0 && old.r.id == rid { // JOIN repetido: ya esta dentro
			old.r.slots[old.slot].seen = now
			s.sendJoined(c, a, old.slot, old.r)
			return
		}
		r, ok := s.rooms[rid]
		if !ok {
			s.sendErr(c, a, errNotFound)
			return
		}
		if _, ok := s.byAddr[key]; ok {
			s.dropAddr(key, "entro a otra sala")
			if r2, ok2 := s.rooms[rid]; ok2 {
				r = r2
			} else {
				s.sendErr(c, a, errNotFound)
				return
			}
		}
		slot := r.freeSlot()
		if slot < 0 {
			s.sendErr(c, a, errFull)
			return
		}
		r.slots[slot] = &peer{key: key, addr: a, conn: c, seen: now, tok: tok}
		s.byAddr[key] = member{r, slot}
		if tok != 0 {
			s.byTok[tok] = member{r, slot}
		}
		s.sendJoined(c, a, slot, r)
		s.logf("sala %d: entro el jugador %d (%s)", r.id, slot+1, key)

	case svKeep:
		ack := make([]byte, 12)
		binary.LittleEndian.PutUint32(ack, srvMagic)
		ack[4] = svKeepAck
		m, ok := s.byAddr[key]
		if ok {
			ack[5], ack[6], ack[7] = 1, byte(m.r.players()), byte(m.slot)
			binary.LittleEndian.PutUint32(ack[8:], m.r.id)
			if s.verbose {
				s.logf("senal de vida de %s (sala %d, jugador %d)", key, m.r.id, m.slot+1)
			}
		} else {
			s.warnOnce(key, "senal de vida de %s: no esta en ninguna sala (se le avisa para que la cree otra vez)", key)
		}
		s.send(c, ack, a)
		if ok {
			m.r.slots[m.slot].seen = now
			if m.slot == 0 && len(d) > 5 && d[5] >= 2 && d[5] <= maxPlayers && int(d[5]) >= m.r.players() && m.r.maxp != int(d[5]) {
				m.r.maxp = int(d[5]) // el anfitrion cambio los jugadores de la sala
				s.logf("sala %d: ahora para %d jugadores", m.r.id, m.r.maxp)
			}
		}
	case svPos:
		if m, ok := s.byAddr[key]; ok && len(d) >= 24 {
			m.r.slots[m.slot].seen = now
			f := func(o int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(d[o:])) }
			s.pos[key] = posInfo{f(8), f(12), f(16), f(20), d[5], now, binary.LittleEndian.Uint16(d[6:])}
		}
	case svLeave:
		s.dropAddr(key, "el jugador pulso salir / cancelar")
	case svPong:
		s.send(c, d, a)
	}
}

// el jugador cambio de IP:puerto: la sala sigue siendo suya
func (s *server) rebind(m member, p *peer, a *net.UDPAddr, c *net.UDPConn, key, why string) {
	old := p.key
	delete(s.byAddr, old)
	if ps, ok := s.pos[old]; ok {
		s.pos[key] = ps
		delete(s.pos, old)
	}
	p.key, p.addr, p.conn, p.seen = key, a, c, time.Now()
	s.byAddr[key] = m
	s.logf("sala %d: el jugador %d cambio de puerto %s -> %s (reconocido por %s), la sala sigue", m.r.id, m.slot+1, old, key, why)
}

func (s *server) warnOnce(key, f string, a ...interface{}) { // un aviso por origen cada 10 s (para no llenar la pantalla)
	if t, ok := s.warned[key]; ok && time.Since(t) < 10*time.Second {
		return
	}
	s.warned[key] = time.Now()
	if len(s.warned) > 5000 {
		s.warned = map[string]time.Time{}
	}
	s.logf("AVISO: "+f, a...)
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
					s.dropAddr(h.key, fmt.Sprintf("el anfitrion dejo de dar senales (%d s sin saber de el)", int(now.Sub(h.seen).Seconds())))
				}
				continue
			}
			for k, p := range r.slots {
				if k != 0 && now.Sub(p.seen) > guestTimeout {
					s.dropAddr(p.key, fmt.Sprintf("sin senales (%d s)", int(now.Sub(p.seen).Seconds())))
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

// Abre un socket por cada IP local (y 127.0.0.1). Asi la respuesta sale SIEMPRE con la misma IP a la que el jugador escribio,
// aunque el PC tenga varias tarjetas (WiFi, VPN, maquinas virtuales). Si el servidor respondia desde otra IP, el router del jugador la descartaba.
func (s *server) openAll() int {
	ips := append([]string{"127.0.0.1"}, localIPs()...)
	n := 0
	for _, ip := range ips {
		s.mu.Lock()
		_, have := s.conns[ip]
		s.mu.Unlock()
		if have {
			continue
		}
		addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ip, s.port))
		if err != nil {
			continue
		}
		c, err := net.ListenUDP("udp4", addr)
		if err != nil {
			s.logf("AVISO: no se pudo abrir %s:%d (%v)", ip, s.port, err)
			continue
		}
		s.mu.Lock()
		s.conns[ip] = c
		s.mu.Unlock()
		n++
		go s.readLoop(c)
	}
	return n
}

func (s *server) readLoop(c *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, a, e := c.ReadFromUDP(buf)
		if e != nil {
			continue
		}
		d := make([]byte, n)
		copy(d, buf[:n])
		s.handle(d, a, c)
	}
}

func openFirewall(port int) {
	if runtime.GOOS != "windows" {
		return
	}
	name := "name=HorrorServerUDP"
	exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", name).Run()
	err := exec.Command("netsh", "advfirewall", "firewall", "add", "rule", name, "dir=in", "action=allow", "protocol=UDP", fmt.Sprintf("localport=%d", port)).Run()
	exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name=HorrorVoiceTCP").Run()
	exec.Command("netsh", "advfirewall", "firewall", "add", "rule", "name=HorrorVoiceTCP", "dir=in", "action=allow", "protocol=TCP", fmt.Sprintf("localport=%d", voicePort)).Run()
	if err != nil {
		fmt.Println(" AVISO: no pude abrir el Firewall de Windows solo. Cierra y abre el programa con clic derecho > Ejecutar como administrador,")
		fmt.Println("        o permite horror_server.exe en las redes PRIVADAS Y PUBLICAS cuando Windows pregunte.")
	} else {
		fmt.Printf(" Firewall de Windows: regla creada para UDP %d.\n", port)
	}
}

func main() {
	port := flag.Int("port", 7778, "puerto UDP")
	maxRooms := flag.Int("max-rooms", 200, "maximo de salas")
	quiet := flag.Bool("quiet", false, "sin mensajes en pantalla")
	verbose := flag.Bool("v", false, "muestra cada senal de vida (diagnostico)")
	flag.Parse()

	s := &server{conns: map[string]*net.UDPConn{}, port: *port, rooms: map[uint32]*room{}, byAddr: map[string]member{}, byTok: map[uint32]member{}, lastSeq: map[uint32]uint32{}, warned: map[string]time.Time{}, pos: map[string]posInfo{}, seenIP: map[string]bool{}, nextID: 1, maxRooms: *maxRooms, quiet: *quiet, verbose: *verbose}
	fmt.Println("==============================================================")
	fmt.Println(" HORROR BETA - SERVIDOR DEDICADO (2 a 4 jugadores)  v3")
	openFirewall(*port)
	if s.openAll() == 0 {
		fmt.Println("ERROR: no se pudo abrir el puerto UDP", *port)
		fmt.Println("(Puede que otro programa lo use. Prueba con:  horror_server.exe -port 7779)")
		fmt.Println("Pulsa ENTER para cerrar...")
		bufio.NewReader(os.Stdin).ReadString('\n')
		return
	}
	fmt.Printf(" Escuchando en UDP puerto %d en estas IP (escribe UNA en el juego: ONLINE > IP SERVIDOR):\n", *port)
	s.mu.Lock()
	for ip := range s.conns {
		fmt.Println("     " + ip)
	}
	s.mu.Unlock()
	fmt.Println(" Mismo PC: 127.0.0.1   Misma WiFi: la 192.168...   VPN (Radmin/Hamachi): la IP de la VPN   Internet: tu IP publica + abrir UDP en el router.")
	fmt.Println(" Cuando un jugador consiga conectar veras aqui: LLEGO UN PAQUETE de <su IP>.")
	fmt.Println(" Si nunca aparece esa linea, el router o el firewall lo estan bloqueando.")
	fmt.Println(" Cierra esta ventana (o Ctrl+C) para apagar el servidor.")
	fmt.Println("==============================================================")
	go s.janitor()
	s.startVoice()
	fmt.Printf(" CHAT DE VOZ: en el celular abre (acepta el aviso de certificado) https://IP:%d con la misma IP que escribiste en el juego.\n", voicePort)
	for range time.Tick(10 * time.Second) { // si aparece una tarjeta nueva (VPN que se conecta despues) tambien se abre
		s.openAll()
	}
}
