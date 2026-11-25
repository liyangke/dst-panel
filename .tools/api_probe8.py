import zipfile, re

Z = zipfile.ZipFile(r'D:/dst-transfer/server_linux/data/databundles/scripts.zip')
for f in ['scripts/mods.lua', 'scripts/modutil.lua']:
    s = Z.read(f).decode('utf-8', 'replace')
    print('#####', f)
    for pat in [r'package\.path', r'env\.require', r'\brequire\b', r'GetModPath', r'setfenv', r'MOD_ROOT']:
        hits = list(re.finditer(pat, s))
        print('  ==', pat, len(hits))
        for m in hits[:3]:
            i = m.start()
            print('     ', s[max(0, i - 100):i + 160].replace('\n', ' ')[:240])
