EE_BIN = horror_beta.elf
EE_OBJS = main.o audsrv_irx.o
EE_CFLAGS += -DUSE_AUDIO

EE_INCS += -I$(PS2DEV)/gsKit/include
EE_LDFLAGS += -L$(PS2DEV)/gsKit/lib
EE_LIBS += -lgskit -ldmakit -lpad -laudsrv -lm

all: $(EE_BIN)

audsrv_irx.c:
	$(PS2SDK)/bin/bin2c $(PS2SDK)/iop/irx/audsrv.irx audsrv_irx.c audsrv_irx

clean:
	rm -f $(EE_BIN) $(EE_OBJS) audsrv_irx.c

include $(PS2SDK)/samples/Makefile.pref
include $(PS2SDK)/samples/Makefile.eeglobal
