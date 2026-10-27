/* 
 * HORROR BETA 0.4 - BOSQUE 
 * PS2 Homebrew Game Source Code - PS2SDK / gsKit Fixed Version
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <math.h>

#include <tamtypes.h>
#include <kernel.h>
#include <sifrpc.h>
#include <loadfile.h>
#include <libpad.h>
#include <gsKit.h>
#include <dmaKit.h>

#ifdef NET_ENABLED
#include <netinet/in.h>
#include <arpa/inet.h>
#include <sys/socket.h>
#include <ps2ip.h>
#endif

typedef struct {
    float x, y, z;
} Vec3;

typedef struct {
    Vec3 pos;
    Vec3 rot;
    float stamina;
    int flashlight;
    int health;
} Player;

typedef struct {
    Vec3 pos;
    int type;
    int active;
    int locked;
    int key_id;
} Prop;

static const char map0[] = "MAPA_BOSQUE_01";
static const char map1[] = "MAPA_BOSQUE_02";

static const float mon_v[] = {
    -0.5f, 0.0f, -0.5f,  0.5f, 0.0f, -0.5f,  0.5f, 2.0f, -0.5f, -0.5f, 2.0f, -0.5f,
    -0.5f, 0.0f,  0.5f,  0.5f, 0.0f,  0.5f,  0.5f, 2.0f,  0.5f, -0.5f, 2.0f,  0.5f
};

static const int mon_t[] = {
    0, 1, 2,  0, 2, 3,  4, 5, 6,  4, 6, 7,
    0, 4, 7,  0, 7, 3,  1, 5, 6,  1, 6, 2
};

static const float vm_v[] = {
    -0.2f, 0.0f, -0.2f,  0.2f, 0.0f, -0.2f,  0.2f, 1.7f, -0.2f, -0.2f, 1.7f, -0.2f
};

static const int vm_t[] = {
    0, 1, 2,  0, 2, 3
};

static Prop props[] = {
    {{ 10.0f, 0.0f,  5.0f}, 1, 1, 1, 101},
    {{-15.0f, 0.0f, -12.0f}, 2, 1, 0, 0},
    {{ 25.0f, 0.0f, -30.0f}, 1, 1, 1, 102},
    {{  0.0f, 0.0f,  40.0f}, 3, 1, 0, 0}
};

static GSGLOBAL *gsGlobal;
static Player player1;
static Player player2;

void init_ps2_hardware(void);
void init_game_state(void);
void update_player_input(int pad_num);
void render_scene(void);

int main(int argc, char *argv[]) {
    init_ps2_hardware();
    init_game_state();

    printf("HORROR BETA 0.4 - BOSQUE iniciado correctamente.\n");

    while (1) {
        update_player_input(0);
        
        gsKit_clear(gsGlobal, GS_SETREG_RGBAQ(0x10, 0x10, 0x15, 0x00, 0x00));
        render_scene();
        gsKit_queue_exec(gsGlobal);
        gsKit_sync_flip(gsGlobal);
    }

    return 0;
}

void init_ps2_hardware(void) {
    SifInitRpc(0);
    
    dmaKit_init(D_CTRL_RELE_OFF, D_CTRL_MFD_OFF, D_CTRL_STS_DISABLE,
                D_CTRL_STD_OFF, D_CTRL_RCK_DISABLE, 10);
    dmaKit_chan_init(DMA_CHANNEL_GIF);

    gsGlobal = gsKit_init_global();
    gsGlobal->Mode = GS_MODE_NTSC;
    gsGlobal->Interlace = GS_INTERLACED;
    gsGlobal->Field = GS_FIELD;
    gsGlobal->Width = 640;
    gsGlobal->Height = 448;
    gsGlobal->PSM = GS_PSM_CT32;
    gsGlobal->ZSM = GS_ZSM_ZB32;
    
    gsKit_init_screen(gsGlobal);
    gsKit_mode_switch(gsGlobal, GS_ONESHOT);

    padInit(0);
}

void init_game_state(void) {
    player1.pos.x = 0.0f;
    player1.pos.y = 0.0f;
    player1.pos.z = 0.0f;
    player1.stamina = 100.0f;
    player1.flashlight = 1;
    player1.health = 100;

    player2.pos.x = 2.0f;
    player2.pos.y = 0.0f;
    player2.pos.z = 0.0f;
    player2.stamina = 100.0f;
    player2.flashlight = 0;
    player2.health = 100;
}

void update_player_input(int pad_num) {
    if (player1.stamina < 100.0f) {
        player1.stamina += 0.1f;
    }
}

void render_scene(void) {
}
