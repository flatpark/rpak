#!/usr/bin/python3
# Generates the GVariant fixtures with GLib itself (python3-gi); see gen.sh.
#
# For every case NAME it writes NAME.gvariant, the a{sv} as GLib serialises
# it, and NAME.stripped.gvariant, the same dict without xa.extra-data
# (entries in the same order), also serialised by GLib. The Go tests check
# that rpak turns the first into the second byte for byte.
#
# Usage: gen.py OUTDIR [REAL.commitmeta]
import sys

import gi

gi.require_version("GLib", "2.0")
from gi.repository import GLib  # noqa: E402

KEY = "xa.extra-data"
out = sys.argv[1]


def V(type_string, value):
    return GLib.Variant(type_string, value)


def extra_data(*files):
    # flatpak_dir_pull_extra_data(): a(ayay) of (name + NUL, contents)
    return V("a(ayay)", [(name.encode() + b"\0", data) for name, data in files])


def gpgsigs(n, size):
    return V("aay", [bytes((i * 7 + j) & 0xFF for j in range(size)) for i in range(n)])


def serialise(entries):
    builder = GLib.VariantBuilder.new(GLib.VariantType.new("a{sv}"))
    for key, value in entries:
        builder.add_value(GLib.Variant.new_dict_entry(V("s", key), GLib.Variant.new_variant(value)))
    return builder.end().get_normal_form().get_data_as_bytes().get_data()


def write(name, entries):
    with open(f"{out}/{name}.gvariant", "wb") as f:
        f.write(serialise(entries))
    with open(f"{out}/{name}.stripped.gvariant", "wb") as f:
        f.write(serialise([(k, v) for k, v in entries if k != KEY]))


small = extra_data(("app.deb", b"\x7fELF" + bytes(range(40))))

write("empty", [])
write("only-extra-data", [(KEY, small)])
write("no-extra-data", [("ostree.gpgsigs", gpgsigs(1, 40)), ("xa.note", V("s", "hello"))])
# xa.extra-data first, in the middle and last, between keys of other types
write("mixed", [
    ("ostree.gpgsigs", gpgsigs(2, 30)),
    (KEY, small),
    ("xa.note", V("s", "über")),
    ("xa.count", V("u", 42)),
    ("xa.flag", V("b", True)),
    ("xa.pair", V("(xs)", (-1, "x"))),
])
write("first", [(KEY, small), ("xa.note", V("s", "a")), ("xa.count", V("t", 7))])
write("last", [("xa.note", V("s", "a")), ("xa.count", V("q", 7)), (KEY, small)])
# Values big enough for 2- and 4-byte framing offsets in the entry and the
# array, staying that way after stripping.
write("offsets-2", [
    ("ostree.gpgsigs", gpgsigs(3, 400)),
    (KEY, extra_data(("a.bin", bytes(300)))),
    ("xa.note", V("s", "n" * 100)),
])
write("offsets-4", [
    ("ostree.gpgsigs", gpgsigs(1, 70000)),
    (KEY, extra_data(("a.bin", bytes(80000)), ("b.bin", bytes(10)))),
    ("xa.note", V("s", "n")),
])
# Stripping shrinks the array enough for smaller framing offsets.
write("shrink-4-to-1", [
    ("ostree.gpgsigs", gpgsigs(1, 60)),
    (KEY, extra_data(("big.bin", bytes(70000)))),
])
write("shrink-2-to-1", [
    (KEY, extra_data(("mid.bin", bytes(300)))),
    ("xa.note", V("s", "short")),
])
# Two entries with the key: GVariantDict would keep one; rpak drops both.
write("duplicate-extra-data", [(KEY, small), ("xa.note", V("s", "a")), (KEY, small)])

if len(sys.argv) > 2:
    # A real commitmeta from a stock flatpak pull: the same keys, in the same
    # order, with the real signatures; only the embedded installers are cut
    # down to 4 KiB so the fixture stays small.
    with open(sys.argv[2], "rb") as f:
        real = GLib.Variant.new_from_bytes(GLib.VariantType.new("a{sv}"), GLib.Bytes.new(f.read()), False)
    assert real.is_normal_form()
    entries = []
    for i in range(real.n_children()):
        entry = real.get_child_value(i)
        key = entry.get_child_value(0).get_string()
        value = entry.get_child_value(1).get_variant()
        print(f"real: {key} {value.get_type_string()} {value.get_size()} bytes")
        if key == KEY:
            files = []
            for j in range(value.n_children()):
                name, data = (value.get_child_value(j).get_child_value(k).get_data_as_bytes().get_data()
                              for k in (0, 1))
                files.append((name.rstrip(b"\0").decode(), data[:4096]))
            value = extra_data(*files)
        entries.append((key, value))
    write("real", entries)
