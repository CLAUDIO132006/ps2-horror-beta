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
import asyncio
import struct
import time

SRV_MAGIC = 0x56535248        # 'HRSV' (mensajes jugador <-> servidor)
GAME_MAGIC = 0x52524F48       # 'HORR' (paquetes del juego: se reenvian tal cual)

SV_CREATE, SV_LIST, SV_JOIN, SV_KEEP, SV_LEAVE = 1, 2, 3, 4, 5
SV_ROOM, SV_LISTRES, SV_JOINED, SV_ERR, SV_PONG = 16, 17, 18, 19, 20
ERR_NOT_FOUND, ERR_FULL, ERR_SERVER_FULL = 1, 2, 3

ROOM_TIMEOUT = 15.0           # segundos sin paquetes de nadie de la sala -> se borra
GUEST_TIMEOUT = 6.0           # segundos sin paquetes de un invitado -> se libera su lugar
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
        self.transport.sendto(data, addr)

    # ------------------------------------------------------------------
    def drop_addr(self, addr):
        ent = self.by_addr.pop(addr, None)
        if not ent:
            return
        room, slot = ent
        if slot == 0:                                 # se va el anfitrion: la sala se cierra
            for k, a in list(room.slots.items()):
                if k != 0:
                    self.by_addr.pop(a, None)
            self.rooms.pop(room.id, None)
            self.log("sala %d cerrada (%s)" % (room.id, room.name))
        else:
            room.slots.pop(slot, None)
            room.seen.pop(slot, None)
            self.log("sala %d: el jugador %d salio" % (room.id, slot + 1))

    def err(self, addr, code):
        self.send(struct.pack("<IBBBB", SRV_MAGIC, SV_ERR, code, 0, 0), addr)

    # ------------------------------------------------------------------
    def datagram_received(self, data, addr):
        self.rx += 1
        if len(data) < 5:
            return
        magic = struct.unpack_from("<I", data, 0)[0]
        now = time.monotonic()

        if magic == GAME_MAGIC:                       # paquete del juego: reenviar a TODOS los demas de la sala
            ent = self.by_addr.get(addr)
            if not ent:
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
            self.send(struct.pack("<IBBBBI", SRV_MAGIC, SV_ROOM, maxp, 0, 0, room.id), addr)
            self.log("sala %d creada: %s  para %d jugadores (%s:%d)" % (room.id, name, maxp, addr[0], addr[1]))

        elif typ == SV_LIST:
            avail = [r for r in self.rooms.values()][-LIST_MAX:]
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
            self.send(struct.pack("<IBBBB", SRV_MAGIC, SV_JOINED, slot, room.maxp, room.players), addr)
            self.log("sala %d: entro el jugador %d (%s:%d)" % (room.id, slot + 1, addr[0], addr[1]))

        elif typ == SV_KEEP:
            ent = self.by_addr.get(addr)
            if ent:
                ent[0].seen[ent[1]] = now

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
                    self.drop_addr(room.host)
                    continue
                for k, a in list(room.slots.items()):
                    if k != 0 and now - room.seen.get(k, now) > GUEST_TIMEOUT:
                        self.drop_addr(a)
            if now - last_stat > 60.0:
                last_stat = now
                self.log("salas: %d   paquetes recibidos/enviados: %d/%d" % (len(self.rooms), self.rx, self.tx))


def main():
    ap = argparse.ArgumentParser(description="Servidor dedicado de HORROR BETA (lista de salas + relay UDP, 2 a 4 jugadores)")
    ap.add_argument("--host", default="0.0.0.0", help="direccion donde escuchar (por defecto todas)")
    ap.add_argument("--port", type=int, default=7778, help="puerto UDP (por defecto 7778)")
    ap.add_argument("--max-rooms", type=int, default=200)
    ap.add_argument("--quiet", action="store_true", help="sin mensajes en pantalla")
    a = ap.parse_args()
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)
    srv = Server(a.max_rooms, a.quiet)
    loop.run_until_complete(loop.create_datagram_endpoint(lambda: srv, local_addr=(a.host, a.port)))
    print("HORROR BETA - servidor dedicado escuchando en UDP %s:%d   (Ctrl+C para cerrar)" % (a.host, a.port))
    try:
        loop.run_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
