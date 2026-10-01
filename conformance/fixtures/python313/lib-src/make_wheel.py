"""Packs lib-src/fixture_lib into a wheel in the current directory."""

import base64
import hashlib
import zipfile

NAME, VERSION = "fixture_lib", "1.0.0"
DIST = f"{NAME}-{VERSION}.dist-info"

with open(f"lib-src/{NAME}/__init__.py", "rb") as source:
    files = {
        f"{NAME}/__init__.py": source.read(),
        f"{DIST}/METADATA": f"Metadata-Version: 2.1\nName: fixture-lib\nVersion: {VERSION}\n".encode(),
        f"{DIST}/WHEEL": b"Wheel-Version: 1.0\nGenerator: fixture\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
    }
record = []
for path, data in files.items():
    digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
    record.append(f"{path},sha256={digest},{len(data)}")
record.append(f"{DIST}/RECORD,,")
files[f"{DIST}/RECORD"] = ("\n".join(record) + "\n").encode()

with zipfile.ZipFile(f"{NAME}-{VERSION}-py3-none-any.whl", "w") as wheel:
    for path, data in files.items():
        wheel.writestr(path, data)
