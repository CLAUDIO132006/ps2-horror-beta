EE_BIN = horror_beta.elf
EE_OBJS = main.o audsrv_irx.o
EE_CFLAGS += -DUSE_AUDIO

# freesd (libsd libre): si el SDK lo trae, se usa en vez del LIBSD de la BIOS
ifneq ($(wildcard $(PS2SDK)/iop/irx/freesd.irx),)
EE_CFLAGS += -DUSE_FREESD
EE_OBJS += freesd_irx.o
endif

# ONLINE (menu principal > ONLINE). Para compilar SIN online: make NET=0
NET ?= 1
ifeq ($(NET),1)
EE_CFLAGS += -DUSE_NET
EE_OBJS += DEV9_irx.o NETMAN_irx.o SMAP_irx.o
# pila TCP/IP: la version con NETMAN (libps2ip_nm) si el SDK la trae; si no, la clasica
ifneq ($(wildcard $(PS2SDK)/ee/lib/libps2ip_nm.a),)
EE_LIBS += -lps2ip_nm
else
EE_LIBS += -lps2ip
endif
EE_LIBS += -lnetman
endif

EE_INCS += -I$(PS2DEV)/gsKit/include
EE_LDFLAGS += -L$(PS2DEV)/gsKit/lib
EE_LIBS += -lgskit -ldmakit -lpad -laudsrv -lpatches -lm

all: $(EE_BIN)

audsrv_irx.c:
	$(PS2SDK)/bin/bin2c $(PS2SDK)/iop/irx/audsrv.irx audsrv_irx.c audsrv_irx

freesd_irx.c:
	$(PS2SDK)/bin/bin2c $(PS2SDK)/iop/irx/freesd.irx freesd_irx.c freesd_irx

# modulos del IOP para la red (se incrustan en el ELF, igual que audsrv)
DEV9_irx.c:
	$(PS2SDK)/bin/bin2c $(PS2SDK)/iop/irx/ps2dev9.irx DEV9_irx.c DEV9_irx

NETMAN_irx.c:
	$(PS2SDK)/bin/bin2c $(PS2SDK)/iop/irx/netman.irx NETMAN_irx.c NETMAN_irx

SMAP_irx.c:
	$(PS2SDK)/bin/bin2c $(PS2SDK)/iop/irx/smap.irx SMAP_irx.c SMAP_irx

clean:
	rm -f $(EE_BIN) $(EE_OBJS) audsrv_irx.c freesd_irx.c DEV9_irx.c NETMAN_irx.c SMAP_irx.c DEV9_irx.o NETMAN_irx.o SMAP_irx.o

include $(PS2SDK)/samples/Makefile.pref
include $(PS2SDK)/samples/Makefile.eeglobal
