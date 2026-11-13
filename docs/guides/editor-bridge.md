# Use Talaria in an editor

`talaria acp` lets an editor that speaks the Agent Client Protocol (ACP), such as VS Code with an ACP extension or Zed, talk to your server's agent. The editor starts `talaria acp` and speaks to it over standard input and output. The bridge relays it through the hub, binds each new chat to the folder open in the editor, and runs the agent's commands on this computer.

## Set up

1. Set up the computer first: [Set up and pair a computer](../start/setup-and-pairing.md). The computer must be paired.
2. In the editor's ACP settings, use `talaria acp` as the agent command. Use the full path, `~/.talaria/bin/talaria`, if the editor does not see your shell's `PATH`.
3. Open a folder and start a chat.

Under Remote-SSH, the ACP extension runs on the SSH host, so `talaria acp` runs there and the SSH host is the computer. Pair the SSH host, and make sure the hub is reachable from it.

## What you see

- **Attached files** are put into the prompt by the bridge.
- **The chat list** shows the chats bound to this computer in the editor's folder, and chats that were never bound to any computer, such as web chats, when the server lists them. It never shows chats bound to another computer.
- **A chat bound to another folder** on this computer opens with its history and a note that names the folder. A message you send gets a notice back and is not sent. Send `/move-here` in that chat to move it to the editor's folder.
- **A web or Telegram chat** that runs in the sandbox opens empty with a notice, and also needs `/move-here` before you can use it from the editor.
- **A chat bound to another computer** does not open. Use `/device` in the chat to move it.
- A dropped connection reconnects by itself, and the editor does not see the history replayed again.

Each open editor window costs one `hermes acp` process on the server.

## Troubleshooting

- The editor says the agent failed to start: run `talaria acp` in a terminal. It prints logs on standard error, and it waits for ACP messages on standard input.
- `give the hub URL`: the computer is not paired, or its config has no `hub.url`. Run `talaria setup https://hub.example.com`.
- More help: [Troubleshooting](troubleshooting.md).
