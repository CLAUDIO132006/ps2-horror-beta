// ============================================================
//  HORROR BETA 0.1  -  PS2 homebrew (PS2SDK + gsKit)
//  Terror en primera persona 3D. Compila a .ELF
//
//  Controles (DualShock 2):
//    Stick izq  : moverse
//    Stick der  : mirar (D-pad izq/der tambien gira)
//    R1         : correr
//    L1         : linterna ON/OFF
//    START      : reiniciar
//
//  Objetivo: recoge las 3 notas amarillas y llega a la
//  luz verde de la salida. Algo te esta buscando...
// ============================================================
#include <tamtypes.h>
#include <kernel.h>
#include <sifrpc.h>
#include <loadfile.h>
#include <libpad.h>
#include <gsKit.h>
#include <dmaKit.h>
#include <math.h>
#include <stdio.h>
#include <string.h>

// ---------------- Mapa ----------------
#define MW 16
#define MH 14
#define CS 4.0f      // tamano de celda
#define WH 3.0f      // altura de pared
#define EYE 1.5f     // altura de ojos

static const char *map[MH] = {
    "################",
    "#S.....#.......#",
    "#.####.#.#####.#",
    "#.#....#.#...#.#",
    "#.#.####.#.#.#.#",
    "#.#......#.#...#",
    "#.########.#####",
    "#..........#...#",
    "######.#####.#.#",
    "#....#.....#.#.#",
    "#.##.#####.#.#.#",
    "#.#......#.#...#",
    "#.######.#.#####",
    "#........#....E#"
};

static const int note_cell[3][2] = { {5, 5}, {12, 3}, {8, 9} };

// ---------------- Estado global ----------------
static GSGLOBAL *gsGlobal;
static char padBuf[256] __attribute__((aligned(64)));

static float px, pz, yaw;
static float fx, fz, rx, rz;       // base de camara
static float amb_r = 9.0f;         // radio de luz ambiente (parpadea)
static int   flash_on = 1;
static float W, H, FOCAL;

static float sx_, sz_;             // monstruo
static int   notes_got[3];
static int   state;                // 0 jugando, 1 atrapado, 2 victoria
static int   state_timer;
static int   frame_no;
static int   dist_map[MH][MW];

#define NEAR_Z 0.2f
#define FAR_Z  40.0f
#define BLACK  GS_SETREG_RGBAQ(0x00, 0x00, 0x00, 0x80, 0x00)

// ---------------- Colision ----------------
static int solid(float x, float z)
{
    int cx = (int)floorf(x / CS), cz = (int)floorf(z / CS);
    if (cx < 0 || cz < 0 || cx >= MW || cz >= MH) return 1;
    return map[cz][cx] == '#';
}

static int blocked(float x, float z, float r)
{
    return solid(x - r, z - r) || solid(x + r, z - r) ||
           solid(x - r, z + r) || solid(x + r, z + r);
}

// ---------------- Pipeline 3D ----------------
typedef struct { float x, y, z, r, g, b; } V;

// Transforma un punto del mundo a espacio de camara y le aplica luz
static V mk(float wx, float wy, float wz, const float *c, float emit)
{
    V v;
    float dx = wx - px, dz = wz - pz;
    v.x = dx * rx + dz * rz;
    v.z = dx * fx + dz * fz;
    v.y = wy - EYE;

    float d  = sqrtf(v.x * v.x + v.y * v.y + v.z * v.z);
    float ca = (d > 0.001f) ? v.z / d : 0.0f;

    float amb = 1.0f - d / amb_r;
    if (amb < 0) amb = 0;
    amb *= amb;

    float fl = 0.0f;
    if (flash_on && ca > 0.8f) {
        fl = (ca - 0.8f) / 0.2f;
        float fd = 1.0f - d / 24.0f;
        if (fd < 0) fd = 0;
        fl *= fd;
    }

    float L = 0.03f + amb * 0.5f + fl * 0.9f + emit;
    if (L > 1.0f) L = 1.0f;
    v.r = c[0] * L; v.g = c[1] * L; v.b = c[2] * L;
    return v;
}

