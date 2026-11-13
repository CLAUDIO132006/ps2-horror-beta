#!/usr/bin/env python3
# ============================================================================
#  HORROR BETA - SERVIDOR DEDICADO
#
#  Servidor UDP que corre en un PC / VPS / Raspberry y hace dos cosas:
#    1) Lista de SALAS por internet (PARTIDAS PUBLICAS en el juego).
#    2) RELAY: el servidor reenvia los paquetes entre TODOS los jugadores de la sala (2 a 4). Asi
#       funciona aunque esten detras de un router o de un emulador (nadie abre puertos: todos
#       solo se conectan HACIA el servidor).
#
#  Uso:        python3 horror_server.py                 (puerto UDP 7778)
#              python3 horror_server.py --port 7778 --max-rooms 200 --quiet
#  Requiere:   Python 3.8 o mas nuevo. Sin librerias externas.
#  Abre en el firewall / router el puerto UDP 7778 hacia el equipo del servidor.
#
#  El servidor NO simula el juego: cada consola sigue simulando (lockstep) y el servidor solo
#  reenvia los controles. Por eso es muy ligero (cientos de salas en un VPS pequeno).
# ============================================================================
import argparse
import os
import re
import socket
import subprocess
import asyncio
import struct
import time

SRV_MAGIC = 0x56535248        # 'HRSV' (mensajes jugador <-> servidor)
GAME_MAGIC = 0x52524F48       # 'HORR' (paquetes del juego: se reenvian tal cual)

SV_CREATE, SV_LIST, SV_JOIN, SV_KEEP, SV_LEAVE = 1, 2, 3, 4, 5
SV_ROOM, SV_LISTRES, SV_JOINED, SV_ERR, SV_PONG, SV_KEEPACK = 16, 17, 18, 19, 20, 21
ERR_NOT_FOUND, ERR_FULL, ERR_SERVER_FULL = 1, 2, 3

LIST_FRESH = 30.0              # en la lista solo salen salas cuyo anfitrion dio senales hace poco
ROOM_TIMEOUT = 90.0           # segundos sin paquetes de nadie de la sala -> se borra
GUEST_TIMEOUT = 25.0          # segundos sin paquetes de un invitado -> se libera su lugar
MAX_PLAYERS = 4
LIST_MAX = 6                  # el juego muestra hasta 6 salas


class Room:
    __slots__ = ("id", "name", "maxp", "slots", "seen", "created")

    def __init__(self, rid, name, host, maxp):
        now = time.monotonic()
        self.id, self.name, self.maxp = rid, name, maxp
        self.slots = {0: host}                        # numero de jugador (0 = anfitrion) -> (ip, puerto)
        self.seen = {0: now}                          # ultima senal de cada jugador
        self.created = now

    @property
    def host(self):
        return self.slots[0]

    @property
    def players(self):
        return len(self.slots)

    def free_slot(self):
        for k in range(1, self.maxp):
            if k not in self.slots:
                return k
        return -1


