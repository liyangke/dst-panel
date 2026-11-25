import zipfile, re

Z = zipfile.ZipFile(r'D:/dst-transfer/server_linux/data/databundles/scripts.zip')
lus = [n for n in Z.namelist() if n.endswith('.lua')]


def dump(path, pattern, ctx=200, limit=6):
    for n in lus:
        if not n.endswith(path):
            continue
        s = Z.read(n).decode('utf-8', 'replace')
        print('#####', n)
        for m in list(re.finditer(pattern, s))[:limit]:
            i = m.start()
            print('  --', s[max(0, i - 120):i + ctx].replace('\n', ' ')[:320])


dump('modutil.lua', r'function M\.\w+')
dump('modutil.lua', r'GetModConfigData')
dump('mods.lua', r'GetModConfigData')
dump('modindex.lua', r'GetModConfigData')
dump('componentactions.lua', r'AddPrefabPostInit')
dump('mainfunctions.lua', r'AddPrefabPostInit')
dump('prefabmanager.lua', r'AddPrefabPostInit')
print('\n== where is AddPrefabPostInit defined / exposed to mods')
for n in lus:
    s = Z.read(n).decode('utf-8', 'replace')
    if 'function AddPrefabPostInit' in s or 'AddPrefabPostInit =' in s:
        i = s.find('AddPrefabPostInit')
        print('  ', n, '::', s[max(0, i - 150):i + 250].replace('\n', ' ')[:330])

print('\n== modmain sandbox globals (look for the table handed to modmain)')
for n in lus:
    s = Z.read(n).decode('utf-8', 'replace')
    if 'modmain' in s and ('GetModConfigData' in s or 'MOD_INI' in s or 'setfenv' in s or '_ENV' in s):
        i = s.find('modmain')
        print('  ', n, '::', s[max(0, i - 200):i + 400].replace('\n', ' ')[:520])

print('\n== ismastersim on world inst')
for n in lus:
    s = Z.read(n).decode('utf-8', 'replace')
    if 'inst.ismastersim' in s:
        print('  ', n, s.count('inst.ismastersim'))
print('\n== TheShard global definition search')
for n in lus:
    s = Z.read(n).decode('utf-8', 'replace')
    for m in re.finditer(r'TheShard\s*=', s):
        print('  ', n, '::', s[max(0, m.start() - 120):m.start() + 160].replace('\n', ' '))
