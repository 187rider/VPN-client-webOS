#!/usr/bin/env python3
"""Normalize Debian IPK tar paths for webOS Homebrew's IPK reader.

GNU dpkg-deb writes ./control and ./usr/... into tar members. Homebrew's
catalog scanner expects control and usr/... instead, so normalize only
archive member names; preserve file bytes, modes and link targets.
"""
import argparse
import gzip
import io
from pathlib import Path
import subprocess
import tarfile
import tempfile


def normalize_member_names(blob: bytes) -> bytes:
    output = io.BytesIO()
    with tarfile.open(fileobj=io.BytesIO(blob), mode="r:gz") as source:
        with gzip.GzipFile(fileobj=output, mode="wb", filename="", mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w|", format=tarfile.PAX_FORMAT) as target:
                for member in source:
                    name = member.name
                    while name.startswith("./"):
                        name = name[2:]
                    if name in ("", "."):
                        continue
                    if name.startswith("/") or any(part == ".." for part in name.split("/")):
                        raise ValueError(f"unsafe IPK member path: {member.name}")
                    member.name = name
                    if member.islnk():
                        while member.linkname.startswith("./"):
                            member.linkname = member.linkname[2:]
                    source_file = source.extractfile(member) if member.isfile() else None
                    target.addfile(member, source_file)
    return output.getvalue()


def read_ar_members(ipk: Path) -> dict:
    blob = ipk.read_bytes()
    if not blob.startswith(b"!<arch>\n"):
        raise ValueError("invalid Debian package marker")
    members = {}
    idx = 8
    while idx < len(blob):
        hdr = blob[idx:idx + 60]
        if len(hdr) < 60:
            break
        name = hdr[:16].decode("ascii", errors="replace").strip().rstrip("/")
        size = int(hdr[48:58].decode("ascii", errors="replace").strip())
        members[name] = blob[idx + 60:idx + 60 + size]
        if size % 2 == 1:
            size += 1
        idx += 60 + size
    return members


def write_ar_members(dest: Path, members: dict) -> None:
    with open(dest, "wb") as f:
        f.write(b"!<arch>\n")
        for name, payload in members.items():
            mode_str = "100644"
            hdr = (
                f"{name:<16}"
                f"{0:<12}"
                f"{0:<6}"
                f"{0:<6}"
                f"{mode_str:<8}"
                f"{len(payload):<10}"
                + "`\n"
            ).encode("ascii")
            f.write(hdr)
            f.write(payload)
            if len(payload) % 2 == 1:
                f.write(b"\n")


def normalize(ipk: Path) -> None:
    if not ipk.is_file():
        raise FileNotFoundError(ipk)
    expected_members = ("debian-binary", "control.tar.gz", "data.tar.gz")
    original = read_ar_members(ipk)
    for name in expected_members:
        if name not in original:
            raise ValueError(f"missing archive member: {name}")
    if original["debian-binary"] != b"2.0\n":
        raise ValueError("invalid Debian package marker")
    normalized_members = {}
    for name in expected_members:
        content = original[name]
        normalized_members[name] = normalize_member_names(content) if name.endswith(".tar.gz") else content

    with tempfile.TemporaryDirectory(prefix="cvpn-ipk-") as temp:
        directory = Path(temp)
        candidate = directory / "normalized.ipk"
        write_ar_members(candidate, normalized_members)

        control = normalized_members["control.tar.gz"]
        data = normalized_members["data.tar.gz"]
        with tarfile.open(fileobj=io.BytesIO(control), mode="r:gz") as archive:
            metadata = archive.extractfile(archive.getmember("control")).read().decode("utf-8")
            if "Package: su.cvpn.webos" not in metadata or "Installed-Size:" not in metadata:
                raise ValueError("missing mandatory webOS control metadata")
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
            archive.getmember("usr/palm/applications/su.cvpn.webos/appinfo.json")
        candidate.replace(ipk)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("ipk", type=Path)
    args = parser.parse_args()
    normalize(args.ipk)
    print(f"WEBOS_IPK_NORMALIZED={args.ipk}")
