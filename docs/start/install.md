# Install

`talaria` is one program for your own computer. It lets the Talaria agent on your server run commands and use Firefox on this computer, in folders you choose. Before you install it, you need a Talaria hub: see [talaria-deploy](https://github.com/USA-RedDragon/talaria-deploy) to run your own. You also need the hub's address, for example `https://hub.example.com`, and an account on it.

## Supported systems

| System | Status |
|---|---|
| Linux, amd64, arm64, armv6, armv7, riscv64 | Supported. The background service needs a systemd user manager. |
| macOS, Intel and Apple silicon | Supported. The background service is a launchd agent. |
| Windows | Not supported. Device paths must be POSIX paths. |

Two features have more limits:

- **Chat from a terminal** (`talaria chat`) runs the same Hermes build as your server on Linux with glibc 2.28 or newer. On macOS it runs a Hermes that you installed yourself, and it must be the same release as the server's.
- **Firefox control** needs Firefox 139 or newer from your distribution or from Mozilla. The Flatpak build of Firefox cannot reach the program that the extension talks to, and the Snap build is untested and likely has the same problem, so use another build.

## Get the program

1. Open the [latest release](https://github.com/USA-RedDragon/talaria/releases/latest) and download `talaria_<version>_<os>_<arch>.tar.gz` for your system. `<os>` is `linux` or `darwin`, and `<arch>` is `amd64`, `arm64`, `armv6`, `armv7` or `riscv64`.
2. Check the download. Every release since <!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const --> has a signed build record that proves which workflow built the file. With the [GitHub CLI](https://cli.github.com):

   ```sh
   gh attestation verify talaria_<version>_linux_amd64.tar.gz --repo USA-RedDragon/talaria
   ```

   [Releases and verification](../releases.md) explains this and the SHA-256 check by hand.
3. Unpack it:

   ```sh
   tar -xzf talaria_<version>_linux_amd64.tar.gz
   ```

## Linux

Run the unpacked program once. It installs itself under `~/.talaria/bin` and does the rest:

```sh
./talaria setup https://hub.example.com
```

Setup is explained on the next page: [Set up and pair a computer](setup-and-pairing.md).

On a headless Linux machine, such as a server you log in to over SSH, the service runs while you are logged in. To keep it running after you log out and after a reboot, ask systemd to keep your user's services running:

```sh
loginctl enable-linger "$USER"
```

If the computer has no systemd user manager (Alpine, WSL without systemd, a container), setup cannot install the service and says so. Run `./talaria setup --no-service https://hub.example.com`, then start the daemon yourself with `~/.talaria/bin/talaria run`, from a process manager of your choice.

## macOS

If you downloaded the archive with a browser, macOS marks the program as untrusted. After you unpack the archive, clear the mark, then run setup:

```sh
xattr -d com.apple.quarantine ./talaria
./talaria setup https://hub.example.com
```

The service is a launchd agent, `~/Library/LaunchAgents/io.github.usa-reddragon.talaria.plist`, and its log is `~/Library/Logs/talaria.log`.

## Check that it works

```sh
talaria --help
```

If your shell says `talaria: command not found`, `~/.talaria/bin` is not on your `PATH` yet. Setup offers to add it. Open a new terminal after you say yes, or run `~/.talaria/bin/talaria` by its full path.