class Server(asyncio.DatagramProtocol):
    def __init__(self, max_rooms, quiet):
        self.max_rooms, self.quiet = max_rooms, quiet
        self.rooms = {}               # id -> Room
        self.by_addr = {}             # (ip, puerto) -> (Room, es_anfitrion)
        self.next_id = 1
        self.rx = self.tx = 0
        self.transport = None
        self.via = {}                 # (ip, puerto) -> socket por el que llego (se le responde por el mismo: misma IP de origen)
        self.seen_ip = set()
        self.by_tok = {}              # token de sesion -> (Room, slot): se reconoce al jugador aunque cambie de IP:puerto
        self.tok_of = {}              # (ip, puerto) -> token
        self.last_seq = {}            # token -> ultimo numero de orden recibido
        self.warned = {}
        self.verbose = False
        self.pos = {}                 # (ip, puerto) -> (x, z, rx, rz, capa, t)  posicion de cada jugador (chat de voz)

    # ------------------------------------------------------------------
    def connection_made(self, transport):
        self.transport = transport
        self.log("servidor listo")
        asyncio.get_event_loop().create_task(self.janitor())

    def log(self, msg):
        if not self.quiet:
            print(time.strftime("%H:%M:%S"), msg, flush=True)

    def send(self, data, addr):
        self.tx += 1
        (self.via.get(addr) or self.transport).sendto(data, addr)

    # ------------------------------------------------------------------
    def forget_tok(self, addr):
        t = self.tok_of.pop(addr, None)
        if t is not None:
            self.by_tok.pop(t, None)

    def rebind(self, room, slot, new, why):         # el jugador cambio de IP:puerto: la sala sigue siendo suya
        old = room.slots[slot]
        self.by_addr.pop(old, None)
        t = self.tok_of.pop(old, None)
        if old in self.pos:
            self.pos[new] = self.pos.pop(old)
        room.slots[slot] = new
        room.seen[slot] = time.monotonic()
        self.by_addr[new] = (room, slot)
        if t is not None:
            self.tok_of[new] = t
        if old in self.via:
            self.via[new] = self.via.get(new) or self.via[old]
        self.log("sala %d: el jugador %d cambio de puerto %s:%d -> %s:%d (reconocido por %s), la sala sigue" % (room.id, slot + 1, old[0], old[1], new[0], new[1], why))

    def warn_once(self, addr, msg):                   # un aviso por origen cada 10 s
        now = time.monotonic()
        if now - self.warned.get(addr, -99) < 10:
            return
        self.warned[addr] = now
        if len(self.warned) > 5000:
            self.warned.clear()
        self.log("AVISO: " + msg)

    def drop_addr(self, addr, why="el jugador pulso salir / cancelar"):
        ent = self.by_addr.pop(addr, None)
        self.pos.pop(addr, None)
        self.forget_tok(addr)
        if not ent:
            return
        room, slot = ent
        if slot == 0:                                 # se va el anfitrion: la sala se cierra
            for k, a in list(room.slots.items()):
                if k != 0:
                    self.by_addr.pop(a, None)
                    self.pos.pop(a, None)
                    self.forget_tok(a)
            self.rooms.pop(room.id, None)
            self.log("sala %d cerrada (%s): %s" % (room.id, room.name, why))
            for k, a in list(self.via.items()):
                if a is None:
                    self.via.pop(k, None)
        else:
            room.slots.pop(slot, None)
            room.seen.pop(slot, None)
            self.log("sala %d: el jugador %d salio" % (room.id, slot + 1))

    def err(self, addr, code):
        self.send(struct.pack("<IBBBB", SRV_MAGIC, SV_ERR, code, 0, 0), addr)

    # ------------------------------------------------------------------
    def datagram_received(self, data, addr, via=None):
        self.rx += 1
        if via is not None:
            self.via[addr] = via
            if addr[0] not in self.seen_ip:
                self.seen_ip.add(addr[0])
                self.log("LLEGO UN PAQUETE de %s hacia %s  (la conexion con esa IP funciona)" % (addr[0], via.get_extra_info('sockname')[0]))
        if len(data) < 5:
            return
        magic = struct.unpack_from("<I", data, 0)[0]
        now = time.monotonic()
        tok = seq = 0
        if magic == SRV_MAGIC and len(data) >= 40:
            tok, seq = struct.unpack_from("<II", data, 32)
        if tok and tok in self.by_tok:                # cambio de IP:puerto (emulador / NAT): se le reconoce por su token
            room, slot = self.by_tok[tok]
            cur = room.slots.get(slot)
            if cur is not None and cur != addr and (cur[0] == addr[0] or now - room.seen.get(slot, now) > 3.0):
                self.rebind(room, slot, addr, "token")

        if magic == GAME_MAGIC:                       # paquete del juego: reenviar a TODOS los demas de la sala
            ent = self.by_addr.get(addr)
            if not ent and len(data) > 28:            # puerto nuevo sin avisar: si de esa IP solo falta un jugador con ese numero (byte 28), es el
                cand = [(r, k) for r in self.rooms.values() for k, a in r.slots.items()
                        if k == data[28] and a[0] == addr[0] and now - r.seen.get(k, now) > 1.0]
                if len(cand) == 1:
                    self.rebind(cand[0][0], cand[0][1], addr, "IP y numero de jugador")
                    ent = self.by_addr.get(addr)
            if not ent:
                self.warn_once(addr, "paquete de juego de %s:%d que no esta en ninguna sala (la sala se cerro o el servidor se reinicio)" % addr)
                return
            room, slot = ent
            room.seen[slot] = now
            for k, a in room.slots.items():
                if k != slot:
                    self.send(data, a)
            return

        if magic != SRV_MAGIC:
            return
        typ = data[4]
        if tok:                                       # orden de los paquetes: un SALIR que llega tarde no cierra nada
            last = self.last_seq.get(tok)
            stale = last is not None and ((seq - last) & 0xFFFFFFFF) >= 0x80000000 or (last is not None and seq == last)
            if not stale:
                self.last_seq[tok] = seq
            if stale and typ == SV_LEAVE:
                self.log("(ignoro un SALIR atrasado de %s:%d)" % addr)
                return
            if len(self.last_seq) > 20000:
                self.last_seq = {tok: seq}

        if typ == SV_CREATE:
            maxp = data[5] if len(data) > 5 else 2
            maxp = max(2, min(MAX_PLAYERS, maxp or 2))
            old = self.by_addr.get(addr)
            if old and old[1] == 0:                   # repite el CREATE (se perdio la respuesta): misma sala
                old[0].seen[0] = now
                self.send(struct.pack("<IBBBBI", SRV_MAGIC, SV_ROOM, old[0].maxp, 0, 0, old[0].id), addr)
                return
            if old:
                self.drop_addr(addr)
            if len(self.rooms) >= self.max_rooms:
                self.err(addr, ERR_SERVER_FULL)
                return
            name = data[8:32].split(b"\0")[0].decode("ascii", "replace")[:23] or "SALA"
            room = Room(self.next_id, name, addr, maxp)
            self.next_id = (self.next_id % 0x7FFFFFFF) + 1
            self.rooms[room.id] = room
            self.by_addr[addr] = (room, 0)
            if tok:
                self.by_tok[tok] = (room, 0)
                self.tok_of[addr] = tok
            self.send(struct.pack("<IBBBBI", SRV_MAGIC, SV_ROOM, maxp, 0, 0, room.id), addr)
            self.log("sala %d creada: %s  para %d jugadores (%s:%d)" % (room.id, name, maxp, addr[0], addr[1]))

        elif typ == SV_LIST:
            avail = [r for r in self.rooms.values() if now - r.seen.get(0, 0) < LIST_FRESH][-LIST_MAX:]
            avail.sort(key=lambda r: (r.players >= r.maxp, -r.created))
            out = struct.pack("<IBB", SRV_MAGIC, SV_LISTRES, len(avail)) + b"\0\0"
            for r in avail:
                # id(4) + jugadores(1) + nombre (23 bytes) + relleno; el ultimo byte de la entrada = capacidad de la sala
                ent = r.name.encode("ascii", "replace")[:23].ljust(26, b"\0") + bytes([r.maxp])
                out += struct.pack("<IB", r.id, r.players) + ent
            self.send(out, addr)

        elif typ == SV_JOIN:
            if len(data) < 12:
                return
            rid = struct.unpack_from("<I", data, 8)[0]
            old = self.by_addr.get(addr)
            if old and old[1] != 0 and old[0].id == rid:        # repite el JOIN: ya esta dentro
                old[0].seen[old[1]] = now
                self.send(struct.pack("<IBBBB", SRV_MAGIC, SV_JOINED, old[1], old[0].maxp, old[0].players), addr)
                return
            room = self.rooms.get(rid)
            if not room:
                self.err(addr, ERR_NOT_FOUND)
                return
            if old:
                self.drop_addr(addr)
            slot = room.free_slot()
            if slot < 0:
                self.err(addr, ERR_FULL)
                return
            room.slots[slot] = addr
            room.seen[slot] = now
            self.by_addr[addr] = (room, slot)
            if tok:
                self.by_tok[tok] = (room, slot)
                self.tok_of[addr] = tok
            self.send(struct.pack("<IBBBB", SRV_MAGIC, SV_JOINED, slot, room.maxp, room.players), addr)
            self.log("sala %d: entro el jugador %d (%s:%d)" % (room.id, slot + 1, addr[0], addr[1]))

        elif typ == SV_KEEP:
            ent = self.by_addr.get(addr)
            if ent:
                if self.verbose:
                    self.log("senal de vida de %s:%d (sala %d, jugador %d)" % (addr[0], addr[1], ent[0].id, ent[1] + 1))
                self.send(struct.pack("<IBBBBI", SRV_MAGIC, SV_KEEPACK, 1, ent[0].players, ent[1], ent[0].id), addr)
            else:
                self.warn_once(addr, "senal de vida de %s:%d: no esta en ninguna sala (se le avisa para que la cree otra vez)" % addr)
                self.send(struct.pack("<IBBBBI", SRV_MAGIC, SV_KEEPACK, 0, 0, 0, 0), addr)
            if ent:
                ent[0].seen[ent[1]] = now
                if ent[1] == 0 and len(data) > 5 and 2 <= data[5] <= MAX_PLAYERS and data[5] >= ent[0].players:
                    if ent[0].maxp != data[5]:                          # el anfitrion cambio los jugadores de la sala
                        ent[0].maxp = data[5]
                        self.log("sala %d: ahora para %d jugadores" % (ent[0].id, data[5]))

        elif typ == SV_POS:
            ent = self.by_addr.get(addr)
            if ent and len(data) >= 24:
                ent[0].seen[ent[1]] = now
                x, z, rx, rz = struct.unpack_from("<ffff", data, 8)
                self.pos[addr] = (x, z, rx, rz, data[5], now, struct.unpack_from('<H', data, 6)[0])

        elif typ == SV_LEAVE:
            self.drop_addr(addr)

        elif typ == SV_PONG:                          # (ping de prueba: responde igual)
            self.send(data, addr)

    # ------------------------------------------------------------------
    async def janitor(self):                          # limpia salas e invitados que dejaron de dar senales
        last_stat = time.monotonic()
        while True:
            await asyncio.sleep(1.0)
            now = time.monotonic()
            for room in list(self.rooms.values()):
                if now - room.seen.get(0, 0) > ROOM_TIMEOUT:
                    self.drop_addr(room.host, "el anfitrion dejo de dar senales (%d s sin saber de el)" % int(now - room.seen.get(0, 0)))
                    continue
                for k, a in list(room.slots.items()):
                    if k != 0 and now - room.seen.get(k, now) > GUEST_TIMEOUT:
                        self.drop_addr(a)
            if now - last_stat > 60.0:
                last_stat = now
                self.log("salas: %d   paquetes recibidos/enviados: %d/%d" % (len(self.rooms), self.rx, self.tx))


