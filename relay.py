#!/usr/bin/env python3
# Relay UDP para el modo SALA de "Horror Beta".
# Reenvia los paquetes entre los 2 jugadores que entran a la misma sala (codigo de 4 digitos).
# Uso:  python3 relay.py [puerto]      (puerto UDP por defecto: 7778)
import socket, sys, time

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 7778
SLOT_TIMEOUT = 30.0            # segundos sin noticias de un jugador: se libera su lugar

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.bind(("0.0.0.0", PORT))
sock.settimeout(1.0)
rooms = {}                     # codigo(4 bytes) -> [[addr, ultima_vez], [addr, ultima_vez]]  (lugar 0 = anfitrion)


def status(room, i):
    other = rooms[room][1 - i]
    sock.sendto(b"RLY" + bytes([10, i, 1 if other else 0, 0, 0]), rooms[room][i][0])


print("Relay escuchando en UDP %d" % PORT, flush=True)
last_clean = time.time()
while True:
    try:
        data, addr = sock.recvfrom(2048)
    except socket.timeout:
        data = None
    now = time.time()
    if now - last_clean > 2.0:                       # liberar lugares de quien dejo de mandar
        last_clean = now
        for room in list(rooms):
            for i in (0, 1):
                if rooms[room][i] and now - rooms[room][i][1] > SLOT_TIMEOUT:
                    rooms[room][i] = None
            if not rooms[room][0] and not rooms[room][1]:
                del rooms[room]
    if not data or len(data) < 8 or data[:3] != b"RLY":
        continue
    kind, room = data[3], bytes(data[4:8])
    slots = rooms.setdefault(room, [None, None])
    me = next((i for i in (0, 1) if slots[i] and slots[i][0] == addr), None)
    if me is None:
        if kind == 3:
            continue
        me = next((i for i in (0, 1) if slots[i] is None), None)
        if me is None:                                # sala llena
            sock.sendto(b"RLY" + bytes([12, 0, 0, 0, 0]), addr)
            continue
        slots[me] = [addr, now]
        print("sala %s: jugador %d entra desde %s:%d" % (room.hex(), me, addr[0], addr[1]), flush=True)
        if slots[1 - me]:
            status(room, 1 - me)                      # avisar al otro que ya llego alguien
    slots[me][1] = now
    if kind == 1:
        status(room, me)
    elif kind == 2:
        other = slots[1 - me]
        if other:
            sock.sendto(b"RLY" + bytes([11]) + room + data[8:], other[0])
    elif kind == 3:
        slots[me] = None
        print("sala %s: jugador %d sale" % (room.hex(), me), flush=True)
