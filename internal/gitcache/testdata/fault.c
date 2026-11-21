#define _GNU_SOURCE
#include <dlfcn.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
static void pause_at(const char *a, const char *b, const char *phase) {
 const char *needle=getenv("REVIEW_NEEDLE"), *marker=getenv("REVIEW_MARKER"), *when=getenv("REVIEW_PHASE");
 if (!needle || !marker || !when || strcmp(when,phase) || !strstr(b,needle)) return;
 int fd=open(marker,O_WRONLY|O_CREAT|O_EXCL,0600);
 if(fd<0) return;
 dprintf(fd,"%d\n%s\n%s\n",getpid(),a,b); fsync(fd); close(fd); raise(SIGSTOP);
}
int rename(const char *a,const char *b) {
 int (*real)(const char*,const char*)=dlsym(RTLD_NEXT,"rename");
 pause_at(a,b,"before"); int r=real(a,b); if(!r) pause_at(a,b,"after"); return r;
}
