import zipfile

ZIP = r"D:/dst-transfer/server_linux/data/databundles/scripts.zip"

RANGES = {
    "scripts/modindex.lua": [(88, 120), (255, 340), (360, 475)],
}

with zipfile.ZipFile(ZIP) as z:
    for path, ranges in RANGES.items():
        lines = z.read(path).decode("utf-8", "replace").splitlines()
        for a, b in ranges:
            print("-" * 24, path, "%d-%d" % (a, b))
            for i in range(a, min(b, len(lines)) + 1):
                print("%4d| %s" % (i, lines[i - 1]))
