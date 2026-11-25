import zipfile, re

Z = zipfile.ZipFile(r'D:/dst-transfer/server_linux/data/databundles/scripts.zip')
s = Z.read('scripts/modutil.lua').decode('utf-8', 'replace')

print('== env.Add* exposed to modmain')
for m in re.finditer(r'env\.(Add\w+|Register\w+|GetModConfigData|modname)\s*=', s):
    print('  ', m.group(1))

i = s.find('InsertPostInitFunctions')
print('\n== environment construction context')
print(s[max(0, i - 1600):i + 200].replace('\r', ''))
