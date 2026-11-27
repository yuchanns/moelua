// Shared memory layout offsets, tags, and exit codes for Plan 9 direct-threaded
// bytecode interpreters (arm64 and amd64). Every constant here is verified at compile
// time by vm/asm_layout.go and checked by TestAsmOffsetsHeaderSync.

// asmExecCtx offsets (sizeof == 160)
#define CTX_REGS        0
#define CTX_INSNS       8
#define CTX_CONSTS      16
#define CTX_PAGEPTRS    24
#define CTX_LARGESPANS  32
#define CTX_STACK       40
#define CTX_FRAMES      48
#define CTX_PROTOS      56
#define CTX_IC          64
#define CTX_ICSLOTS     72
#define CTX_CLUPVALS    80
#define CTX_CURBP       88
#define CTX_TH          96
#define CTX_PROTOEPOCH  104
#define CTX_PC          112
#define CTX_POLL        120
#define CTX_CI          128
#define CTX_BASE        136
#define CTX_ENTRYDEPTH  144
#define CTX_THSELF      152
#define CTX_SQRTIDX     156
#define CTX_ABSIDX      160

// Assembly return exit codes
#define ASM_EXIT_DONE    0
#define ASM_EXIT_POLL    1
#define ASM_EXIT_RELOAD  2
#define ASM_EXIT_COLD    3

// Value layout (sizeof == 16, !moeluafloatnil polarity)
#define VAL_SIZE         16
#define VAL_BITS         0
#define VAL_TAG          8

#define TAG_INT          0
#define TAG_FLOAT        1
#define TAG_NIL          2
#define TAG_FALSE        3
#define TAG_TRUE         4
#define TAG_LIGHTUD      5
#define TAG_TOKENLUD     6
#define TAG_STATICNAT    7
#define TAG_FROZENSTR    8
#define TAG_STRING       9
#define TAG_CLOSURE      10
#define TAG_NATIVE       11
#define TAG_TABLE        12
#define TAG_USERDATA     13
#define TAG_THREAD       14
#define TAG_FIRST_GC     9
#define TAG_LAST_GC      14

// Arena ObjRef bit layout
#define ARENA_WORDBITS   13
#define ARENA_WORDMASK   0x1fff
#define ARENA_ALIGNSHIFT 3
#define ARENA_LARGEBIT   31
#define MARK_BLACK_BIT   2

// Table layout (sizeof == 80)
#define TBL_SIZE         80
#define TBL_MARK         5
#define TBL_FLAGS        7
#define TBL_SELF         8
#define TBL_SHAPEREF     12
#define TBL_METAREF      16
#define TBL_HASHREF      20
#define TBL_ARRREF       24
#define TBL_SLOTSREF     28
#define TBL_ARRLEN       32
#define TBL_ARRCAP       36
#define TBL_SLOTSLEN     44
#define TBL_SLOTSCAP     45
#define TBL_ARRKIND      46
#define TBL_TMFLAGS      47
#define TBL_INLINESLOTS  48

#define TBL_FLAG_ISPROTO_BIT 0

#define ARR_EMPTY        0
#define ARR_INT          1
#define ARR_FLOAT        2
#define ARR_BOOL         3
#define ARR_GENERIC      4

// Closure layout (sizeof == 32)
#define CL_SIZE          32
#define CL_SELF          8
#define CL_PROTOREF      12
#define CL_UPVALSREF     16
#define CL_PROTOIDX      20
#define CL_NUPVALS       24

// Upvalue layout (sizeof == 48)
#define UV_SIZE          48
#define UV_MARK          5
#define UV_SELF          8
#define UV_OWNERREF      12
#define UV_CLOSED        16
#define UV_STACKIDX      40

// ICEntry layout (sizeof == 48)
#define IC_SIZE          48
#define IC_VAL           0
#define IC_EPOCH         16
#define IC_SHAPEREF      24
#define IC_TARGETSHAPE   28
#define IC_HOLDERREF     32
#define IC_POLYREF       36
#define IC_SLOT          40
#define IC_KIND          44
#define IC_NOMETA        45
#define IC_PROTOABSENT   46
#define IC_POLYCOUNT     47

#define IC_EMPTY         0
#define IC_OWNSLOT       1
#define IC_PROTOVAL      2
#define IC_ADDSLOT       3
#define IC_ENVSLOT       4

// polyEntry layout (sizeof == 40)
#define POLY_SIZE        40
#define POLY_VAL         0
#define POLY_EPOCH       16
#define POLY_SHAPEREF    24
#define POLY_TARGETSHAPE 28
#define POLY_SLOT        32
#define POLY_KIND        36
#define POLY_NOMETA      37
#define POLY_PROTOABSENT 38

// tableHash layout (sizeof == 32)
#define THASH_NODESREF   0
#define THASH_MASK       4
#define THASH_DICTGEN    8

// hashNode layout (sizeof == 40)
#define HNODE_SIZE       40
#define HNODE_KEY        0
#define HNODE_VAL        16
#define HNODE_STATE      32

// boundProto layout (!moeluaregcheck)
#define BP_INSNS         8
#define BP_CONSTS        16
#define BP_ICSLOTS       40
#define BP_IC            64
#define BP_NUMPARAMS     128
#define BP_MAXSTACK      129
#define BP_ISVARARG      130

// callFrame layout (sizeof == 200)
#define CF_SIZE          200
#define CF_KIND          0
#define CF_CLREF         4
#define CF_VARARGTABREF  8
#define CF_BASE          16
#define CF_FUNCIDX       24
#define CF_PC            32
#define CF_NRESULTS      40
#define CF_NEXTRAARGS    48
#define CF_FINISH        96
#define CF_DIRTY         176
#define CF_ISTAILCALL    177
#define CF_EXTRAARGS     183
#define CF_OLDPC         184

#define FRAME_LUA        0
#define FINISH_NONE      0

// Thread layout
#define TH_SELF          8
#define TH_STACKLEN      48
#define TH_TOP           56
#define TH_HIGHSTACK     64
#define TH_FRAMESCAP     72
#define TH_NFRAME        80
#define TH_HIGHFRAMES    88

// LuaString layout (sizeof == 32)
#define STR_KIND        4
#define STR_LEN         20
#define GCKIND_SHORTSTR 1

// VM stack headroom constant
#define STACK_SLACK      264

// Bytecode immediate offsets
#define OFFSET_SC        127
#define OFFSET_SBX       65535
#define OFFSET_SJ        16777215
