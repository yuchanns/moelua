# Reference PUC-Rio Lua 5.5.1 sources and official test suite default to the
# pinned third_party/lua git submodule (`git submodule update --init`).
# Override LUA55_SRC or LUA55_TESTES to point at another local checkout.
LUA55_SRC ?= $(if $(wildcard third_party/lua/onelua.c),third_party/lua,)
LUA55_TESTES ?= $(if $(wildcard third_party/lua/testes/all.lua),third_party/lua/testes,)
BUILD_DIR := build

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  LUA_PLAT_CFLAGS := -DLUA_USE_MACOSX
  LUA_PLAT_LIBS   := -lm -lreadline
  LUA_O3_MCPU     := -mcpu=apple-m1
else
  LUA_PLAT_CFLAGS := -DLUA_USE_LINUX
  LUA_PLAT_LIBS   := -lm -ldl
  LUA_O3_MCPU     := -march=native
endif

.PHONY: tools generate test test-compare pgo bench bench-pgo bench-compare cli clean

tools: $(BUILD_DIR)/lua55_o2 $(BUILD_DIR)/lua55_o3 $(BUILD_DIR)/lua55_o3_m1 $(BUILD_DIR)/luac55

cli: $(BUILD_DIR)/moelua

$(BUILD_DIR)/moelua: | $(BUILD_DIR)
	go build -o $@ ./cmd/moelua

generate:
	go generate ./vm/...

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

define check_lua_src
	@test -n "$(LUA55_SRC)" || (echo "Lua 5.5.1 source not found; run 'git submodule update --init' or set LUA55_SRC=/path/to/lua-5.5.1/src" && exit 1)
endef

define check_lua_testes
	@test -n "$(LUA55_TESTES)" || (echo "Lua 5.5.1 testes not found; run 'git submodule update --init' or set LUA55_TESTES=/path/to/lua-5.5.1/testes" && exit 1)
endef

$(BUILD_DIR)/lua55_o2: | $(BUILD_DIR)
	$(check_lua_src)
	cc -O2 $(LUA_PLAT_CFLAGS) -DMAKE_LUA -I$(LUA55_SRC) $(LUA55_SRC)/onelua.c $(LUA_PLAT_LIBS) -o $@

$(BUILD_DIR)/lua55_o3: | $(BUILD_DIR)
	$(check_lua_src)
	cc -O3 -march=native $(LUA_PLAT_CFLAGS) -DMAKE_LUA -I$(LUA55_SRC) $(LUA55_SRC)/onelua.c $(LUA_PLAT_LIBS) -o $@

$(BUILD_DIR)/lua55_o3_m1: | $(BUILD_DIR)
	$(check_lua_src)
	cc -O3 $(LUA_O3_MCPU) $(LUA_PLAT_CFLAGS) -DMAKE_LUA -I$(LUA55_SRC) $(LUA55_SRC)/onelua.c $(LUA_PLAT_LIBS) -o $@

$(BUILD_DIR)/luac55: $(BUILD_DIR)/lua55_o2 | $(BUILD_DIR)
	printf '#!/bin/sh\nset -e\nSTRIP=false\nOUT=luac.out\nwhile [ "$$#" -gt 0 ]; do\n  case "$$1" in\n    -s) STRIP=true; shift ;;\n    -o) OUT="$$2"; shift 2 ;;\n    *) IN="$$1"; shift ;;\n  esac\ndone\nLUAC_IN="$$IN" LUAC_OUT="$$OUT" LUAC_STRIP="$$STRIP" exec "$$(dirname "$$0")/lua55_o2" -e "local f=assert(loadfile(os.getenv('\''LUAC_IN'\''))); local o=assert(io.open(os.getenv('\''LUAC_OUT'\''), '\''wb'\'')); o:write(string.dump(f, os.getenv('\''LUAC_STRIP'\'')=='\''true'\'')); o:close()"\n' > $@
	chmod +x $@

pgo: | $(BUILD_DIR)
	$(check_lua_testes)
	LUA55_TESTES=$(LUA55_TESTES) go test -count=3 -cpuprofile=$(BUILD_DIR)/disjoint.pgo -run '^TestOfficialLua55Testes$$' ./vm

test:
	go vet ./...
	go test -count=1 ./...
	go test -tags moeluaregcheck -count=1 ./...
	go test -tags moeluafloatnil -count=1 ./...
	go test -tags moeluafaithful -count=1 ./...
	go test -race -count=1 ./...

bench: tools
	go build -o $(BUILD_DIR)/benchcmp ./cmd/benchcmp
	./$(BUILD_DIR)/benchcmp -rounds 10 -gomaxprocs 1 -lua-o3 $(BUILD_DIR)/lua55_o3_m1

bench-pgo: tools pgo
	go build -pgo=$(BUILD_DIR)/disjoint.pgo -o $(BUILD_DIR)/benchcmp_pgo ./cmd/benchcmp
	./$(BUILD_DIR)/benchcmp_pgo -rounds 10 -gomaxprocs 1 -lua-o3 $(BUILD_DIR)/lua55_o3_m1

test-compare: $(BUILD_DIR)/lua55_o2
	cd bench/compare && go test -vet=off -timeout 5m -run TestOutputs -engines '^(C|MoeLua)$$' .

bench-compare: $(BUILD_DIR)/lua55_o2 $(BUILD_DIR)/lua55_o3
	cd bench/compare && GOLUA_DIR="$$(go list -m -f '{{.Dir}}' github.com/arnodel/golua)" && \
		( GOMAXPROCS=1 go test -vet=off -timeout 60m -run @ -bench . -benchtime 300ms -count 3 \
			-engines '^(C|C-O3|MoeLua|Apogee-JIT|Apogee-Interp|CCGO|GopherLua|GoLua|DCLua)$$' . && \
		  GOMAXPROCS=1 go test -vet=off -timeout 30m -pgo="$$GOLUA_DIR/default.pgo" -run @ -bench . -benchtime 300ms -count 3 \
			-engines '^ArnodelGolua$$' . ) | go run benchreport.go

clean:
	rm -rf $(BUILD_DIR)
