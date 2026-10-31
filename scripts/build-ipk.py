#!/usr/bin/env python3
"""Build reproducible Debian-format webOS IPK without requiring dpkg-deb.

Guarantees:
1. Correct member paths (no ./ prefix) for webOS Homebrew catalog.
2. Mode 100644 in ar headers for BusyBox 1.31.1 compatibility on webOS 22.
3. Bundled Xray, panel, GeoIP & GeoSite routing assets, and recovery scripts.
"""
import gzip
import hashlib
import io
import json
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def build() -> Path:
    appinfo_file = ROOT / "launcher/appinfo.json"
    appinfo = json.loads(appinfo_file.read_text())
    version = appinfo["version"]
    app_id = appinfo["id"]

    payload_dir = ROOT / "dist/payload"
    if not (payload_dir / "SHA256SUMS").is_file():
        raise SystemExit("Missing payload/SHA256SUMS; check payload")

    with tempfile.TemporaryDirectory(prefix="cvpn-build-") as temp_str:
        temp = Path(temp_str)
        app_dir = temp / f"usr/palm/applications/{app_id}"
        pkg_dir = temp / f"usr/palm/packages/{app_id}"
        app_dir.mkdir(parents=True)
        pkg_dir.mkdir(parents=True)

        # 1. Copy payload
        shutil.copytree(payload_dir, app_dir / "payload", symlinks=True)

        # 2. Copy launcher files
        for f in [
            "appinfo.json",
            "index.html",
            "icon.png",
            "largeicon.png",
            "icon-400.png",
            "qr-bundle.js",
            "QR-NOTICE.txt",
            "QR-THIRD-PARTY-LICENSE.txt",
        ]:
            src = ROOT / "launcher" / f
            if src.is_file():
                shutil.copy2(src, app_dir / f)

        shutil.copy2(ROOT / "LICENSE", app_dir / "CVPN-LICENSE.txt")
        shutil.copy2(ROOT / "THIRD_PARTY_NOTICES.md", app_dir / "THIRD_PARTY_NOTICES.md")

        # 3. Create packageinfo.json
        pkg_info = {"id": app_id, "version": version, "app": app_id}
        (pkg_dir / "packageinfo.json").write_text(json.dumps(pkg_info, indent=2))

        # 4. Calculate installed size in KiB
        total_size = sum(f.stat().st_size for f in temp.rglob("*") if f.is_file())
        installed_kib = total_size // 1024

        control_content = f"""Package: {app_id}
Version: {version}
Section: misc
Priority: optional
Architecture: all
Maintainer: CVPN Project
Installed-Size: {installed_kib}
Description: CVPN webOS All-in-One: launcher, root VPN backend, Xray-core and GeoIP routing.
webOS-Package-Format-Version: 2
webOS-Packager-Version: custom-debian2
"""

        # 5. Build control.tar.gz
        control_buf = io.BytesIO()
        with gzip.GzipFile(fileobj=control_buf, mode="wb", filename="", mtime=0) as gz:
            with tarfile.open(fileobj=gz, mode="w", format=tarfile.PAX_FORMAT) as tar:
                ctrl_bytes = control_content.encode("utf-8")
                ti = tarfile.TarInfo(name="control")
                ti.size = len(ctrl_bytes)
                ti.mtime = 0
                ti.mode = 0o644
                ti.uid = 0
                ti.gid = 0
                ti.uname = "root"
                ti.gname = "root"
                tar.addfile(ti, io.BytesIO(ctrl_bytes))
        control_tar_gz = control_buf.getvalue()

        # 6. Build data.tar.gz
        data_buf = io.BytesIO()
        with gzip.GzipFile(fileobj=data_buf, mode="wb", filename="", mtime=0) as gz:
            with tarfile.open(fileobj=gz, mode="w", format=tarfile.PAX_FORMAT) as tar:
                for item in sorted(temp.glob("usr/**/*")):
                    rel_path = item.relative_to(temp).as_posix()
                    ti = tar.gettarinfo(str(item), arcname=rel_path)
                    ti.mtime = 0
                    ti.uid = 0
                    ti.gid = 0
                    ti.uname = "root"
                    ti.gname = "root"
                    if item.is_file():
                        if item.suffix in (".sh", "") and ("bin" in item.parts or "install" in item.name):
                            ti.mode = 0o755
                        else:
                            ti.mode = 0o644
                        with open(item, "rb") as f:
                            tar.addfile(ti, f)
                    elif item.is_dir():
                        ti.mode = 0o755
                        tar.addfile(ti)

        data_tar_gz = data_buf.getvalue()

        # 7. Package into IPK (ar archive) with mode 100644 for BusyBox compatibility
        out_ipk = ROOT / f"dist/{app_id}_{version}_all.ipk"
        members = {
            "debian-binary": b"2.0\n",
            "control.tar.gz": control_tar_gz,
            "data.tar.gz": data_tar_gz,
        }

        with open(out_ipk, "wb") as f:
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

        print(f"IPK_BUILT={out_ipk}")
        print(f"IPK_SIZE={out_ipk.stat().st_size} bytes")

        h = hashlib.sha256(out_ipk.read_bytes()).hexdigest()
        print(f"SHA256={h}")
        (ROOT / "dist/SHA256SUMS").write_text(f"{h}  {out_ipk.name}\n")
        return out_ipk


if __name__ == "__main__":
    build()
