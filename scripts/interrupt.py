"""Interrupt the running turn of a stored session, as the interactive CLI's Stop does.

Run inside the Hermes runtime: interrupt.py <stored-session-id>
"""
import asyncio
import json
import sys

from hermes_cli.gateway_client import connect_gateway


async def main(stored_id: str) -> None:
    async with connect_gateway() as client:
        snap = await client.rpc("session.resume", session_id=stored_id)
        sid = snap.get("session_id") or stored_id
        gen = snap.get("execution_generation")
        params = {"session_id": sid}
        if gen is not None:
            params["execution_generation"] = gen
        print(json.dumps({"resumed_sid": sid, "running": snap.get("running"), "generation": gen}))
        print(json.dumps(await client.rpc("session.interrupt", **params)))


asyncio.run(main(sys.argv[1]))
