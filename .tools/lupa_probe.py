from lupa.lua51 import LuaRuntime

for unpack in (True, False):
    print("=== unpack_returned_tuples =", unpack)
    lua = LuaRuntime(unpack_returned_tuples=unpack, encoding=None)
    lua.globals().RET_BOOL = lambda: True
    lua.globals().RET_BYTES = lambda: b"abc"
    lua.globals().RET_TUPLE = lambda: (True, b"abc")
    lua.globals().RET_TRIPLE = lambda: (1, 2, 3)
    lua.globals().RET_NONE = lambda: None

    def show(name):
        r = lua.eval(
            "function() local a, b, c = %s() return tostring(a), tostring(b), tostring(c), select('#', %s()) end"
            % (name, name)
        )()
        print("  ", name, "->", r)

    for n in ("RET_BOOL", "RET_BYTES", "RET_TUPLE", "RET_TRIPLE", "RET_NONE"):
        show(n)

    # does a python function receive and see a lua callback correctly?
    seen = []

    def get_cb(path, cb):
        seen.append(path)
        cb(True, b"data-for-" + path)

    lua.globals().GET = get_cb
    lua.execute(
        """
        GOT = {}
        GET('p1', function(ok, data) GOT.ok = ok GOT.data = data end)
        """
    )
    print("   seen:", seen, "got:", lua.eval("GOT"), type(lua.eval("GOT.ok")), lua.eval("GOT.data"))
