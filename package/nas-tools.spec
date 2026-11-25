# -*- mode: python -*-

# <<< START ADDED PART
from PyInstaller.building.build_main import Analysis, PYZ, EXE, COLLECT, BUNDLE, TOC
import babelfish
import os
from pathlib import Path

project = Path(SPECPATH).resolve().parent
helper_name = 'nas-tools-db.exe' if os.name == 'nt' else 'nas-tools-db'
web_data = [(str(file.relative_to(project)), str(file), 'DATA')
            for file in (project / 'web').rglob('*')
            if file.is_file() and '__pycache__' not in file.parts and file.suffix not in {'.py', '.pyc', '.pyo'}]
config_data = [('config/config.yaml', str(project / 'backend/internal/config/default_config.yaml'), 'DATA'),
               ('config/default-category.yaml', str(project / 'config/default-category.yaml'), 'DATA')]
script_data = [(str(file.relative_to(project)), str(file), 'DATA')
               for file in (project / 'scripts/sqls').glob('*.sql')]
# <<< START HIDDENIMPORTS PART
def collect_local_submodules(package):
    import os
    base_dir = '..'
    package_dir = os.path.join(base_dir, package.replace('.', os.sep))
    submodules = []
    for dir_path, dir_names, files in os.walk(package_dir):
        for f in files:
            if f == '__init__.py':
                continue
            if f.endswith('.py'):
                submodules.append(package + '.' + f[:-3])
    return submodules

hiddenimports = ['Crypto.Math',
                 'Crypto.Cipher',
                 'Crypto.Util',
                 'Crypto.Hash',
                 'Crypto.Protocol',
                 'babelfish.converters.alpha2',
                 'babelfish.converters.alpha3b',
                 'babelfish.converters.alpha3t',
                 'babelfish.converters.name',
                 'babelfish.converters.opensubtitles',
                 'babelfish.converters.countryname',
                 'logging.config',
                 'app.mediaserver.client',
                 'app.message.client',
                 'app.indexer.client',
                 'app.downloader.client',
                 'app.plugins.modules',
                 'app.plugins.modules._autosignin']
hiddenimports += collect_local_submodules('app.mediaserver.client')
hiddenimports += collect_local_submodules('app.message.client')
hiddenimports += collect_local_submodules('app.indexer.client')
hiddenimports += collect_local_submodules('app.downloader.client')
hiddenimports += collect_local_submodules('app.plugins.modules')
hiddenimports += collect_local_submodules('app.plugins.modules._autosignin')
# <<< END HIDDENIMPORTS PART

# <<< START DATAS
def collect_datas():
    import os
    import guessit
    datas = []
    return [
        (babelfish.__path__[0] + '/data', 'babelfish/data'),
        (guessit.__path__[0] + '/config', 'guessit/config'),
        (guessit.__path__[0] + '/data', 'guessit/data'),
    ]

datas = collect_datas()
# <<< END DATAS

block_cipher = None
a = Analysis(
    ['./../run.py'],
    pathex=[],
    binaries=[(str(project / 'backend/bin' / helper_name), '.')],
    datas=datas,
    hiddenimports=hiddenimports,
    hookspath=[str(project / 'package/rely')],
    hooksconfig={},
    runtime_hooks=[],
    excludes=[],
    win_no_prefer_redirects=False,
    win_private_assemblies=False,
    cipher=block_cipher,
    noarchive=False,
)
pyz = PYZ(a.pure, a.zipped_data, cipher=block_cipher)
a.datas += [('./nas-tools.ico', './nas-tools.ico', 'DATA')]
exe = EXE(
    pyz,
    a.scripts,
    a.binaries,
    a.zipfiles,
    a.datas,
    web_data,
    config_data,
    script_data,
    [],
    name='nas-tools',
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    upx=True,
    upx_exclude=['nas-tools-db.exe', 'nas-tools-db'],
    runtime_tmpdir=None,
    console=False,
    disable_windowed_traceback=False,
    target_arch=None,
    codesign_identity=None,
    entitlements_file=None,
    icon='nas-tools.ico'
)