class Port(asyncio.DatagramProtocol):
    """Un socket por cada IP del PC: la respuesta sale SIEMPRE desde la IP a la que el jugador escribio."""
    def __init__(self, srv):
        self.srv, self.transport = srv, None

    def connection_made(self, transport):
        self.transport = transport

    def datagram_received(self, data, addr):
        self.srv.datagram_received(data, addr, self.transport)


def local_ips():
    ips = set()
    try:
        for r in socket.getaddrinfo(socket.gethostname(), None, socket.AF_INET):
            ips.add(r[4][0])
    except Exception:
        pass
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.connect(("8.8.8.8", 80))                      # (no envia nada: solo averigua la IP de salida)
        ips.add(s.getsockname()[0])
        s.close()
    except Exception:
        pass
    try:                                                 # lee tambien todas las tarjetas (VPN, WiFi, virtuales)
        cmd = ["ipconfig"] if os.name == "nt" else ["ip", "-4", "addr"]
        out = subprocess.run(cmd, capture_output=True, text=True, errors="ignore", timeout=5).stdout
        pat = r"IPv4[^\d]*(\d+\.\d+\.\d+\.\d+)" if os.name == "nt" else r"inet (\d+\.\d+\.\d+\.\d+)"
        ips.update(re.findall(pat, out))
    except Exception:
        pass
    ips.add("127.0.0.1")
    return sorted(i for i in ips if not i.startswith("0.") and not i.startswith("169.254."))


