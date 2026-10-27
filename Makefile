# Makefile para HORROR BETA 0.4 - BOSQUE
EE_BIN = horror_beta.elf
EE_OBJS = main.o

# Desactivar temporalmente la red para evitar errores de enlace con ps2ip/netman si no están instalados por defecto
NET ?= 0
ifeq ($(NET),1)
EE_CFLAGS += -DNET_ENABLED
endif

EE_CFLAGS += -O2 -Wall -Wno-unused-variable -Wno-unused-const-variable

EE_LIBS = -lgsKit -ldmadkit -lgskit_toolkit -laudsrv -lpad -lm -lc

ifeq ($(NET),1)
EE_LIBS += -lps2ip -lnetman
endif

all: $(EE_BIN)

clean:
	rm -f $(EE_OBJS) $(EE_BIN)

include $(PS2SDK)/samples/Makefile.pref
include $(PS2SDK)/samples/Makefile.eeglobal