static V lerpv(V a, V b, float t)
{
    V o;
    o.x = a.x + (b.x - a.x) * t; o.y = a.y + (b.y - a.y) * t;
    o.z = a.z + (b.z - a.z) * t; o.r = a.r + (b.r - a.r) * t;
    o.g = a.g + (b.g - a.g) * t; o.b = a.b + (b.b - a.b) * t;
    return o;
}

static float clampf(float v, float lo, float hi)
{
    return v < lo ? lo : (v > hi ? hi : v);
}

static void emit_tri(V a, V b, V c)
{
    V t[3] = { a, b, c };
    float X[3], Y[3];
    int Z[3];
    u64 col[3];
    for (int i = 0; i < 3; i++) {
        X[i] = clampf(W * 0.5f + t[i].x / t[i].z * FOCAL, -1000.0f, W + 1000.0f);
        Y[i] = clampf(H * 0.5f - t[i].y / t[i].z * FOCAL, -1000.0f, H + 1000.0f);
        float zn = clampf(t[i].z, NEAR_Z, FAR_Z) / FAR_Z;
        Z[i] = (int)((1.0f - zn) * 16777000.0f);   // mas cerca = mas grande
        col[i] = GS_SETREG_RGBAQ((int)clampf(t[i].r, 0, 255),
                                 (int)clampf(t[i].g, 0, 255),
                                 (int)clampf(t[i].b, 0, 255), 0x80, 0x00);
    }
    gsKit_prim_triangle_gouraud_3d(gsGlobal,
        X[0], Y[0], Z[0], X[1], Y[1], Z[1], X[2], Y[2], Z[2],
        col[0], col[1], col[2]);
}

// Triangulo en espacio de camara, recortado contra el plano cercano
static void tri_cam(V a, V b, V c)
{
    V in[3] = { a, b, c };
    V out[4];
    int n = 0;
    for (int i = 0; i < 3; i++) {
        V cur = in[i], nxt = in[(i + 1) % 3];
        int ci = cur.z >= NEAR_Z, ni = nxt.z >= NEAR_Z;
        if (ci) out[n++] = cur;
        if (ci != ni) {
            float t = (NEAR_Z - cur.z) / (nxt.z - cur.z);
            out[n++] = lerpv(cur, nxt, t);
        }
    }
    if (n >= 3) emit_tri(out[0], out[1], out[2]);
    if (n == 4) emit_tri(out[0], out[2], out[3]);
}

// Quad subdividido n x n (para que la luz por vertice se vea suave)
static void quad_sub(const float p[4][3], const float *col, int n, float emit)
{
    V g[4][4];
    for (int j = 0; j <= n; j++) {
        for (int i = 0; i <= n; i++) {
            float u = (float)i / n, v = (float)j / n;
            float ax = p[0][0] + (p[1][0] - p[0][0]) * u;
            float ay = p[0][1] + (p[1][1] - p[0][1]) * u;
            float az = p[0][2] + (p[1][2] - p[0][2]) * u;
            float bx = p[3][0] + (p[2][0] - p[3][0]) * u;
            float by = p[3][1] + (p[2][1] - p[3][1]) * u;
            float bz = p[3][2] + (p[2][2] - p[3][2]) * u;
            g[j][i] = mk(ax + (bx - ax) * v, ay + (by - ay) * v,
                         az + (bz - az) * v, col, emit);
        }
    }
    for (int j = 0; j < n; j++)
        for (int i = 0; i < n; i++) {
            tri_cam(g[j][i], g[j][i + 1], g[j + 1][i + 1]);
            tri_cam(g[j][i], g[j + 1][i + 1], g[j + 1][i]);
        }
}

// Caja (4 lados + tapa) para el monstruo, notas y salida
static void box(float cx, float cz, float w, float y0, float y1,
                const float *col, float emit)
{
    float x0 = cx - w, x1 = cx + w, z0 = cz - w, z1 = cz + w;
    float q[4][3];
#define Q(a,b,c,d,e,f,g,h,i,j,k,l) \
    q[0][0]=a;q[0][1]=b;q[0][2]=c; q[1][0]=d;q[1][1]=e;q[1][2]=f; \
    q[2][0]=g;q[2][1]=h;q[2][2]=i; q[3][0]=j;q[3][1]=k;q[3][2]=l; \
    quad_sub(q, col, 1, emit);
    Q(x0,y0,z0, x1,y0,z0, x1,y1,z0, x0,y1,z0)
    Q(x0,y0,z1, x1,y0,z1, x1,y1,z1, x0,y1,z1)
    Q(x0,y0,z0, x0,y0,z1, x0,y1,z1, x0,y1,z0)
    Q(x1,y0,z0, x1,y0,z1, x1,y1,z1, x1,y1,z0)
    Q(x0,y1,z0, x1,y1,z0, x1,y1,z1, x0,y1,z1)
#undef Q
}

