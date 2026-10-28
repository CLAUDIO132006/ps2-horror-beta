#!/usr/bin/env python3
# ============================================================================
#  HORROR BETA - SERVIDOR DEDICADO
#
#  Servidor UDP que corre en un PC / VPS / Raspberry y hace dos cosas:
#    1) Lista de SALAS por internet (PARTIDAS PUBLICAS en el juego).
#    2) RELAY: el servidor reenvia los paquetes entre el anfitrion y el invitado. Asi funciona
#       aunque los dos esten detras de un router o de un emulador (nadie abre puertos: los dos
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
GUEST_TIMEOUT = 6.0           # segundos sin paquetes del invitado -> se libera su lugar
LIST_MAX = 6                  # el juego muestra hasta 6 salas


class Room:
    __slots__ = ("id", "name", "host", "guest", "t_host", "t_guest", "created")

    def __init__(self, rid, name, host):
        now = time.monotonic()
        self.id, self.name, self.host, self.guest = rid, name, host, None
        self.t_host = self.t_guest = self.created = now

    @property
    def players(self):
        return 2 if self.guest else 1


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
        room, is_host = ent
        if is_host:                                   # se va el anfitrion: la sala se cierra
            if room.guest:
                self.by_addr.pop(room.guest, None)
            self.rooms.pop(room.id, None)
            self.log("sala %d cerrada (%s)" % (room.id, room.name))
        else:
            room.guest = None
            self.log("sala %d: el invitado salio" % room.id)

    def err(self, addr, code):
        self.send(struct.pack("<IBBBB", SRV_MAGIC, SV_ERR, code, 0, 0), addr)

    # ------------------------------------------------------------------
    def datagram_received(self, data, addr):
        self.rx += 1
        if len(data) < 5:
            return
        magic = struct.unpack_from("<I", data, 0)[0]
        now = time.monotonic()

        if magic == GAME_MAGIC:                       # paquete del juego: reenviar al otro de la sala
            ent = self.by_addr.get(addr)
            if not ent:
                return
            room, is_host = ent
            if is_host:
                room.t_host = now
                if room.guest:
                    self.send(data, room.guest)
            else:
                room.t_guest = now
                self.send(data, room.host)
            return

        if magic != SRV_MAGIC:
            return
        typ = data[4]

        if typ == SV_CREATE:
            old = self.by_addr.get(addr)
            if old and old[1]:                        # repite el CREATE (se perdio la respuesta): misma sala
                old[0].t_host = now
                self.send(struct.pack("<IBBBBI", SRV_MAGIC, SV_ROOM, 0, 0, 0, old[0].id), addr)
                return
            if old:
                self.drop_addr(addr)
            if len(self.rooms) >= self.max_rooms:
                self.err(addr, ERR_SERVER_FULL)
                return
            name = data[8:32].split(b"\0")[0].decode("ascii", "replace")[:23] or "SALA"
            room = Room(self.next_id, name, addr)
            self.next_id = (self.next_id % 0x7FFFFFFF) + 1
            self.rooms[room.id] = room
            self.by_addr[addr] = (room, True)
            self.send(struct.pack("<IBBBBI", SRV_MAGIC, SV_ROOM, 0, 0, 0, room.id), addr)
            self.log("sala %d creada: %s  (%s:%d)" % (room.id, name, addr[0], addr[1]))

        elif typ == SV_LIST:
            avail = [r for r in self.rooms.values()][-LIST_MAX:]
            avail.sort(key=lambda r: (r.players >= 2, -r.created))
            out = struct.pack("<IBB", SRV_MAGIC, SV_LISTRES, len(avail)) + b"\0\0"
            for r in avail:
                out += struct.pack("<IB", r.id, r.players) + r.name.encode("ascii", "replace")[:23].ljust(27, b"\0")
            self.send(out, addr)

        elif typ == SV_JOIN:
            if len(data) < 12:
                return
            rid = struct.unpack_from("<I", data, 8)[0]
            old = self.by_addr.get(addr)
            if old and not old[1] and old[0].id == rid:         # repite el JOIN: ya esta dentro
                old[0].t_guest = now
                self.send(struct.pack("<IBBBB", SRV_MAGIC, SV_JOINED, 0, 0, 0), addr)
                return
            room = self.rooms.get(rid)
            if not room:
                self.err(addr, ERR_NOT_FOUND)
                return
            if room.guest and room.guest != addr:
                self.err(addr, ERR_FULL)
                return
            if old:
                self.drop_addr(addr)
            room.guest = addr
            room.t_guest = now
            self.by_addr[addr] = (room, False)
            self.send(struct.pack("<IBBBB", SRV_MAGIC, SV_JOINED, 0, 0, 0), addr)
            self.log("sala %d: entro un invitado (%s:%d)" % (room.id, addr[0], addr[1]))

        elif typ == SV_KEEP:
            ent = self.by_addr.get(addr)
            if ent:
                if ent[1]:
                    ent[0].t_host = now
                else:
                    ent[0].t_guest = now

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
                if now - room.t_host > ROOM_TIMEOUT:
                    self.drop_addr(room.host)
                elif room.guest and now - room.t_guest > GUEST_TIMEOUT:
                    g = room.guest
                    self.drop_addr(g)
            if now - last_stat > 60.0:
                last_stat = now
                self.log("salas: %d   paquetes recibidos/enviados: %d/%d" % (len(self.rooms), self.rx, self.tx))


def main():
    ap = argparse.ArgumentParser(description="Servidor dedicado de HORROR BETA (lista de salas + relay UDP)")
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
