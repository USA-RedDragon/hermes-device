import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "install-plugin.sh"


def run(src, dest):
    return subprocess.run(["sh", str(SCRIPT), str(src), str(dest)], capture_output=True, text=True, check=True).stdout


@pytest.mark.parametrize("plugin", ["subagent-routes", "project-memory"])
def test_install_and_idempotence(tmp_path, plugin):
    dest = tmp_path / "plugins" / plugin
    assert "installed" in run(ROOT / plugin, dest)
    assert (dest / "plugin.yaml").is_file() and (dest / "dashboard" / "plugin_api.py").is_file()
    assert not (dest / "README.md").exists()
    assert oct(dest.stat().st_mode & 0o777) == "0o755"
    assert oct((dest / "__init__.py").stat().st_mode & 0o777) == "0o644"
    inode = dest.stat().st_ino
    assert "is current" in run(ROOT / plugin, dest)
    assert dest.stat().st_ino == inode  # untouched when nothing changed
    assert not (tmp_path / "plugins" / f"{plugin}.new").exists()


def test_changed_plugin_is_replaced(tmp_path):
    src = tmp_path / "src"
    (src / "dashboard").mkdir(parents=True)
    (src / "plugin.yaml").write_text("name: x\n")
    (src / "__init__.py").write_text("A = 1\n")
    (src / "__pycache__").mkdir()
    (src / "__pycache__" / "junk.pyc").write_text("x")
    dest = tmp_path / "out" / "x"
    run(src, dest)
    assert not (dest / "__pycache__").exists()
    (src / "__init__.py").write_text("A = 2\n")
    assert "installed" in run(src, dest)
    assert (dest / "__init__.py").read_text() == "A = 2\n"
    assert not (tmp_path / "out" / "x.old").exists()


def test_refuses_a_directory_that_is_not_a_plugin(tmp_path):
    r = subprocess.run(["sh", str(SCRIPT), str(tmp_path), str(tmp_path / "d")], capture_output=True, text=True)
    assert r.returncode == 1
