import pytest


@pytest.mark.parametrize("command", [
    "sleep 60",
    "make build && sleep 45",
    "while true; do check; sleep 5; done",
    "until test -f x; do sleep 1; done",
    "sleep 2m",
])
def test_sleep_polls_are_refused(waiting, command):
    assert waiting.refusal("terminal", {"command": command}) == waiting.REFUSAL


@pytest.mark.parametrize("args", [
    {"command": "sleep 5"},
    {"command": "echo 'sleep 600'"},
    {"command": "sleep 600 &"},
    {"command": "while true; do sleep 5; done", "background": True},
    {"command": "ls"},
])
def test_other_commands_pass(waiting, args):
    assert waiting.refusal("terminal", args) is None


def test_other_tools_pass(waiting):
    assert waiting.refusal("read_file", {"command": "sleep 600"}) is None


def test_guidance_is_detected(waiting):
    assert waiting.has_guidance([{"role": "user", "content": waiting.GUIDANCE}])
    assert not waiting.has_guidance([{"role": "user", "content": "hello"}])