// ---------------- Dibujo del nivel ----------------
static void draw_level(void)
{
    int pcx = (int)(px / CS), pcz = (int)(pz / CS);
    const int R = 5;
    for (int cz = pcz - R; cz <= pcz + R; cz++) {
        for (int cx = pcx - R; cx <= pcx + R; cx++) {
            if (cx < 0 || cz < 0 || cx >= MW || cz >= MH) continue;
            if (map[cz][cx] == '#') continue;

            float x0 = cx * CS, x1 = x0 + CS, z0 = cz * CS, z1 = z0 + CS;
            int ddx = cx - pcx, ddz = cz - pcz;
            int n = (ddx * ddx + ddz * ddz <= 2) ? 2 : 1;   // mas detalle cerca

            int alt = (cx + cz) & 1;
            float fl[3] = { alt ? 150.f : 120.f, alt ? 140.f : 112.f, alt ? 125.f : 100.f };
            float ce[3] = { 110.f, 110.f, 120.f };
            float wa[3] = { 190.f, 170.f, 150.f };
            float q[4][3];

            // Suelo
            float f[4][3] = { {x0,0,z0}, {x1,0,z0}, {x1,0,z1}, {x0,0,z1} };
            quad_sub(f, fl, n, 0.0f);
            // Techo
            float c[4][3] = { {x0,WH,z0}, {x1,WH,z0}, {x1,WH,z1}, {x0,WH,z1} };
            quad_sub(c, ce, n, 0.0f);

            // Paredes hacia celdas solidas
            if (cz - 1 < 0 || map[cz - 1][cx] == '#') {
                float w[4][3] = { {x0,0,z0}, {x1,0,z0}, {x1,WH,z0}, {x0,WH,z0} };
                memcpy(q, w, sizeof(q)); quad_sub(q, wa, n, 0.0f);
            }
            if (cz + 1 >= MH || map[cz + 1][cx] == '#') {
                float w[4][3] = { {x0,0,z1}, {x1,0,z1}, {x1,WH,z1}, {x0,WH,z1} };
                memcpy(q, w, sizeof(q)); quad_sub(q, wa, n, 0.0f);
            }
            if (cx - 1 < 0 || map[cz][cx - 1] == '#') {
                float w[4][3] = { {x0,0,z0}, {x0,0,z1}, {x0,WH,z1}, {x0,WH,z0} };
                memcpy(q, w, sizeof(q)); quad_sub(q, wa, n, 0.0f);
            }
            if (cx + 1 >= MW || map[cz][cx + 1] == '#') {
                float w[4][3] = { {x1,0,z0}, {x1,0,z1}, {x1,WH,z1}, {x1,WH,z0} };
                memcpy(q, w, sizeof(q)); quad_sub(q, wa, n, 0.0f);
            }
        }
    }
}

// ---------------- IA del monstruo (BFS sobre la cuadricula) ----------------
static void compute_flow(int tx, int tz)
{
    int qx[MW * MH], qz[MW * MH], h = 0, t = 0;
    for (int z = 0; z < MH; z++) for (int x = 0; x < MW; x++) dist_map[z][x] = 9999;
    dist_map[tz][tx] = 0; qx[t] = tx; qz[t++] = tz;
    static const int dx[4] = {1, -1, 0, 0}, dz[4] = {0, 0, 1, -1};
    while (h < t) {
        int x = qx[h], z = qz[h++];
        for (int k = 0; k < 4; k++) {
            int nx = x + dx[k], nz = z + dz[k];
            if (nx < 0 || nz < 0 || nx >= MW || nz >= MH) continue;
            if (map[nz][nx] == '#' || dist_map[nz][nx] != 9999) continue;
            dist_map[nz][nx] = dist_map[z][x] + 1;
            qx[t] = nx; qz[t++] = nz;
        }
    }
}

