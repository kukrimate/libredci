#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-2.0-only
"""Set up Intel OpenIPC and ipccli from System Studio 2020 Update 3.

  - download the RPMs from Intel (or the Wayback Machine if Intel's server
    fails) into a cache, unless they are already there, and check them
  - extract OpenIPC, decrypt its configuration and add the CSME core
  - create a Python venv with ipccli (patched for Python >= 3.12) and IPython
  - write DEST/ipc, which starts IPython with `ipc = ipccli.baseaccess()`

usage: setup.py DEST
Needs rpm2cpio, cpio, patch and the Python cryptography package (Debian:
python3-cryptography) on an x86-64 Linux host.
"""
import glob
import hashlib
import os
import shutil
import subprocess
import sys
import urllib.request
from dataclasses import dataclass
from pathlib import Path

from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes

HERE = Path(__file__).resolve().parent
BASE = "https://software.repos.intel.com/system_debugger/2020p3/lin/common"
OPENIPC_VER = "1.2035.4868.100"
WHEEL = "ipccli-1.2035.1920.100-py2.py3-none-any.whl"


@dataclass(frozen=True)
class RPM:
    """A package from Intel's server, with its Wayback Machine snapshot."""

    name: str
    wayback: str  # snapshot timestamp
    digest: str  # SHA-256

    def urls(self):
        """Returns the download URLs in order of preference."""
        url = f"{BASE}/{self.name}"
        return [url, f"https://web.archive.org/web/{self.wayback}id_/{url}"]


OPENIPC = RPM(
    f"intel-openipc-linux-2037RC0-{OPENIPC_VER}.noarch.rpm",
    "20240415084633",
    "51aea6e65eaf2c06b7b4466e8255c78ad9dd14eb00735e886068fbdd9144da9f",
)
IPCCLI = RPM(
    "intel-ipccli-linux-2037RC0-1.2035.1920.100.noarch.rpm",
    "20240415084329",
    "53bdfb6095b9993cb922245f02afa8d626e34d1728b60f0284508ae4cb813cce",
)

# The key of OpenIPC's encrypted configuration is in this library's .rodata.
KEY_LIB = "Bin/libStructuredData_x64.so"
KEY_OFFSET = 0x5175F3

CACHE = (
    Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "libredci-openipc"
)


def file_sha256(path):
    """Returns the hex SHA-256 of a file."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while chunk := f.read(1 << 20):
            h.update(chunk)
    return h.hexdigest()


def fetch(rpm):
    """Returns the cached RPM, downloading it first if needed."""
    path = CACHE / rpm.name
    if path.exists() and file_sha256(path) == rpm.digest:
        return path
    CACHE.mkdir(parents=True, exist_ok=True)
    part = path.with_suffix(".part")
    for url in rpm.urls():
        print(f"downloading {url}")
        try:
            with urllib.request.urlopen(url, timeout=60) as r, open(part, "wb") as f:
                shutil.copyfileobj(r, f)
            break
        except OSError as e:
            print(f"  failed: {e}")
    else:
        sys.exit(f"could not download {rpm.name}")
    if file_sha256(part) != rpm.digest:
        sys.exit(f"{part}: SHA-256 mismatch")
    part.rename(path)
    return path


def extract(rpm, dest):
    """Extracts an RPM's files into dest."""
    with subprocess.Popen(["rpm2cpio", rpm], stdout=subprocess.PIPE) as rpm2cpio:
        subprocess.run(
            ["cpio", "-idm", "--quiet"], stdin=rpm2cpio.stdout, cwd=dest, check=True
        )
    if rpm2cpio.returncode:
        sys.exit(f"rpm2cpio {rpm} failed")


def patch(directory, patch_file):
    """Applies one of this directory's patches to directory."""
    with open(HERE / patch_file, "rb") as f:
        subprocess.run(["patch", "-d", directory, "-p1"], stdin=f, check=True)


def evp_bytes_to_key(secret, key_len=32, iv_len=16, rounds=5):
    """OpenSSL's EVP_BytesToKey with SHA-1 and no salt: returns (key, iv)."""
    out, block = b"", b""
    while len(out) < key_len + iv_len:
        block = hashlib.sha1(block + secret).digest()
        for _ in range(rounds - 1):
            block = hashlib.sha1(block).digest()
        out += block
    return out[:key_len], out[key_len : key_len + iv_len]


NAME_ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"


def plain_name(name):
    """Undoes the name obfuscation: each alphabet character's index is XORed
    with 0xA and its position in the run of alphabet characters (from 1).
    Other characters, and results outside the alphabet, are kept as they are
    and restart the count."""
    out, pos = [], 0
    for c in name:
        i = NAME_ALPHABET.find(c)
        if i >= 0:
            pos += 1
            i ^= pos ^ 0xA
        if 0 <= i < len(NAME_ALPHABET):
            out.append(NAME_ALPHABET[i])
        else:
            out.append(c)
            pos = 0
    return "".join(out)


