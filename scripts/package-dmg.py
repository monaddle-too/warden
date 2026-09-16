#!/usr/bin/env python3
"""Build a self-contained, ad-hoc signed local DMG. Never downloads inputs."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import plistlib
import re
import shutil
import subprocess
import tempfile

ROOT=Path(__file__).resolve().parents[1]
SYSTEM=('/usr/lib/','/System/Library/')

def run(*args,**kwargs):return subprocess.run([str(a) for a in args],check=True,**kwargs)
def digest(path):
    h=hashlib.sha256()
    with path.open('rb') as f:
        for block in iter(lambda:f.read(8*1024**2),b''):h.update(block)
    return h.hexdigest()
def copy_tree(source,target):
    shutil.copytree(source,target,symlinks=True,ignore=shutil.ignore_patterns('.build','__pycache__','*.pyc','site-packages','.DS_Store'))
def deps(path):
    result=run('otool','-L',path,capture_output=True,text=True).stdout
    return [line.strip().split(' (compatibility')[0] for line in result.splitlines()[1:]]
def sign(path):run('codesign','--force','--sign','-',path,stdout=subprocess.DEVNULL,stderr=None)
def relocate(source,target,libdir,seen,notices):
    source=source.resolve()
    if source in seen:return seen[source]
    target.parent.mkdir(parents=True,exist_ok=True)
    if target.exists():raise ValueError('Library basename collision: '+target.name)
    shutil.copy2(source,target);target.chmod(0o755);seen[source]=target
    cellar=source.parts
    if 'Cellar' in cellar:
        i=cellar.index('Cellar');package=Path(*cellar[:i+3]);name='-'.join(cellar[i+1:i+3]);dest=notices/name;dest.mkdir(exist_ok=True)
        for filename in ('COPYING','COPYING.LIB','LICENSE','LICENSE.txt','COPYRIGHT','AUTHORS','sbom.spdx.json'):
            if (package/filename).is_file():shutil.copy2(package/filename,dest/filename)
        licenses=package/'share/doc'/cellar[i+1]
        if licenses.is_dir():shutil.copytree(licenses,dest/'doc',dirs_exist_ok=True)
    for dependency in deps(source):
        if dependency.startswith(SYSTEM):continue
        if dependency.startswith('@loader_path/'):
            original=source.parent/dependency[len('@loader_path/'):]
        elif dependency.startswith('/'):
            original=Path(dependency)
        else:raise ValueError('Unresolved dynamic library: '+dependency)
        if original.resolve()==source:continue  # dylib's own install name
        copied=relocate(original,libdir/original.name,libdir,seen,notices)
        new='@loader_path/'+os.path.relpath(copied,target.parent)
        run('install_name_tool','-change',dependency,new,target,stderr=subprocess.PIPE)
    if target.suffix=='.dylib':run('install_name_tool','-id','@loader_path/'+target.name,target,stderr=subprocess.PIPE)
    sign(target);return target


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--python-prefix',type=Path,required=True)
    p.add_argument('--qemu-img',type=Path,default=Path('/opt/homebrew/bin/qemu-img'))
    p.add_argument('--cache',type=Path,default=ROOT/'.local')
    p.add_argument('--output',type=Path,default=ROOT/'dist/Warden-0.1.0-local-arm64.dmg')
    a=p.parse_args();a.output=a.output.resolve();a.output.parent.mkdir(parents=True,exist_ok=True)
    files={}
    images=json.loads((ROOT/'config/images.lock.json').read_text())
    for name,kind in [('downloads/ubuntu.qcow2','linux'),('downloads/macos.ipsw','macos')]:files[name]={'sha256':images[kind]['sha256']}
    artifacts=json.loads((ROOT/'config/tools.lock.json').read_text())['artifacts']
    for name,value in artifacts.items():files['guest-assets/'+name]={'sha256':value['sha256']}
    agent=a.cache/'guest-assets/agent-tools.tar.gz'
    if not agent.is_file():raise ValueError('Cached agent-tools.tar.gz required; no downloads permitted')
    files['guest-assets/agent-tools.tar.gz']={'sha256':digest(agent)}
    node_archive=a.cache/'guest-assets/node.tar.gz'
    if digest(node_archive)!=artifacts['node.tar.gz']['sha256']:raise ValueError('Cached Node archive checksum mismatch')
    with tempfile.TemporaryDirectory(prefix='dmg-build-',dir=ROOT/'.local') as tmp:
        work=Path(tmp);helpers=work/'helpers'
        run('bash',ROOT/'scripts/build-native.sh',env={**os.environ,'WARDEN_STATE':str(helpers)})
        volume=work/'volume';app=volume/'Warden.app';contents=app/'Contents';resources=contents/'Resources';macos=contents/'MacOS'
        resources.mkdir(parents=True);macos.mkdir()
        payload=resources/'payload';payload.mkdir()
        for folder in ('host','proxy','scripts','config','schemas','docs','vendor','tests'):copy_tree(ROOT/folder,payload/folder)
        shutil.copy2(ROOT/'warden',payload/'warden');shutil.copy2(ROOT/'README.md',payload/'README.md')
        (payload/'config/offline-inputs.json').write_text(json.dumps({'version':1,'files':files},indent=2)+'\n')
        helper_runtime=payload/'runtime';helper_runtime.mkdir()
        for name in ('Warden.app','Warden Menu.app','bin','guest-assets'):copy_tree(helpers/name,helper_runtime/name)
        runtime=resources/'runtime';runtime.mkdir();binpath=runtime/'bin';binpath.mkdir();notices=resources/'Third Party Notices';notices.mkdir()
        python=runtime/'python';(python/'bin').mkdir(parents=True);(python/'lib').mkdir()
        executable=next(a.python_prefix.glob('bin/python3.[0-9][0-9]'))
        version=executable.name.removeprefix('python')
        shutil.copy2(executable.resolve(),python/'bin'/executable.name)
        (python/'bin/python3').symlink_to(executable.name)
        copy_tree(a.python_prefix/'lib'/('python'+version),python/'lib'/('python'+version))
        shutil.copy2(a.python_prefix/'lib'/('python'+version)/'LICENSE.txt',notices/'Python-LICENSE.txt')
        # This cached standalone Python must not depend on its installation prefix.
        for dependency in deps(python/'bin'/executable.name):
            if not dependency.startswith(SYSTEM):raise ValueError('Python is not standalone: '+dependency)
        sign(python/'bin'/executable.name)
        with tempfile.TemporaryDirectory(dir=work) as node_tmp:
            run('tar','-xzf',node_archive,'-C',node_tmp)
            source=next(Path(node_tmp).glob('node-v*-darwin-arm64'));copy_tree(source,runtime/'node')
        shutil.copy2(runtime/'node/LICENSE',notices/'Node-LICENSE.txt')
        sign(runtime/'node/bin/node')
        for name,target in [('python3','../python/bin/python3'),('node','../node/bin/node'),('npm','../node/bin/npm'),('npx','../node/bin/npx')]:
            (binpath/name).symlink_to(target)
        seen={};relocate(a.qemu_img,binpath/'qemu-img',runtime/'lib',seen,notices)
        # The host's QEMU/dylib build may require a newer macOS than Warden itself.
        minimum=(14,0)
        for binary in [*seen.values(),python/'bin'/executable.name,runtime/'node/bin/node']:
            load=run('otool','-l',binary,capture_output=True,text=True).stdout
            versions=re.findall(r'\bminos (\d+)\.(\d+)',load)
            for value in versions:minimum=max(minimum,tuple(map(int,value)))
        plist={'CFBundleExecutable':'warden-desktop','CFBundleIdentifier':'dev.warden.desktop','CFBundleName':'Warden','CFBundleDisplayName':'Warden','CFBundlePackageType':'APPL','CFBundleShortVersionString':'0.1.0','CFBundleVersion':'1','LSMinimumSystemVersion':'.'.join(map(str,minimum)),'LSArchitecturePriority':['arm64'],'NSHighResolutionCapable':True,'CFBundleIconFile':'Warden.icns'}
        icons=work/'Warden.iconset'
        run('swift',ROOT/'scripts/make-app-icon.swift',icons)
        run('iconutil','-c','icns','-o',resources/'Warden.icns',icons)
        (contents/'Info.plist').write_bytes(plistlib.dumps(plist))
        shutil.copy2(ROOT/'native/.build/release/warden-desktop',macos/'warden-desktop')
        shutil.copy2(ROOT/'native/.build/release/warden-cli',macos/'warden')
        sign(macos/'warden')
        guide=(ROOT/'docs/local-setup.html').read_text();(resources/'Local Setup.html').write_text(guide)
        (volume/'Read Me.html').write_text(guide);(volume/'Applications').symlink_to('/Applications')
        inventory={'version':1,'local_only':True,'apple_notarized':False,'minimum_macos':plist['LSMinimumSystemVersion'],'files':{str(f.relative_to(app)):digest(f) for f in app.rglob('*') if f.is_file() and not f.is_symlink()}}
        (resources/'bundle-manifest.json').write_text(json.dumps(inventory,indent=2)+'\n')
        sign(app)
        run('codesign','--verify','--deep','--strict',app)
        # Run with an empty environment and a separate state; never touch the live VM.
        check=work/'fresh-state'
        run('/usr/bin/env','-i','WARDEN_STATE='+str(check),macos/'warden','desktop','status')
        run('/usr/bin/env','-i','PATH='+str(binpath)+':/usr/bin:/bin',binpath/'qemu-img','--version',stdout=subprocess.DEVNULL)
        run('/usr/bin/env','-i',python/'bin/python3','-c','import ssl,sqlite3,hashlib,urllib.request; print("Bundled Python standard library OK")')
        run('/usr/bin/env','-i',runtime/'node/bin/node','--version')
        # Require every internal symlink and every non-system dylib to stay in the bundle.
        for path in app.rglob('*'):
            if path.is_symlink() and not path.resolve().is_relative_to(app.resolve()):raise ValueError('External bundle symlink: '+str(path))
        staged=a.output.with_name(a.output.stem+'.new.dmg')
        staged.unlink(missing_ok=True)
        run('hdiutil','create','-quiet','-volname','Warden Local','-srcfolder',volume,'-format','UDZO',staged)
        os.replace(staged,a.output)
        metadata={'version':1,'filename':a.output.name,'bytes':a.output.stat().st_size,'sha256':digest(a.output),'apple_notarized':False,'minimum_macos':plist['LSMinimumSystemVersion'],'architecture':'arm64','local_only':True}
        a.output.with_suffix('.json').write_text(json.dumps(metadata,indent=2)+'\n')
        print('Local DMG ready:',a.output)
if __name__=='__main__':main()
