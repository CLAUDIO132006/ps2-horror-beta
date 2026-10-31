#include <kernel.h>
#include <sifrpc.h>
#include <libpad.h>
#include <gsKit.h>
#include <dmaKit.h>
#include <tamtypes.h>

#define SCREEN_W 640
#define SCREEN_H 448
#define PLAYER_W 24
#define PLAYER_H 34
#define SPEED 2.2f

static unsigned char pad_buffer[256] __attribute__((aligned(64)));

static int hits_wall(float x, float y)
{
    if (x < 34 || y < 46 || x + PLAYER_W > 606 || y + PLAYER_H > 410)
        return 1;
    if (x + PLAYER_W > 286 && x < 354 && y + PLAYER_H > 118 && y < 330) {
        if (!(y > 204 && y + PLAYER_H < 250)) return 1;
    }
    if (x + PLAYER_W > 120 && x < 174 && y + PLAYER_H > 118 && y < 174)
        return 1;
    if (x + PLAYER_W > 454 && x < 510 && y + PLAYER_H > 292 && y < 348)
        return 1;
    return 0;
}

static void draw_room(GSGLOBAL *gs)
{
    u64 floor = GS_SETREG_RGBAQ(23, 34, 49, 0x80, 0);
    u64 wall  = GS_SETREG_RGBAQ(78, 102, 126, 0x80, 0);
    u64 trim  = GS_SETREG_RGBAQ(125, 151, 171, 0x80, 0);
    u64 dark  = GS_SETREG_RGBAQ(12, 19, 29, 0x80, 0);

    gsKit_clear(gs, dark);
    gsKit_prim_sprite(gs, 24, 36, 616, 420, 1, floor);
    gsKit_prim_sprite(gs, 24, 36, 616, 48, 1, wall);
    gsKit_prim_sprite(gs, 24, 408, 616, 420, 1, wall);
    gsKit_prim_sprite(gs, 24, 48, 36, 408, 1, wall);
    gsKit_prim_sprite(gs, 604, 48, 616, 408, 1, wall);

    /* Divider with a doorway */
    gsKit_prim_sprite(gs, 286, 112, 354, 204, 1, wall);
    gsKit_prim_sprite(gs, 286, 250, 354, 336, 1, wall);
    gsKit_prim_sprite(gs, 286, 204, 292, 250, 1, trim);
    gsKit_prim_sprite(gs, 348, 204, 354, 250, 1, trim);

    /* Test consoles */
    gsKit_prim_sprite(gs, 120, 118, 174, 174, 1, wall);
    gsKit_prim_sprite(gs, 454, 292, 510, 348, 1, wall);
    gsKit_prim_sprite(gs, 128, 126, 166, 134, 1, trim);
    gsKit_prim_sprite(gs, 462, 300, 502, 308, 1, trim);
}

static void draw_explorer(GSGLOBAL *gs, float x, float y)
{
    u64 suit   = GS_SETREG_RGBAQ(30, 190, 190, 0x80, 0);
    u64 visor  = GS_SETREG_RGBAQ(170, 225, 238, 0x80, 0);
    u64 shine  = GS_SETREG_RGBAQ(235, 250, 255, 0x80, 0);
    u64 shadow = GS_SETREG_RGBAQ(7, 15, 24, 0x80, 0);

    /* Temporary original placeholder sprite, not official game artwork. */
    gsKit_prim_sprite(gs, x + 3, y + 8, x + 21, y + 30, 2, suit);
    gsKit_prim_sprite(gs, x + 1, y + 13, x + 5, y + 26, 2, suit);
    gsKit_prim_sprite(gs, x + 19, y + 13, x + 23, y + 26, 2, suit);
    gsKit_prim_sprite(gs, x + 5, y + 27, x + 11, y + 34, 2, suit);
    gsKit_prim_sprite(gs, x + 14, y + 27, x + 20, y + 34, 2, suit);
    gsKit_prim_sprite(gs, x + 5, y + 2, x + 20, y + 16, 2, suit);
    gsKit_prim_sprite(gs, x + 8, y + 5, x + 21, y + 13, 3, visor);
    gsKit_prim_sprite(gs, x + 10, y + 6, x + 17, y + 8, 3, shine);
    gsKit_prim_sprite(gs, x + 2, y + 32, x + 22, y + 35, 1, shadow);
}

int main(int argc, char *argv[])
{
    GSGLOBAL *gs;
    struct padButtonStatus pad;
    float px = 78.0f, py = 78.0f, nx, ny;
    u16 buttons;

    (void)argc; (void)argv;

    SifInitRpc(0);
    padInit(0);
    padPortOpen(0, 0, pad_buffer);

    gs = gsKit_init_global();
    gs->Mode = GS_MODE_NTSC;
    gs->Interlace = GS_INTERLACED;
    gs->Field = GS_FIELD;
    gs->Width = SCREEN_W;
    gs->Height = SCREEN_H;
    gs->PSM = GS_PSM_CT24;
    gs->PSMZ = GS_PSMZ_16S;

    dmaKit_init(D_CTRL_RELE_OFF, D_CTRL_MFD_OFF, D_CTRL_STS_UNSPEC,
                D_CTRL_STD_OFF, D_CTRL_RCYC_8, 1 << DMA_CHANNEL_GIF);
    dmaKit_chan_init(DMA_CHANNEL_GIF);
    gsKit_init_screen(gs);
    gsKit_mode_switch(gs, GS_ONESHOT);

    for (;;) {
        if (padGetState(0, 0) == PAD_STATE_STABLE && padRead(0, 0, &pad)) {
            buttons = pad.btns; /* PS2 button bits are active-low. */
            nx = px; ny = py;
            if (!(buttons & PAD_LEFT))  nx -= SPEED;
            if (!(buttons & PAD_RIGHT)) nx += SPEED;
            if (!(buttons & PAD_UP))    ny -= SPEED;
            if (!(buttons & PAD_DOWN))  ny += SPEED;
            if (!hits_wall(nx, py)) px = nx;
            if (!hits_wall(px, ny)) py = ny;
        }

        draw_room(gs);
        draw_explorer(gs, px, py);
        gsKit_queue_exec(gs);
        gsKit_sync_flip(gs);
    }

    padPortClose(0, 0);
    padEnd();
    return 0;
}
