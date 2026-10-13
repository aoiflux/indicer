"""Write the deterministic input files for gen_legacy_dbs.sh into the CWD.

pad.bin  compressible filler copied first, so a.bin lands across the first
         256 KiB chunk boundary of the FAT volume (RST-1)
a.bin    40000 random bytes; random so it cannot dedup against anything
blob.bin a non-image object of 700001 bytes, a size that is not a multiple of
         any chunk size; low-entropy text so the stored chunks compress
"""

import random

with open("pad.bin", "w", newline="\n") as f:
    f.write("dues fixture padding line\n" * 9600)

r = random.Random(7)
with open("a.bin", "wb") as f:
    f.write(bytes(r.getrandbits(8) for _ in range(40000)))

r = random.Random(11)
words = ["alpha", "beta", "gamma", "delta", "evidence", "chunk"]
text = " ".join(r.choice(words) + str(r.randrange(100)) for _ in range(120000))
with open("blob.bin", "w", newline="\n") as f:
    f.write(text[:700001])
