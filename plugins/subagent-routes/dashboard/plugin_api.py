"""subagent-routes routes for hermes-hub, mounted at /api/plugins/subagent-routes/ behind dashboard sign-in: the
Subagents choice of one chat and the choices the picker offers."""
from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse

router = APIRouter()
_CORE = "hermes_subagent_routes_core"


def _core():
    module = sys.modules.get(_CORE)
    if module is None:
        spec = importlib.util.spec_from_file_location(_CORE, Path(__file__).parent.parent / "routes.py")
        module = importlib.util.module_from_spec(spec)
        sys.modules[_CORE] = module
        spec.loader.exec_module(module)
    return module


def _delegation() -> dict:
    from hermes_cli.config import load_config_readonly
    delegation = load_config_readonly().get("delegation") or {}
    return delegation if isinstance(delegation, dict) else {}


def _options():
    return _core().options(_delegation())


def _answer(session_id: str) -> dict:
    core = _core()
    delegation = _delegation()
    opts = core.options(delegation)
    return {"choice": core.choice_for(session_id, opts), "choices": core.choices(opts, core.guidance(delegation))}


@router.get("/sessions/{session_id}")
def get_choice(session_id: str) -> dict:
    return _answer(session_id)


@router.put("/sessions/{session_id}")
async def put_choice(session_id: str, request: Request):
    try:
        body = await request.json()
    except Exception:
        body = None
    choice = body.get("choice") if isinstance(body, dict) else None
    core = _core()
    if not isinstance(choice, str) or not core.valid_choice(choice, _options()):
        return JSONResponse({"error": "bad_choice", "message": "That is not a Subagents choice here."}, status_code=400)
    import asyncio
    await asyncio.to_thread(core.set_choice, session_id, choice)
    return _answer(session_id)