static void update_monster(float dt, int notes)
{
    int mcx = (int)(sx_ / CS), mcz = (int)(sz_ / CS);
    int pcx = (int)(px / CS), pcz = (int)(pz / CS);
    float tx = px, tz = pz;

    if (mcx != pcx || mcz != pcz) {
        static const int dx[4] = {1, -1, 0, 0}, dz[4] = {0, 0, 1, -1};
        int best = dist_map[mcz][mcx], bx = mcx, bz = mcz;
        for (int k = 0; k < 4; k++) {
            int nx = mcx + dx[k], nz = mcz + dz[k];
            if (nx < 0 || nz < 0 || nx >= MW || nz >= MH) continue;
            if (dist_map[nz][nx] < best) { best = dist_map[nz][nx]; bx = nx; bz = nz; }
        }
        tx = (bx + 0.5f) * CS; tz = (bz + 0.5f) * CS;
    }
    float dx = tx - sx_, dz = tz - sz_;
    float len = sqrtf(dx * dx + dz * dz);
    if (len > 0.01f) {
        float speed = 1.3f + 0.5f * notes;       // mas notas = mas rapido
        sx_ += dx / len * speed * dt;
        sz_ += dz / len * speed * dt;
    }
}

// ---------------- Reinicio ----------------
static void reset_game(void)
{
    px = 1.5f * CS; pz = 1.5f * CS; yaw = 1.5708f;
    sx_ = 14.5f * CS; sz_ = 7.5f * CS;
    for (int i = 0; i < 3; i++) notes_got[i] = 0;
    state = 0; state_timer = 0; flash_on = 1;
}

