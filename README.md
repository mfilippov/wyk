# wyk

Use a YubiKey (or any smartcard) plugged into Windows for SSH and GPG inside
WSL2. wyk forwards the Windows gpg-agent into WSL over a Hyper-V socket
(AF_VSOCK). The key stays usable in Windows at the same time, PIN entry and
touch prompts happen in Windows, and no USB passthrough is involved.

- No admin rights, no UAC, no registry entries.
- No background program on Windows: WSL starts the Windows side on demand.
- One signed exe to download, which installs itself into WSL.
- Its GPG socket can sign and decrypt, but not manage your keys or the card.
- Go standard library only; signed, reproducible release builds.

## Why not usbipd-win or npiperelay?

[usbipd-win](https://github.com/dorssel/usbipd-win) attaches the YubiKey to
WSL, so Windows loses it until you detach it again, and it needs admin rights.
wyk forwards the agent instead, so Windows and WSL use the key at the same time.

The usual `socat` + [npiperelay](https://github.com/jstarks/npiperelay) setup
starts a Windows process for every connection and is wired up by hand. wyk
keeps one host process per WSL session, reaches it over vsock rather than
interop pipes, and needs no configuration beyond `install`.

## How it works

```
 WSL2 distro                                  Windows
 ───────────                                  ───────
 ssh, git ──► $XDG_RUNTIME_DIR/wyk/ssh.sock ─┐
 gpg ───────► $XDG_RUNTIME_DIR/gnupg/S.gpg-agent
              (systemd socket units)          │
                         │                    │
                     wyk-relay ══ vsock ══► wyk-host.exe ─┬─► \\.\pipe\openssh-ssh-agent
                         └─ starts via interop ─┘         └─► S.gpg-agent.extra
                                                                gpg-agent ─ scdaemon ─ YubiKey
```

1. systemd owns the unix sockets. The first `ssh` or `gpg` call starts
   `wyk-relay`.
2. `wyk-relay` reads the VM id with `wslinfo --vm-id`, starts `wyk-host.exe`
   through WSL interop on a random vsock port, and hands it a fresh random
   token on stdin.
3. `wyk-host.exe` listens on that VM only, checks the token on every
   connection, and connects it to gpg-agent's OpenSSH pipe (SSH) or its
   restricted extra socket (GPG), starting gpg-agent if needed.
4. When the relay stops or the VM shuts down, the host sees stdin close and
   exits.

## Requirements

- WSL 2.6.1 or later (for `wslinfo --vm-id`), with systemd enabled in the
  distro: `[boot] systemd=true` in `/etc/wsl.conf`.
- On Windows: GnuPG 2.3.9 or later (Gpg4win or the GnuPG installer), with SSH
  support turned on in `%APPDATA%\gnupg\gpg-agent.conf`:

  ```
  enable-ssh-support
  enable-win32-openssh-support
  ```

- In the distro: `gnupg` and `openssh-client`.

## Install

Download `wyk-<version>-<arch>.exe` for your machine (`amd64` or, on Windows
on ARM, `arm64`) from the [releases](https://github.com/mfilippov/wyk/releases)
and run it from PowerShell or cmd:

```powershell
.\wyk-v1.0.0-amd64.exe install             # into WSL's default distro
.\wyk-v1.0.0-amd64.exe install -d Debian   # or into a named one
```

It installs for the distro's default user; `-u name` picks another (an
imported distro may default to root). Run from inside WSL, it cannot tell
which distro and user it came from, so there it needs
`-d "$WSL_DISTRO_NAME" -u "$USER"`. It runs in your login shell, so set
`GNUPGHOME`, if you use one, where a login shell reads it (`~/.profile`,
`~/.bash_profile`, or `~/.zprofile` for zsh), not only in `~/.bashrc` or
`~/.zshrc`. The exe carries the Linux half,
`wyk-relay`, so both always come from the same build.

The exe is Authenticode-signed (Mikhail Filippov), and every release file has
[build provenance](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)
tying it to the commit and workflow it was built from. To check it before
running it (optional):

```powershell
gh attestation verify wyk-v1.0.0-amd64.exe --repo mfilippov/wyk --signer-workflow mfilippov/wyk/.github/workflows/release.yml --deny-self-hosted-runners
```

`install` is idempotent; run a newer release's exe to upgrade. In the distro,
it:

- copies the exe to `%LOCALAPPDATA%\wyk\<distro>\<uid>\wyk-host.exe` and
  `wyk-relay` to `~/.local/bin/`;
- installs `wyk-ssh.socket`, `wyk-gpg.socket` and `wyk-relay.service` as
  systemd user units, and a drop-in that keeps the distro's own
  `gpg-agent*.socket` units from starting, since they would take the gpg
  socket path (a mask would be simpler, but uninstall could not tell it from
  one you set yourself);
- sets `SSH_AUTH_SOCK` in `~/.profile`, `~/.bashrc` (and `~/.bash_profile` or `~/.bash_login` if present), each inside a
  `# >>> wyk >>>` block (for zsh and other shells it prints the line to add);
- imports the public keys (and ownertrust) of your Windows secret keys, so
  local gpg can sign and decrypt (skip with `-no-import`).

Open a new shell and check:

```sh
ssh-add -L
echo test | gpg --clearsign
```

`.\wyk-v1.0.0-amd64.exe uninstall` (with the same `-d` and `-u`) reverts all
of the above. Use the installed version or a newer one, but not the copy in
`%LOCALAPPDATA%`, which cannot delete itself. Imported public keys stay in the keyring.

## What GPG can do from WSL

wyk forwards only gpg-agent's *extra* socket, the restricted one gpg itself
uses for agent forwarding over SSH, and the host never opens the main one.
Signing and decryption work. Key and card management do not:
`gpg --card-status`, `--card-edit`, PIN changes or a card reset through wyk
fail with `Forbidden`, so do those on Windows. `gpg -K` prints a harmless
`problem with fast path key listing: Forbidden` for the same reason.

This limits wyk's GPG socket and whatever you hand it to, such as a
container. The SSH socket is not limited: it speaks the whole ssh-agent
protocol, so whoever has it can also add keys to gpg-agent.
Neither is a wall around WSL: through interop, code in WSL can run Windows
GnuPG directly. See [SECURITY.md](SECURITY.md).

## Security

See [SECURITY.md](SECURITY.md) for the threat model and how to report a
vulnerability.

## Troubleshooting

```sh
wyk-relay version
systemctl --user status wyk-ssh.socket wyk-gpg.socket wyk-relay.service
journalctl --user -u wyk-relay.service -b
```

Host-side messages are in the same journal, prefixed with `wyk-host:`.

If GnuPG was installed on Windows while WSL was running and the journal says
`gpgconf` is not found, run `wsl --shutdown`: Windows programs started from
WSL keep the PATH WSL started with.

## Building

```sh
scripts/build.sh v1.2.3 dist   # dist/{amd64,arm64}/{wyk-host.exe,wyk-relay}
go test ./...
```

`wyk-host.exe` embeds the `wyk-relay` next to it; a plain `go build` of
`cmd/wyk-host` gives an exe that runs but cannot install. Builds are
reproducible: the same commit and Go version give byte-identical binaries.
For a quick run in WSL without installing anything, from `dist/amd64`:

```sh
./wyk-relay -ssh /tmp/wyk-ssh.sock   # then: SSH_AUTH_SOCK=/tmp/wyk-ssh.sock ssh-add -L
```

## License

Apache License 2.0, see [LICENSE](LICENSE).
