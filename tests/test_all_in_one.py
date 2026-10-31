"""Isolated installer integration tests. Never touch TV networking or /media."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[1]
SOURCE=(ROOT/'scripts/install-bundled.sh').read_text()

class AllInOneInstaller(unittest.TestCase):
    def setUp(self):
        tmp=tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.temp=Path(tmp.name)
        self.app=self.temp/'app'
        self.payload=self.app/'payload'
        (self.payload/'bin').mkdir(parents=True)
        self.base=self.temp/'base'
        self.hb=self.temp/'homebrew'
        (self.hb/'init.d').mkdir(parents=True)
        (self.hb/'startup.sh').write_text('#!/bin/sh\n')
        installer=SOURCE.replace('/media/developer/apps/usr/palm/applications/su.cvpn.webos',str(self.app))
        installer=installer.replace('/media/developer/cvpn-webos',str(self.base))
        installer=installer.replace('/var/lib/webosbrew/init.d/90-cvpn-webos',str(self.hb/'init.d/90-cvpn-webos'))
        installer=installer.replace('/var/lib/webosbrew/init.d',str(self.hb/'init.d'))
        installer=installer.replace('/var/lib/webosbrew/startup.sh',str(self.hb/'startup.sh'))
        installer=installer.replace('[ -c /dev/net/tun ]','true # fake TUN in isolated test')
        self.installer=self.temp/'test-installer.sh'
        self.installer.write_text(installer)
        self.fakebin=self.temp/'bin'
        self.fakebin.mkdir()
        commands={
            'id':'#!/bin/sh\necho 0\n',
            'uname':'#!/bin/sh\necho aarch64\n',
            'getconf':'#!/bin/sh\necho 32\n',
            'ip':'#!/bin/sh\ncase "$*" in\n "link show dev cvpn0") exit 1;;\n "-4 route show "*) exit 0;;\n *) exit 1;;\nesac\n',
            'curl':'#!/bin/sh\necho \'{"version":"2.0.0"}\'\n'
        }
        for name,src in commands.items():
            out=self.fakebin/name
            out.write_text(src)
            out.chmod(0o755)
        self.env=os.environ.copy()
        self.env['PATH']=str(self.fakebin)+os.pathsep+self.env.get('PATH','')
        files={
            'xray':'#!/bin/sh\necho Xray-mock\n',
            'panel':'#!/bin/sh\necho mock-panel\n',
            'engine':'#!/bin/sh\necho engine\n',
            'worker':'#!/bin/sh\necho worker\n',
            'boot-hook':'#!/bin/sh\necho boot\n',
            'start-panel':'#!/bin/sh\necho PANEL_RUNNING=YES\n',
            'stop-panel':'#!/bin/sh\necho PANEL_STOP_REQUESTED=YES\n',
            'geoip.dat':'mock-geoip\n',
            'geosite.dat':'mock-geosite\n',
        }
        for name,src in files.items():
            out=self.payload/'bin'/name
            out.write_text(src)
            out.chmod(0o755)
        self.regenerate_sums()

    def regenerate_sums(self):
        files=sorted((self.payload/'bin').iterdir())
        data=''.join(f'{hashlib.sha256(f.read_bytes()).hexdigest()}  bin/{f.name}\n' for f in files)
        (self.payload/'SHA256SUMS').write_text(data)

    def run_it(self,action):
        return subprocess.run(['/bin/sh',str(self.installer),action],env=self.env,text=True,capture_output=True,timeout=20)

    def test_install_and_idempotent_reopen(self):
        self.assertIn('BUNDLE_READY=NO',self.run_it('status').stdout)
        res=self.run_it('install')
        self.assertEqual(res.returncode,0,res.stderr+res.stdout)
        self.assertIn('USER_DATA_PRESERVED=YES',res.stdout)
        self.assertIn('BUNDLE_READY=YES',self.run_it('status').stdout)
        self.assertIn('BUNDLE_READY=YES',self.run_it('install').stdout)
        hook=self.hb/'init.d/90-cvpn-webos'
        self.assertEqual(hook.resolve(),(self.payload/'bin/boot-hook').resolve())
        self.assertEqual((self.base/'webui/access.key').stat().st_mode & 0o777,0o600)

    def test_preserves_profile_and_key_during_upgrade(self):
        self.assertEqual(self.run_it('install').returncode,0)
        key=self.base/'webui/access.key'
        original=key.read_bytes()
        profile=self.base/'webui/secrets/user.txt'
        profile.write_text('PRIVATE_TEST_SECRET')
        cfg=self.base/'config/client.json'
        cfg.write_text('{"saved":true}')
        (self.payload/'bin/engine').write_text('#!/bin/sh\necho newer\n')
        self.regenerate_sums()
        res=self.run_it('install')
        self.assertEqual(res.returncode,0,res.stderr+res.stdout)
        self.assertEqual(original,key.read_bytes())
        self.assertEqual(profile.read_text(),'PRIVATE_TEST_SECRET')
        self.assertEqual(cfg.read_text(),'{"saved":true}')

    def test_rejects_tampered_payload(self):
        (self.payload/'bin/panel').write_text('changed')
        res=self.run_it('install')
        self.assertNotEqual(res.returncode,0)
        self.assertIn('REFUSED=payload_checksum_invalid',res.stderr)
        self.assertFalse(self.base.exists())

    def test_refuses_upgrade_with_vpn_active(self):
        self.assertEqual(self.run_it('install').returncode,0)
        previous=(self.base/'bin/cvpn.sh').read_bytes()
        (self.base/'state/active').write_text('active')
        (self.payload/'bin/engine').write_text('#!/bin/sh\necho newer\n')
        self.regenerate_sums()
        res=self.run_it('install')
        self.assertNotEqual(res.returncode,0)
        self.assertIn('REFUSED=vpn_running_disconnect_first',res.stderr)
        self.assertEqual(previous,(self.base/'bin/cvpn.sh').read_bytes())

    def test_partial_copy_failure_rolls_back(self):
        self.assertEqual(self.run_it('install').returncode,0)
        original=(self.base/'bin/cvpn.sh').read_bytes()
        (self.payload/'bin/engine').write_text('#!/bin/sh\necho new-engine\n')
        self.regenerate_sums()
        cp=self.fakebin/'cp'
        cp.write_text('#!/bin/sh\ncase "$2" in *cvpn.sh.next.*) exit 23 ;; esac\nexec /bin/cp "$@"\n')
        cp.chmod(0o755)
        res=self.run_it('install')
        self.assertNotEqual(res.returncode,0)
        self.assertIn('ROLLBACK_ATTEMPTED=YES',res.stderr)
        self.assertEqual((self.base/'bin/cvpn.sh').read_bytes(),original)
        self.assertFalse((self.base/'state/lock').exists())

    def test_engine_lock_pid_is_numeric(self):
        # Engine requires a numeric shell PID (not a literal dollar sign).
        self.assertIn("printf '%s\\n' \"$$\" > \"$BASE/state/lock/pid\"", SOURCE)
        self.assertNotIn("printf '%s\\n' \"$\" > \"$BASE/state/lock/pid\"", SOURCE)

    def test_status_reports_in_progress_without_starting_second_install(self):
        self.assertEqual(self.run_it('install').returncode,0)
        (self.payload/'bin/engine').write_text('#!/bin/sh\\necho changed\\n')
        self.regenerate_sums()
        lock=self.base/'state/lock'
        lock.mkdir()
        (lock/'pid').write_text(str(os.getpid()))
        res=self.run_it('status')
        self.assertEqual(res.returncode,0,res.stderr)
        self.assertIn('BUNDLE_READY=NO',res.stdout)
        self.assertIn('INSTALL_IN_PROGRESS=YES',res.stdout)

    def test_safe_stale_lock_recovery_before_install(self):
        self.assertEqual(self.run_it('install').returncode,0)
        old=(self.base/'bin/cvpn.sh').read_text()
        (self.payload/'bin/engine').write_text('#!/bin/sh\\necho changed\\n')
        self.regenerate_sums()
        lock=self.base/'state/lock'
        lock.mkdir()
        (lock/'pid').write_text('99999999')
        res=self.run_it('status')
        self.assertIn('STALE_LOCK_RECOVERABLE=YES',res.stdout)
        self.assertNotIn('INSTALL_IN_PROGRESS=YES',res.stdout)
        res=self.run_it('install')
        self.assertEqual(res.returncode,0,res.stderr+res.stdout)
        self.assertNotEqual((self.base/'bin/cvpn.sh').read_text(),old)
        self.assertFalse(lock.exists())

    def test_refuse_to_remove_live_lock(self):
        self.assertEqual(self.run_it('install').returncode,0)
        (self.payload/'bin/engine').write_text('#!/bin/sh\\necho changed\\n')
        self.regenerate_sums()
        lock=self.base/'state/lock'
        lock.mkdir()
        (lock/'pid').write_text(str(os.getpid()))
        res=self.run_it('install')
        self.assertNotEqual(res.returncode,0)
        self.assertIn('REFUSED=engine_busy',res.stderr)
        self.assertEqual((lock/'pid').read_text(),str(os.getpid()))

    def test_backup_retention_is_bounded_after_upgrades(self):
        self.assertEqual(self.run_it('install').returncode,0)
        for n in range(4):
            (self.payload/'bin/engine').write_text('#!/bin/sh\\necho ver%d\\n'%n)
            self.regenerate_sums()
            res=self.run_it('install')
            self.assertEqual(res.returncode,0,res.stderr+res.stdout)
            backups=list((self.base/'webui/state').glob('backup-allinone-*'))
            self.assertLessEqual(len(backups),2)
        self.assertEqual((self.base/'config').is_dir(),True)

    def test_orphan_xray_refuses_binary_replacement(self):
        self.assertEqual(self.run_it('install').returncode,0)
        (self.payload/'bin/engine').write_text('#!/bin/sh\\necho changed\\n')
        self.regenerate_sums()
        pidof=self.fakebin/'pidof'
        pidof.write_text(chr(10).join(['#!/bin/sh', '[ "$1" = xray ] && echo 4242', '']))
        pidof.chmod(0o755)
        res=self.run_it('install')
        self.assertNotEqual(res.returncode,0)
        self.assertIn('REFUSED=vpn_xray_present',res.stderr)

    def test_root_required(self):
        (self.fakebin/'id').write_text('#!/bin/sh\necho 1000\n')
        res=self.run_it('install')
        self.assertNotEqual(res.returncode,0)
        self.assertIn('REFUSED=root_required',res.stderr)
        self.assertFalse(self.base.exists())

if __name__=='__main__':unittest.main()
