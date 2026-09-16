import json
import tempfile
from types import SimpleNamespace
from pathlib import Path
from unittest import TestCase,mock
from warden import desktop
from warden.environments import digest

class DesktopSetupTests(TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name).resolve();self.state=self.root/'state';self.state.mkdir()
        (self.root/'config').mkdir();(self.root/'runtime/guest-assets').mkdir(parents=True)
        for name in ('Warden.app','Warden Menu.app','bin'):(self.root/'runtime'/name).mkdir()
        (self.root/'runtime/guest-assets/warden-clipboard').write_bytes(b'helper')
        self.source=self.root/'cache';(self.source/'downloads').mkdir(parents=True)
        (self.source/'downloads/image').write_bytes(b'pinned input')
        self.manifest={'version':1,'files':{'downloads/image':{'sha256':digest(self.source/'downloads/image')}}}
        (self.root/'config/offline-inputs.json').write_text(json.dumps(self.manifest))
        self.patch=mock.patch.object(desktop,'ROOT',self.root);self.patch.start();self.addCleanup(self.patch.stop)
        # These fixture pipelines create tiny marker files, not VM disks. Their
        # behavior must not depend on the developer machine's current free space.
        space=mock.patch.object(desktop.shutil,'disk_usage',return_value=SimpleNamespace(free=100*1024**3))
        space.start();self.addCleanup(space.stop)

    def test_insufficient_space_stops_before_subprocess(self):
        with mock.patch.object(desktop.shutil,'disk_usage',return_value=SimpleNamespace(free=1024)),mock.patch.object(desktop,'execute') as execute:
            with self.assertRaisesRegex(ValueError,'70 GiB'):
                desktop.setup(self.state,self.source)
            execute.assert_not_called()
    def test_initialize_uses_separate_persistent_state_and_relocatable_links(self):
        desktop.initialize(self.state);desktop.initialize(self.state)
        self.assertEqual((self.state/'Warden.app').resolve(),self.root/'runtime/Warden.app')
        self.assertFalse((self.state/'mac/disk.raw').exists())
        self.assertFalse((self.state/'admin-token').exists())
    def test_import_only_pinned_files_and_detects_corruption(self):
        (self.source/'private-key').write_text('secret')
        desktop.import_inputs(self.state,self.source)
        self.assertEqual((self.state/'downloads/image').read_bytes(),b'pinned input')
        self.assertFalse((self.state/'private-key').exists())
        desktop.verify_inputs(self.state)
        (self.source/'downloads/image').write_bytes(b'changed')
        with self.assertRaisesRegex(ValueError,'Checksum mismatch'):desktop.import_inputs(self.state,self.source)
        self.assertEqual((self.state/'downloads/image').read_bytes(),b'pinned input')
    def test_prepare_refuses_missing_inputs_before_any_subprocess(self):
        desktop.initialize(self.state)
        with mock.patch.object(desktop,'execute') as execute:
            with self.assertRaisesRegex(ValueError,'Missing or mismatched'):desktop.main(self.state,['desktop','prepare'])
            execute.assert_not_called()
    def test_existing_real_runtime_is_never_overwritten(self):
        (self.state/'Warden.app').mkdir()
        with self.assertRaisesRegex(ValueError,'different runtime'):desktop.initialize(self.state)

    def complete_step(self,state,command):
        if command=='prepare':
            (state/'proxy').mkdir(exist_ok=True)
            (state/'proxy/disk.raw').write_bytes(b'disk')
            (state/'proxy/seed.iso').write_bytes(b'seed')
        if command=='install-mac':
            (state/'mac').mkdir(exist_ok=True)
            (state/'mac/installed').write_text('installed')
            (state/'mac/tools-staged.json').write_text('{}')
        if command=='stage-tools':(state/'mac/tools-staged.json').write_text('{}')

    def test_one_click_setup_runs_in_order_and_resumes_without_reinstalling(self):
        desktop.initialize(self.state)
        with mock.patch.object(desktop,'execute',side_effect=self.complete_step) as execute:
            desktop.setup(self.state,self.source)
            self.assertEqual([call.args[1] for call in execute.call_args_list],['prepare','install-mac'])
            execute.reset_mock();desktop.setup(self.state)
            execute.assert_not_called()

    def test_prepare_only_never_installs_or_starts_vm(self):
        desktop.initialize(self.state)
        with mock.patch.object(desktop,'execute',side_effect=self.complete_step) as execute:
            desktop.setup(self.state,self.source,prepare_only=True)
            self.assertEqual([call.args[1] for call in execute.call_args_list],['prepare'])
        self.assertFalse((self.state/'mac/installed').exists())

    def test_installer_failure_stops_the_pipeline(self):
        desktop.initialize(self.state)
        with mock.patch.object(desktop,'execute',side_effect=ValueError('interrupted')) as execute:
            with self.assertRaisesRegex(ValueError,'interrupted'):desktop.setup(self.state,self.source)
            self.assertEqual([call.args[1] for call in execute.call_args_list],['prepare'])

    def test_resumes_tool_staging_without_overwriting_installed_mac(self):
        desktop.initialize(self.state);desktop.import_inputs(self.state,self.source)
        self.complete_step(self.state,'prepare');self.complete_step(self.state,'install-mac')
        (self.state/'mac/tools-staged.json').unlink()
        with mock.patch.object(desktop,'execute',side_effect=self.complete_step) as execute:
            desktop.setup(self.state)
            self.assertEqual([call.args[1] for call in execute.call_args_list],['stage-tools'])