class BinaryXML:
    """Converts binary XML to text, two spaces of indentation per level.

    Layout (integers are little-endian u32): an optional "!LAD toN" magic,
    a string table (count, then length and bytes of each string) and the
    root element. An element is its tag's string index, a flags byte and,
    in order, when flag 1 is set an unused integer; flag 2 a text string
    index; flag 4 an attribute count and name/value string index pairs;
    flag 8 a child count and the child elements."""

    def __init__(self, data, plain_paths):
        self.data, self.plain_paths = data, plain_paths
        self.pos = 8 if data.startswith(b"!LAD toN") else 0
        self.strings = [self.take(self.u32()) for _ in range(self.u32())]
        self.lines = []
        self.element(b"")

    def take(self, n):
        """Returns the next n bytes."""
        self.pos += n
        return self.data[self.pos - n : self.pos]

    def u32(self):
        """Returns the next little-endian 32-bit integer."""
        return int.from_bytes(self.take(4), "little")

    def string(self):
        """Returns the string table entry indexed by the next integer."""
        return self.strings[self.u32()]

    def element(self, indent):
        """Converts the next element and its children."""
        tag = self.string()
        flags = self.take(1)[0]
        if flags & 1:
            self.u32()
        text = self.string().strip() if flags & 2 else b""
        start = indent + b"<" + tag
        for _ in range(self.u32() if flags & 4 else 0):
            name, value = self.string(), self.string()
            if self.plain_paths and name == b"Path":
                value = plain_name(value.decode("latin-1")).encode("latin-1")
            start += b" " + name + b'="' + value + b'"'
        end = indent + b"</" + tag + b">"
        if flags & 8:
            if text:
                raise ValueError(f"element {tag} has both text and children")
            self.lines.append(start + b">")
            for _ in range(self.u32()):
                self.element(indent + b"  ")
            self.lines.append(end)
        elif text:
            self.lines += [start + b">", indent + b"  " + text, end]
        else:
            self.lines.append(start + b"/>")

    def text(self):
        """Returns the converted document."""
        return b"\n".join(self.lines) + b"\n"


def decrypt_config(openipc, secret):
    """Decrypts an OpenIPC tree's configuration in place.

    Config/Index.bin and everything under Data/ are encrypted with
    AES-256-CBC and PKCS#7 padding, with the key and IV derived from a
    16-byte secret. XML and XSD files are stored as binary XML, Python files
    as text. Names under Data/ are obfuscated too (see plain_name).
    """
    key, iv = evp_bytes_to_key(secret)

    def plaintext(path):
        d = Cipher(algorithms.AES(key), modes.CBC(iv)).decryptor()
        data = d.update(path.read_bytes()) + d.finalize()
        path.unlink()
        return data[: -data[-1]]

    def decrypt_file(path, dest, plain_paths=False):
        data = plaintext(path)
        if dest.suffix in (".xml", ".xsd"):
            data = BinaryXML(data, plain_paths).text()
        elif dest.suffix != ".py":
            raise ValueError(f"{path}: unexpected file type {dest.name}")
        dest.write_bytes(data)

    def decrypt_tree(directory, skip=()):
        for path in sorted(directory.iterdir()):
            dest = directory / plain_name(path.name)
            if path.name in skip:
                continue
            if path.is_dir():
                path.rename(dest)
                decrypt_tree(dest)
            else:
                decrypt_file(path, dest)

    data = openipc / "Data"
    decrypt_tree(data, skip={"Index.bin"})
    decrypt_file(data / "Index.bin", data / "Index.xml", plain_paths=True)
    decrypt_file(openipc / "Config" / "Index.bin", openipc / "Config" / "Index.xml")


LAUNCHER = '''#!{python}
"""Start IPython with ipccli connected to OpenIPC (`ipc` is the baseaccess)."""
import os
import sys

import IPython

os.environ["IPC_PATH"] = {ipc_path!r}
sys.exit(IPython.start_ipython(
    argv=["-i", "-c", "import ipccli; ipc = ipccli.baseaccess()"] + sys.argv[1:]))
'''


def main():
    """Sets up OpenIPC in the directory named on the command line."""
    if len(sys.argv) != 2:
        sys.exit(__doc__.strip())
    dest = Path(sys.argv[1]).resolve()
    if dest.exists():
        sys.exit(f"{dest} already exists")

    rpms = [fetch(OPENIPC), fetch(IPCCLI)]
    work = dest / "rpm"
    work.mkdir(parents=True)
    for rpm in rpms:
        extract(rpm, work)
    openipc = dest / "OpenIPC"
    (work / "tools" / f"OpenIPC_{OPENIPC_VER}").rename(openipc)
    wheel = dest / WHEEL
    (work / "system_debugger_2020" / "debugger" / "ipccli" / WHEEL).rename(wheel)
    shutil.rmtree(work)

    venv = dest / "venv"
    python = venv / "bin" / "python"
    subprocess.run([sys.executable, "-m", "venv", venv], check=True)
    subprocess.run(
        [python, "-m", "pip", "install", "-q", wheel, "packaging", "ipython"],
        check=True,
    )
    (site_packages,) = glob.glob(str(venv / "lib" / "python3.*" / "site-packages"))
    patch(site_packages, "ipccli-py312.patch")

    with open(openipc / KEY_LIB, "rb") as f:
        f.seek(KEY_OFFSET)
        secret = f.read(16)
    decrypt_config(openipc, secret)
    patch(openipc, "csme.patch")

    launcher = dest / "ipc"
    launcher.write_text(LAUNCHER.format(python=python, ipc_path=str(openipc / "Bin")))
    launcher.chmod(0o755)
    print(f"set up in {dest}; run {launcher}")


if __name__ == "__main__":
    main()
