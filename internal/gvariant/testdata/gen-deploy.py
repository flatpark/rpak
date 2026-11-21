#!/usr/bin/python3
# Generates the deploy-data fixtures with GLib itself (python3-gi); see
# gen-deploy.sh. For every case NAME it writes NAME.deploy, a
# (ssasta{sv}) as flatpak writes it to <deployment>/deploy, and
# NAME.forced.deploy, the same with the "runtime" entry's value replaced
# (entries in the same order), serialised by GLib. The Go tests check that
# rpak turns the first into the second byte for byte.
#
# Usage: gen-deploy.py OUTDIR [REAL.deploy]
import sys

import gi

gi.require_version("GLib", "2.0")
from gi.repository import GLib  # noqa: E402

out = sys.argv[1]
TYPE = GLib.VariantType.new("(ssasta{sv})")
NEW = "org.freedesktop.Platform/x86_64/26.08"


def dict_of(entries):
    b = GLib.VariantBuilder.new(GLib.VariantType.new("a{sv}"))
    for k, v in entries:
        b.add_value(GLib.Variant.new_dict_entry(GLib.Variant("s", k), GLib.Variant.new_variant(v)))
    return b.end()


def deploy(origin, commit, subpaths, size, entries):
    return GLib.Variant.new_tuple(
        GLib.Variant("s", origin), GLib.Variant("s", commit), GLib.Variant("as", subpaths),
        GLib.Variant("t", size), dict_of(entries))


def entries_of(v):
    d = v.get_child_value(4)
    return [(d.get_child_value(i).get_child_value(0).get_string(),
             d.get_child_value(i).get_child_value(1).get_variant()) for i in range(d.n_children())]


def write(name, v, new=NEW):
    v = v.get_normal_form()
    with open(f"{out}/{name}.deploy", "wb") as f:
        f.write(v.get_data_as_bytes().get_data())
    forced = deploy(v.get_child_value(0).get_string(), v.get_child_value(1).get_string(),
                    v.get_child_value(2).get_strv(), v.get_child_value(3).get_uint64(),
                    [(k, GLib.Variant("s", new) if k == "runtime" else val) for k, val in entries_of(v)])
    with open(f"{out}/{name}.forced.deploy", "wb") as f:
        f.write(forced.get_normal_form().get_data_as_bytes().get_data())


OLD = "org.freedesktop.Platform/x86_64/25.08"
C = "0" * 63 + "1"
common = [("deploy-version", GLib.Variant("i", 4)), ("timestamp", GLib.Variant("t", 1700000000))]
# Small enough for 1-byte offsets before, 2-byte after: the framing grows.
write("grow-1-to-2", deploy("r", C, [], 1, [("runtime", GLib.Variant("s", "a/b/1"))]),
      new="org.freedesktop.Platform/x86_64/26.08" + "x" * 120)
# The other way round.
write("shrink-2-to-1", deploy("r", C, [], 1, [("runtime", GLib.Variant("s", "a/b/" + "1" * 150))]), new="a/b/2")
# Runtime first, in the middle, last; subpaths; big-endian size.
write("middle", deploy("flathub", C, ["/de", "/fr"], 123456789,
                       common + [("runtime", GLib.Variant("s", OLD)), ("eol", GLib.Variant("s", "gone"))]))
write("last", deploy("flathub", C, [], 0, common + [("runtime", GLib.Variant("s", OLD))]))
# 4-byte offsets: a large appdata entry.
write("offsets-4", deploy("flathub", C, [], 7, [("runtime", GLib.Variant("s", OLD)),
                                                  ("appdata-summary", GLib.Variant("s", "y" * 70000))]))
# No runtime (a runtime's own deployment): unchanged.
write("no-runtime", deploy("flathub", C, [], 7, common))

if len(sys.argv) > 2:
    with open(sys.argv[2], "rb") as f:
        real = GLib.Variant.new_from_bytes(TYPE, GLib.Bytes.new(f.read()), False)
    write("real", real)