async def open_all(loop, srv, port, opened):
    for ip in local_ips():
        if ip in opened:
            continue
        try:
            await loop.create_datagram_endpoint(lambda: Port(srv), local_addr=(ip, port))
            opened.add(ip)
            srv.log("escuchando en %s:%d" % (ip, port))
        except OSError as e:
            srv.log("AVISO: no se pudo abrir %s:%d (%s)" % (ip, port, e))


async def refresh(loop, srv, port, opened):             # si se conecta una VPN despues, tambien se abre
    while True:
        await asyncio.sleep(10.0)
        await open_all(loop, srv, port, opened)


# ============================================================================
#  CHAT DE VOZ DE PROXIMIDAD (celular como microfono)
#  El juego manda su posicion (paquete SV_POS). El celular abre https://IP:8443 y manda su microfono por WebSocket.
#  El servidor empareja celular y jugador por la IP y reenvia la voz a los demas de la sala con volumen y
#  panoramizacion segun la distancia y hacia donde mira cada uno. Sin librerias externas (usa openssl para el certificado).
# ============================================================================
import base64
import hashlib
import json
import math
import ssl

SV_POS = 6
VOICE_PORT = 8443
VOICE_RANGE = 25.0
POS_STALE = 3.0
HERE = os.path.dirname(os.path.abspath(__file__))