// ---------------- Main ----------------
int main(void)
{
    SifInitRpc(0);
    SifLoadModule("rom0:SIO2MAN", 0, NULL);
    SifLoadModule("rom0:PADMAN", 0, NULL);

    padInit(0);
    padPortOpen(0, 0, padBuf);
    while (padGetState(0, 0) != PAD_STATE_STABLE &&
           padGetState(0, 0) != PAD_STATE_FINDCTP1) ;
    padSetMainMode(0, 0, PAD_MMODE_DUALSHOCK, PAD_MMODE_LOCK);

    gsGlobal = gsKit_init_global();
    gsGlobal->PSM = GS_PSM_CT24;
    gsGlobal->PSMZ = GS_PSMZ_24;
    gsGlobal->ZBuffering = GS_SETTING_ON;
    gsGlobal->DoubleBuffering = GS_SETTING_ON;
    gsGlobal->PrimAlphaEnable = GS_SETTING_OFF;

    dmaKit_init(D_CTRL_RELE_OFF, D_CTRL_MFD_OFF, D_CTRL_STS_UNSPEC,
                D_CTRL_STD_OFF, D_CTRL_RCYC_8, 1 << DMA_CHANNEL_GIF);
    dmaKit_chan_init(DMA_CHANNEL_GIF);

    gsKit_init_screen(gsGlobal);
    gsKit_mode_switch(gsGlobal, GS_ONESHOT);
    gsKit_set_test(gsGlobal, GS_ZTEST_ON);

    W = (float)gsGlobal->Width;
    H = (float)gsGlobal->Height;
    FOCAL = W * 0.7f;

    reset_game();

    struct padButtonStatus pad;
    u32 paddata = 0, old_pad = 0;
    const float dt = 1.0f / 60.0f;

    while (1) {
        frame_no++;
        float t = frame_no * dt;

        // ---- Entrada ----
        float fwd = 0, str = 0, turn = 0;
        if (padRead(0, 0, &pad)) {
            paddata = 0xffff ^ pad.btns;
            u32 newp = paddata & ~old_pad;
            old_pad = paddata;

            float lh = (pad.ljoy_h - 128) / 128.0f, lv = (pad.ljoy_v - 128) / 128.0f;
            float rh = (pad.rjoy_h - 128) / 128.0f;
            if (fabsf(lh) > 0.2f) str = lh;
            if (fabsf(lv) > 0.2f) fwd = -lv;
            if (fabsf(rh) > 0.2f) turn = rh;
            if (paddata & PAD_RIGHT) turn = 0.7f;
            if (paddata & PAD_LEFT)  turn = -0.7f;
            if (newp & PAD_L1) flash_on = !flash_on;
            if (newp & PAD_START) reset_game();
        }

        // ---- Camara ----
        fx = sinf(yaw); fz = cosf(yaw);
        rx = cosf(yaw); rz = -sinf(yaw);

        // ---- Logica ----
        int notes = notes_got[0] + notes_got[1] + notes_got[2];
        if (state == 0) {
            yaw += turn * 2.4f * dt;
            float speed = (paddata & PAD_R1) ? 5.0f : 2.8f;
            float mx = (fx * fwd + rx * str) * speed * dt;
            float mz = (fz * fwd + rz * str) * speed * dt;
            if (!blocked(px + mx, pz, 0.35f)) px += mx;
            if (!blocked(px, pz + mz, 0.35f)) pz += mz;

            // Luz ambiente parpadeante
            float flick = 0.85f + 0.15f * sinf(t * 13.0f) * sinf(t * 7.3f);
            if ((frame_no / 6) % 41 == 0) flick = 0.35f;
            amb_r = 9.0f * flick;

            // Monstruo
            if (frame_no % 15 == 0) compute_flow((int)(px / CS), (int)(pz / CS));
            update_monster(dt, notes);
            float ddx = sx_ - px, ddz = sz_ - pz;
            if (ddx * ddx + ddz * ddz < 1.0f) { state = 1; state_timer = 150; }

            // Notas
            for (int i = 0; i < 3; i++) {
                if (notes_got[i]) continue;
                float nx = (note_cell[i][0] + 0.5f) * CS, nz = (note_cell[i][1] + 0.5f) * CS;
                float ax = nx - px, az = nz - pz;
                if (ax * ax + az * az < 1.7f) notes_got[i] = 1;
            }
            // Salida
            if (notes == 3) {
                float ex = 14.5f * CS - px, ez = 13.5f * CS - pz;
                if (ex * ex + ez * ez < 2.5f) { state = 2; state_timer = 240; }
            }
        } else {
            if (--state_timer <= 0) reset_game();
        }

        // ---- Render ----
        if (state == 1) gsKit_clear(gsGlobal, GS_SETREG_RGBAQ(0x60, 0x00, 0x00, 0x80, 0x00));
        else if (state == 2) gsKit_clear(gsGlobal, GS_SETREG_RGBAQ(0x00, 0x50, 0x10, 0x80, 0x00));
        else {
            gsKit_clear(gsGlobal, BLACK);
            draw_level();

            // Notas (cajitas amarillas que flotan)
            float ycol[3] = { 255.f, 230.f, 80.f };
            for (int i = 0; i < 3; i++) {
                if (notes_got[i]) continue;
                float nx = (note_cell[i][0] + 0.5f) * CS, nz = (note_cell[i][1] + 0.5f) * CS;
                float by = 0.9f + 0.1f * sinf(t * 3.0f + i);
                box(nx, nz, 0.18f, by, by + 0.3f, ycol, 0.6f);
            }
            // Salida (luz verde cuando tienes las 3 notas)
            if (notes == 3) {
                float gcol[3] = { 80.f, 255.f, 120.f };
                box(14.5f * CS, 13.5f * CS, 0.3f, 0.0f, WH, gcol, 0.7f);
            }
            // Monstruo
            float mcol[3] = { 120.f, 20.f, 20.f };
            box(sx_, sz_, 0.4f, 0.0f, 2.3f, mcol, 0.0f);

            // HUD: notas recogidas
            for (int i = 0; i < 3; i++) {
                u64 c = notes_got[i] ? GS_SETREG_RGBAQ(255, 230, 80, 0x80, 0)
                                     : GS_SETREG_RGBAQ(50, 50, 50, 0x80, 0);
                gsKit_prim_sprite(gsGlobal, 20.0f + i * 24.0f, 20.0f,
                                  36.0f + i * 24.0f, 36.0f, 16777000, c);
            }
        }

        gsKit_queue_exec(gsGlobal);
        gsKit_sync_flip(gsGlobal);
    }
    return 0;
}
