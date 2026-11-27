#define _POSIX_C_SOURCE 200809L

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include "lauxlib.h"
#include "lua.h"
#include "lualib.h"

#ifndef ENGINE_NAME
#define ENGINE_NAME "C"
#endif

static const char *ACTIVE_LUA =
    "local function makeTree(d)\n"
    "  if d == 0 then return { x = 1, y = 2 } end\n"
    "  return { l = makeTree(d - 1), r = makeTree(d - 1), tag = d }\n"
    "end\n"
    "G_TREE = makeTree(10)\n";

static int current_rss_kib(long long *out) {
  char cmd[64];
  snprintf(cmd, sizeof(cmd), "ps -o rss= -p %ld", (long)getpid());
  FILE *fp = popen(cmd, "r");
  if (!fp) return 0;
  long long v = 0;
  int ok = (fscanf(fp, "%lld", &v) == 1);
  pclose(fp);
  if (!ok) return 0;
  *out = v;
  return 1;
}

static double now_us(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (double)ts.tv_sec * 1e6 + (double)ts.tv_nsec / 1e3;
}

static lua_State *new_full_state(void) {
  lua_State *L = luaL_newstate();
  if (!L) {
    fprintf(stderr, "%s_mem: luaL_newstate failed\n", ENGINE_NAME);
    exit(1);
  }
  luaL_openlibs(L);
  return L;
}

int main(int argc, char **argv) {
  if (argc != 3) {
    fprintf(stderr, "usage: %s <idle|active> <count>\n", argv[0]);
    return 2;
  }
  const char *mode = argv[1];
  int n = atoi(argv[2]);
  if (n <= 0) {
    fprintf(stderr, "%s_mem: invalid count %s\n", ENGINE_NAME, argv[2]);
    return 2;
  }

  /* Warm up libc allocator and process pages. */
  lua_State *warm = new_full_state();
  lua_close(warm);

  long long rss0 = 0, rss1 = 0;
  int has_rss = current_rss_kib(&rss0);

  lua_State **vms = (lua_State **)calloc((size_t)n, sizeof(lua_State *));
  if (!vms) {
    fprintf(stderr, "%s_mem: calloc failed\n", ENGINE_NAME);
    return 1;
  }

  if (strcmp(mode, "idle") == 0) {
    double t0 = now_us();
    for (int i = 0; i < n; i++) {
      vms[i] = new_full_state();
    }
    double dt = now_us() - t0;
    has_rss = has_rss && current_rss_kib(&rss1);
    double idle_rss = has_rss ? (double)(rss1 - rss0) / (double)n : 0.0;
    printf("{\"engine\":\"" ENGINE_NAME "\",\"vms\":%d,\"new_state_us\":%.6f,\"idle_rss_kib\":%.6f,\"has_rss\":%s}\n",
           n, dt / (double)n, idle_rss, has_rss ? "true" : "false");
  } else if (strcmp(mode, "active") == 0) {
    for (int i = 0; i < n; i++) {
      vms[i] = new_full_state();
      if (luaL_dostring(vms[i], ACTIVE_LUA) != 0) {
        fprintf(stderr, "%s_mem: active init failed at VM %d: %s\n", ENGINE_NAME, i, lua_tostring(vms[i], -1));
        return 1;
      }
    }
    has_rss = has_rss && current_rss_kib(&rss1);
    double act_rss = has_rss ? (double)(rss1 - rss0) / (double)n : 0.0;
    printf("{\"engine\":\"" ENGINE_NAME "\",\"vms\":%d,\"active_rss_kib\":%.6f,\"has_rss\":%s}\n",
           n, act_rss, has_rss ? "true" : "false");
  } else {
    fprintf(stderr, "%s_mem: unknown mode %s\n", ENGINE_NAME, mode);
    free(vms);
    return 2;
  }

  for (int i = 0; i < n; i++) {
    lua_close(vms[i]);
  }
  free(vms);
  return 0;
}