class OfflinePreparationTests(TestCase):
    def module(self,filename):
        import importlib.util
        spec=importlib.util.spec_from_file_location('local_prepare',Path(__file__).resolve().parents[1]/'scripts'/filename)
        module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
    def test_failed_disk_preparation_never_exposes_partial_live_disk(self):
        module=self.module('prepare-proxy.py')
        with tempfile.TemporaryDirectory() as tmp:
            state=Path(tmp)
            def broken(proxy,downloads):
                (proxy/'disk.raw').write_bytes(b'partial')
                raise ValueError('injected interruption')
            with mock.patch.object(module,'STATE',state),mock.patch.object(module,'prepare',broken),mock.patch('sys.argv',['prepare']):
                with self.assertRaisesRegex(ValueError,'injected'):module.main()
            self.assertFalse((state/'proxy').exists())
            self.assertFalse(list(state.glob('.proxy-prepare-*')))
    def test_complete_disk_and_seed_become_visible_together(self):
        module=self.module('prepare-proxy.py')
        with tempfile.TemporaryDirectory() as tmp:
            state=Path(tmp)
            def complete(proxy,downloads):
                (proxy/'disk.raw').write_bytes(b'disk');(proxy/'seed.iso').write_bytes(b'seed')
            with mock.patch.object(module,'STATE',state),mock.patch.object(module,'prepare',complete),mock.patch('sys.argv',['prepare']):module.main()
            self.assertEqual((state/'proxy/seed.iso').read_bytes(),b'seed')
    def test_offline_missing_linux_input_never_invokes_downloader(self):
        module=self.module('prepare-proxy.py')
        with tempfile.TemporaryDirectory() as tmp,mock.patch.dict('os.environ',{'WARDEN_OFFLINE':'1'}),mock.patch.object(module,'run') as run:
            with self.assertRaisesRegex(SystemExit,'Offline mode'):module.fetch('https://unused.invalid',Path(tmp)/'missing')
            run.assert_not_called()
    def test_offline_missing_tool_never_invokes_npm_or_curl(self):
        module=self.module('prepare-tools.py')
        with tempfile.TemporaryDirectory() as tmp,mock.patch.dict('os.environ',{'WARDEN_OFFLINE':'1'}),mock.patch.object(module,'STATE',Path(tmp)),mock.patch.object(module,'run') as run:
            with self.assertRaisesRegex(SystemExit,'Offline mode'):module.main()
            run.assert_not_called()
