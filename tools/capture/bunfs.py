"""Read the files embedded in a Bun single-file executable, without running it.

`bun build --compile` appends a module graph to the runtime binary. On Linux it
sits in the ELF section `.bun`: a u64 payload length, the payload, and inside
the payload a 32-byte offsets record followed by a trailer line. Every offset is
relative to the start of the payload.
"""

import struct
from dataclasses import dataclass

TRAILER = b"\n---- Bun! ----\n"
ROOT = "/$bunfs/root/"

# name, contents, sourcemap, bytecode, module_info, bytecode_origin_path as
# (offset, length) pairs, then encoding, loader, module_format, side.
RECORD = struct.Struct("<12I4B")
OFFSETS = struct.Struct("<QIIIIII")


class BundleError(Exception):
    pass


@dataclass(frozen=True)
class Module:
    name: str
    data: bytes
    loader: int
    fmt: int


def elf_section(binary: bytes, wanted: str) -> tuple[int, int]:
    if binary[:4] != b"\x7fELF" or binary[4] != 2 or binary[5] != 1:
        raise BundleError("not a little-endian ELF64 file")
    shoff = struct.unpack_from("<Q", binary, 0x28)[0]
    shentsize, shnum, shstrndx = struct.unpack_from("<HHH", binary, 0x3A)
    if shentsize != 64:
        raise BundleError(f"unexpected section header size {shentsize}")

    def header(i):
        name, _type, _flags, _addr, off, size = struct.unpack_from("<IIQQQQ", binary, shoff + i * 64)
        return name, off, size

    _, stroff, strsize = header(shstrndx)
    names = binary[stroff : stroff + strsize]
    for i in range(shnum):
        name, off, size = header(i)
        end = names.index(b"\0", name)
        if names[name:end].decode() == wanted:
            return off, size
    raise BundleError(f"ELF section {wanted} not found")


def modules(binary: bytes) -> list[Module]:
    off, size = elf_section(binary, ".bun")
    (length,) = struct.unpack_from("<Q", binary, off)
    if length + 8 > size:
        raise BundleError(f".bun payload length {length} exceeds section size {size}")
    payload = memoryview(binary)[off + 8 : off + 8 + length]
    if bytes(payload[-len(TRAILER) :]) != TRAILER:
        raise BundleError("Bun trailer not found at the end of the .bun payload")
    at = length - len(TRAILER) - OFFSETS.size
    byte_count, mods_off, mods_len, _entry, _argv_off, _argv_len, _flags = OFFSETS.unpack_from(payload, at)
    if byte_count != at:
        raise BundleError(f"offsets record says {byte_count} bytes, found {at}")
    if mods_len % RECORD.size:
        raise BundleError(f"module table length {mods_len} is not a multiple of {RECORD.size}")

    def span(o, n):
        if o + n > byte_count:
            raise BundleError("module span outside the payload")
        return bytes(payload[o : o + n])

    out = []
    for at in range(mods_off, mods_off + mods_len, RECORD.size):
        r = RECORD.unpack_from(payload, at)
        name = span(r[0], r[1]).decode("utf-8")
        if not name.startswith("/$bunfs/"):
            raise BundleError(f"unexpected module name {name!r}: the record layout has changed")
        out.append(Module(name, span(r[2], r[3]), r[13], r[14]))
    if not out:
        raise BundleError("module table is empty")
    return out
