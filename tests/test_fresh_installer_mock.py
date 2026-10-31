"""No-TV simulation of first-time setup; fixed paths rewritten to a temp tree."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[1]

class FreshInstallerMock(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        d=Path(self.tmp.name)
        self.base=d/'cvpn'
        self.stage=d/'upload'
        self.hb=d/'homebrew'
        self.hb.mkdir()
        (self.hb/'startup.sh').write_text('#!/bin/sh\n')
        self.stage.mkdir()
        self.proc_mounts = d / 'proc_mounts'
        self.proc_mounts.write_text('none /sys sysfs rw 0 0\n')
        self.script=d/'setup.sh'
        src=(ROOT/'scripts/install-root.sh').read_text()
        src=src.replace('/media/developer/cvpn-webos',str(self.base))
        src=src.replace('/var/lib/webosbrew/init.d',str(self.hb/'init.d'))
        src=src.replace('/var/lib/webosbrew/startup.sh',str(self.hb/'startup.sh'))
        src=src.replace('/tmp/cvpn-root-setup',str(self.stage))
        src=src.replace('/proc/mounts',str(self.proc_mounts))
        self.script.write_text(src)
        self.bindir=d/'bin';self.bindir.mkdir()
        commands={
         # Emulate root only inside the isolated test process.
         # The production installer retains its real `id -u` requirement.
         'id':'#!/bin/sh\nif [ "$#" -eq 1 ] && [ "$1" = "-u" ]; then printf "0\\n"; else exec /usr/bin/id "$@"; fi\n',
         'uname':'#!/bin/sh\nif [ "$1" = -m ]; then echo armv7l; else /usr/bin/uname "$@"; fi\n',
         'ip':'#!/bin/sh\ncase "$*" in\n "link show dev cvpn0") exit 1;;\n "-4 route show 0.0.0.0/1"|"-4 route show 128.0.0.0/1") exit 0;;\n *) exit 1;;\nesac\n',
         'curl':'#!/bin/sh\necho \'{"version":"2.0.0"}\'\n'
        }
        for name,contents in commands.items():
            p=self.bindir/name;p.write_text(contents);p.chmod(0o755)
        self.env=os.environ.copy(); self.env['PATH']=str(self.bindir)+':'+self.env.get('PATH','')
        self.contents={
            'panel':b'#!/bin/sh\necho mock-panel\n',
            'engine':(ROOT/'engine/cvpn.sh').read_bytes(),
            'worker':(ROOT/'recovery/recovery-worker.sh').read_bytes(),
            'boot-hook':(ROOT/'recovery/boot-hook.sh').read_bytes(),
            'start-panel':b'#!/bin/sh\necho PANEL_RUNNING=YES\n',
            'stop-panel':b'#!/bin/sh\necho PANEL_STOP_REQUESTED=YES\n',
            'xray':b'#!/bin/sh\n[ "$1" = version ] || exit 1\necho Xray-mock\n'
        }
        for name,b in self.contents.items():
            (self.stage/name).write_bytes(b)
        checks=''.join(f'{hashlib.sha256(b).hexdigest()}  {name}\n' for name,b in self.contents.items())
        (self.stage/'payload.sha256').write_text(checks)

    def act(self,mode):
        return subprocess.run(['/bin/sh',str(self.script),mode],env=self.env,text=True,capture_output=True,timeout=20)

    def test_rejects_non_root_in_production(self):
        # Explicitly verify the real installer still enforces root privileges.
        fake_id=self.bindir/'id'
        fake_id.write_text('#!/bin/sh\nif [ "$1" = "-u" ]; then echo 1000; else exec /usr/bin/id "$@"; fi\n')
        fake_id.chmod(0o755)
        res=self.act('check')
        self.assertNotEqual(res.returncode,0)
        self.assertIn('REFUSED=root_required',res.stderr)
        self.assertFalse(self.base.exists())

    def set_kernel_arch(self, architecture, bits='32'):
        # Mock kernel and userspace separately in this temporary test directory.
        uname=self.bindir/'uname'
        uname.write_text(f'#!/bin/sh\nif [ "$1" = -m ]; then echo {architecture}; else exec /usr/bin/uname "$@"; fi\n')
        uname.chmod(0o755)
        getconf=self.bindir/'getconf'
        getconf.write_text(f'#!/bin/sh\nif [ "$1" = LONG_BIT ]; then echo {bits}; else exec /usr/bin/getconf "$@"; fi\n')
        getconf.chmod(0o755)

    def test_aarch64_kernel_with_32bit_userspace(self):
        self.set_kernel_arch('aarch64', '32')
        res=self.act('check')
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn('TARGET_ABI=armv7_on_aarch64_kernel', res.stdout)
        self.assertIn('FRESH_PREFLIGHT_OK=YES', res.stdout)
        self.assertFalse(self.base.exists())

    def test_aarch64_kernel_with_64bit_userspace_rejected(self):
        self.set_kernel_arch('aarch64', '64')
        res=self.act('check')
        self.assertNotEqual(res.returncode, 0)
        self.assertIn('REFUSED=armv7_userspace_required', res.stderr)
        self.assertFalse(self.base.exists())

    def test_unknown_kernel_arch_rejected(self):
        self.set_kernel_arch('x86_64', '64')
        res=self.act('check')
        self.assertNotEqual(res.returncode, 0)
        self.assertIn('REFUSED=unsupported_kernel_arch:x86_64', res.stderr)
        self.assertFalse(self.base.exists())

    def test_first_install_aarch64_kernel_32bit_userspace(self):
        self.set_kernel_arch('aarch64', '32')
        res=self.act('install')
        self.assertEqual(res.returncode, 0, res.stderr+res.stdout)
        self.assertIn('FRESH_INSTALLED=YES', res.stdout)
        self.assertTrue((self.base/'webui/access.key').exists())

    def test_check_does_not_create_user_store(self):
        res=self.act('check')
        self.assertEqual(res.returncode,0,res.stderr)
        self.assertIn('FRESH_PREFLIGHT_OK=YES',res.stdout)
        self.assertFalse(self.base.exists())

    def test_first_install_creates_unique_key_and_hook(self):
        res=self.act('install')
        self.assertEqual(res.returncode,0,res.stderr+res.stdout)
        self.assertIn('FRESH_INSTALLED=YES',res.stdout)
        key=(self.base/'webui/access.key').read_text().strip()
        self.assertEqual(len(key),64)
        self.assertTrue(all(ch in '0123456789abcdef' for ch in key))
        self.assertTrue((self.hb/'init.d/90-cvpn-webos').is_symlink())
        self.assertEqual((self.base/'bin/cvpn.sh').read_bytes(),self.contents['engine'])
        second=self.act('check')
        self.assertNotEqual(second.returncode,0)
        self.assertIn('existing_install_use_legacy_upgrade',second.stderr)

    def test_bad_stage_refuses_install(self):
        (self.stage/'panel').write_bytes(b'changed')
        res=self.act('install')
        self.assertNotEqual(res.returncode,0)
        self.assertIn('staged_hash_mismatch',res.stderr)
        self.assertFalse(self.base.exists())

    def test_missing_xray_refuses_install(self):
        (self.stage/'xray').unlink()
        checks=''.join(f'{hashlib.sha256(b).hexdigest()}  {name}\n' for name,b in self.contents.items() if name!='xray')
        (self.stage/'payload.sha256').write_text(checks)
        res=self.act('install')
        self.assertNotEqual(res.returncode,0)
        self.assertIn('xray_binary_required',res.stderr)
        self.assertFalse(self.base.exists())
