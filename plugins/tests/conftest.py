"""Shared fixtures: load a plugin the way Hermes does (by file path) and give every test its own Hermes home."""
import importlib.util
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent


def load(plugin: str, filename: str, name: str):
    """Import ROOT/<plugin>/<filename> under ``name`` (once)."""
    module = sys.modules.get(name)
    if module is None:
        spec = importlib.util.spec_from_file_location(name, ROOT / plugin / filename)
        module = importlib.util.module_from_spec(spec)
        sys.modules[name] = module
        spec.loader.exec_module(module)
    return module


@pytest.fixture
def hermes_home(tmp_path, monkeypatch):
    home = tmp_path / "home"
    home.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(home))
    return home


@pytest.fixture(scope="session")
def routes():
    return load("subagent-routes", "routes.py", "hermes_subagent_routes_core")


@pytest.fixture(scope="session")
def waiting():
    return load("subagent-routes", "waiting.py", "hermes_subagent_routes_waiting")


@pytest.fixture(scope="session")
def pm():
    return load("project-memory", "core.py", "hermes_project_memory_core")