def make_cert():
    cert, key = os.path.join(HERE, "voice_cert.pem"), os.path.join(HERE, "voice_key.pem")
    if os.path.exists(cert) and os.path.exists(key):
        return cert, key
    ips = ",".join("IP:" + i for i in local_ips())
    try:
        subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes",
                        "-keyout", key, "-out", cert, "-days", "1825", "-subj", "/CN=horror-voice",
                        "-addext", "subjectAltName=DNS:localhost," + ips], check=True, capture_output=True, timeout=30)
        return cert, key
    except Exception:
        pass
    try:                                              # alternativa: libreria 'cryptography' si esta instalada
        import datetime
        import ipaddress
        from cryptography import x509
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import ec
        from cryptography.x509.oid import NameOID
        k = ec.generate_private_key(ec.SECP256R1())
        nm = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "horror-voice")])
        san = [x509.DNSName("localhost")] + [x509.IPAddress(ipaddress.ip_address(i)) for i in local_ips()]
        c = (x509.CertificateBuilder().subject_name(nm).issuer_name(nm).public_key(k.public_key())
             .serial_number(x509.random_serial_number()).not_valid_before(datetime.datetime.utcnow() - datetime.timedelta(hours=1))
             .not_valid_after(datetime.datetime.utcnow() + datetime.timedelta(days=1825))
             .add_extension(x509.SubjectAlternativeName(san), False).sign(k, hashes.SHA256()))
        open(cert, "wb").write(c.public_bytes(serialization.Encoding.PEM))
        open(key, "wb").write(k.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
        return cert, key
    except Exception:
        return None, None


class VClient:
    def __init__(self, reader, writer, ip):
        self.reader, self.writer, self.ip = reader, writer, ip
        self.room, self.slot = None, 0
        self.want = None
        self.asked = False
        self.status = ""
        self.code = 0

    def send(self, op, payload):
        n = len(payload)
        h = bytes([0x80 | op, n]) if n < 126 else bytes([0x80 | op, 126, n >> 8, n & 255])
        try:
            self.writer.write(h + payload)
        except Exception:
            pass


class Voice:
    def __init__(self, srv):
        self.srv = srv
        self.clients = set()

    def candidates(self, ip, code=0):
        out = []
        now = time.monotonic()
        for a, (room, slot) in self.srv.by_addr.items():
            if code:
                p = self.srv.pos.get(a)
                if p and p[6] == code and now - p[5] < 10.0:
                    out.append((room, slot))
            elif a[0] == ip:
                out.append((room, slot))
        return out

    async def pair_loop(self, c):
        while True:
            await asyncio.sleep(1.0)
            s = self.srv
            if c.room is not None and (s.rooms.get(c.room.id) is not c.room or c.slot not in c.room.slots):
                c.room = None
            msg = None
            if c.room is None:
                cands = self.candidates(c.ip, c.code)
                pick = None
                if len(cands) == 1:
                    pick = cands[0]
                elif len(cands) > 1 and c.want:
                    for r, sl in cands:
                        if r.id == c.want[0] and sl == c.want[1]:
                            pick = (r, sl)
                if pick:
                    c.room, c.slot, c.asked, c.status = pick[0], pick[1], False, "ok"
                    msg = {"t": "ok", "room": c.room.id, "slot": c.slot}
                elif len(cands) > 1:
                    if not c.asked:
                        c.asked = True
                        msg = {"t": "pick", "opts": [{"room": r.id, "slot": sl} for r, sl in cands]}
                elif c.status != "wait":
                    c.status = "wait"
                    msg = {"t": "wait"}
            if msg:
                c.send(1, json.dumps(msg).encode())

    def relay(self, c, pcm):
        if not pcm or len(pcm) > 4000 or c.room is None:
            return
        room, s = c.room, self.srv
        now = time.monotonic()
        spa = room.slots.get(c.slot)
        sp = s.pos.get(spa)
        if not sp or now - sp[5] > POS_STALE:
            return
        for o in self.clients:
            if o is c or o.room is not room:
                continue
            lpa = room.slots.get(o.slot)
            lp = s.pos.get(lpa)
            if not lp or now - lp[5] > POS_STALE:
                continue
            dx, dz = sp[0] - lp[0], sp[1] - lp[1]
            d = math.hypot(dx, dz)
            flags, lay = 0, 1.0
            if sp[4] != lp[4]:
                lay, flags, d = 0.35, 1, d + 4
            if d >= VOICE_RANGE:
                continue
            g = (1 - d / VOICE_RANGE) ** 2 * lay
            pan = 0.0
            if d > 0.6:
                rl = math.hypot(lp[2], lp[3])
                if rl > 0.001:
                    pan = (dx * lp[2] + dz * lp[3]) / (d * rl)
                pan *= min(1.0, (d - 0.6) / 2)
            o.send(2, bytes([c.slot, int(round(g * 255)), int(round(pan * 127)) + 128, flags]) + pcm)

    async def handle(self, reader, writer):
        try:
            head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 10)
        except Exception:
            writer.close()
            return
        lines = head.decode("latin1").split("\r\n")
        hd = {}
        for ln in lines[1:]:
            if ":" in ln:
                k, v = ln.split(":", 1)
                hd[k.strip().lower()] = v.strip()
        path = lines[0].split(" ")[1] if len(lines[0].split(" ")) > 1 else "/"
        if not path.startswith("/ws"):
            try:
                body = open(os.path.join(HERE, "voice.html"), "rb").read()
            except Exception:
                body = b"falta voice.html junto a horror_server.py"
            writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n" % len(body) + body)
            await writer.drain()
            writer.close()
            return
        key = hd.get("sec-websocket-key", "")
        acc = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        writer.write(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n" % acc).encode())
        ip = (writer.get_extra_info("peername") or ("?",))[0]
        if ip.startswith("::ffff:"):
            ip = ip[7:]
        c = VClient(reader, writer, ip)
        self.clients.add(c)
        self.srv.log("VOZ: se conecto un celular desde %s" % ip)
        task = asyncio.get_event_loop().create_task(self.pair_loop(c))
        try:
            while True:
                h2 = await asyncio.wait_for(reader.readexactly(2), 30)
                op, n = h2[0] & 15, h2[1] & 127
                if n == 126:
                    n = int.from_bytes(await reader.readexactly(2), "big")
                elif n == 127:
                    n = int.from_bytes(await reader.readexactly(8), "big")
                if n > 65536:
                    break
                mk = await reader.readexactly(4) if h2[1] & 128 else b"\0\0\0\0"
                p = bytearray(await reader.readexactly(n))
                if h2[1] & 128:
                    for i in range(n):
                        p[i] ^= mk[i & 3]
                if op == 8:
                    break
                if op == 9:
                    c.send(10, bytes(p))
                elif op == 1:
                    t = p.decode("utf8", "ignore")
                    if t.startswith("code:"):
                        try:
                            c.code = int(t[5:].strip())
                        except ValueError:
                            c.code = 0
                        c.room, c.status, c.asked = None, "", False
                    elif t.startswith("sel:"):
                        f = t.split(":")
                        if len(f) == 3:
                            c.want, c.room = (int(f[1]), int(f[2])), None
                elif op == 2:
                    self.relay(c, bytes(p))
        except Exception:
            pass
        finally:
            task.cancel()
            self.clients.discard(c)
            self.srv.log("VOZ: se desconecto %s" % ip)
            try:
                writer.close()
            except Exception:
                pass


async def start_voice(srv):
    cert, key = make_cert()
    if not cert:
        print("AVISO: chat de voz desactivado: instala openssl (o 'pip install cryptography') o pon voice_cert.pem y voice_key.pem junto al programa.")
        return
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(cert, key)
    v = Voice(srv)
    try:
        await asyncio.start_server(v.handle, "0.0.0.0", VOICE_PORT, ssl=ctx)
        print("CHAT DE VOZ: en el celular abre (acepta el aviso del certificado) https://IP:%d con la misma IP que escribiste en el juego. Abre TCP %d en el firewall." % (VOICE_PORT, VOICE_PORT))
    except OSError as e:
        print("AVISO: chat de voz desactivado (puerto TCP %d ocupado?): %s" % (VOICE_PORT, e))


def main():
    ap = argparse.ArgumentParser(description="Servidor dedicado de HORROR BETA (lista de salas + relay UDP, 2 a 4 jugadores)")
    ap.add_argument("--host", default="", help="escuchar solo en esta IP (por defecto: en todas las IP del PC, cada una con su socket)")
    ap.add_argument("--port", type=int, default=7778, help="puerto UDP (por defecto 7778)")
    ap.add_argument("--max-rooms", type=int, default=200)
    ap.add_argument("--quiet", action="store_true", help="sin mensajes en pantalla")
    ap.add_argument("-v", "--verbose", action="store_true", help="muestra cada senal de vida (diagnostico)")
    a = ap.parse_args()
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)
    srv = Server(a.max_rooms, a.quiet)
    srv.verbose = a.verbose
    srv.transport = None
    opened = set()
    if a.host:
        loop.run_until_complete(loop.create_datagram_endpoint(lambda: Port(srv), local_addr=(a.host, a.port)))
        opened.add(a.host)
    else:
        loop.run_until_complete(open_all(loop, srv, a.port, opened))
        loop.create_task(refresh(loop, srv, a.port, opened))
    if not opened:
        print("ERROR: no se pudo abrir el puerto UDP %d (otro programa lo usa?). Prueba con --port 7779" % a.port)
        return
    srv.transport = next(iter(srv.via.values()), None)
    loop.create_task(srv.janitor())
    loop.run_until_complete(start_voice(srv))
    print("HORROR BETA - servidor dedicado v3 escuchando en UDP %d  en estas IP: %s   (Ctrl+C para cerrar)" % (a.port, ", ".join(sorted(opened))))
    print("Escribe UNA de esas IP en el juego (ONLINE > IP SERVIDOR). VPN (ZeroTier/Radmin): la IP de la VPN. Internet: tu IP publica + abrir UDP %d en el router." % a.port)
    print("Cuando un jugador conecte vera aqui: LLEGO UN PAQUETE de <su IP>.  Si nunca sale, el router o el firewall lo bloquean.")
    try:
        loop.run_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
