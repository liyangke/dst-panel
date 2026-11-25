import zipfile, re, collections

Z = zipfile.ZipFile(r'D:/dst-transfer/server_linux/data/databundles/scripts.zip')
lus = [n for n in Z.namelist() if n.endswith('.lua')]


def scan(pattern, limit=4):
    rx = re.compile(pattern)
    out = []
    for n in lus:
        s = Z.read(n).decode('utf-8', 'replace')
        for m in rx.finditer(s):
            i = m.start()
            out.append((n, s[max(0, i - 40):i + 120].replace('\n', ' ')))
            if len(out) >= limit:
                return out
    return out


print('== experience components', collections.Counter(
    m.group(1) for n in lus
    for m in re.finditer(r'AddComponent\("(\w*experience\w*)"', Z.read(n).decode('utf-8', 'replace'))
))
for p in [r'maxhealth', r'components\.hunger\.max', r'components\.sanity\.max',
          r'experience:GetExperience\(\)', r'GetShardId\(\)', r'GetSessionIdentifier\(\)',
          r'TheShard\s*=', r'inventario']:
    print('\n==', p)
    for n, ctx in scan(p):
        print('  ', n, '::', ctx[:130])
