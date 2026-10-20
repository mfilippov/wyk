# Security

## Reporting a vulnerability

Please report it privately through GitHub:
[Report a vulnerability](https://github.com/mfilippov/wyk/security/advisories/new).
Do not open a public issue.

## What wyk protects

wyk gives a WSL2 distro the use of a key that lives in Windows: a YubiKey or
another card behind gpg-agent. The secret key never leaves the card, and PIN
entry and touch prompts happen in Windows.

## Threat model

**Trusted:** your Windows account, gpg-agent, and the whole WSL VM. WSL
interop, which wyk needs to start its Windows half, lets processes in a distro
start Windows programs as you; `gpg.exe` started that way has full access to
gpg-agent, card management included. wyk cannot fence the agent off from code
running in WSL, and does not try.

**Kept out:**

| Who | How |
|---|---|
| Other Hyper-V VMs (Windows Sandbox, other guests) | The host listens on the WSL VM's id only, not on the wildcard, so the platform does not route their connections. |
| Containers in the WSL VM, which can open `AF_VSOCK` but have no interop | Every connection must present a 32-byte token, random per relay start. It exists only in the relay's memory and the host's stdin, never on disk. Comparison is constant-time. The unix sockets are `0600` in a `0700` directory. |
| Whatever wyk's GPG socket is handed to: a container it is mounted into, a session it is forwarded to | Only gpg-agent's restricted *extra* socket is forwarded, and the host does not know the main one. Signing and decryption work; key import/export, `--card-edit`, PIN changes and card resets are refused by gpg-agent. |
| Exhausting the host before the handshake | A connection must present the token within 2 s, and at most 32 may be pending; the rest are dropped, so the host's threads and handles stay bounded. |
| Argument injection into Windows processes | Interop turns arguments into a Windows command line that the program splits again, so the relay passes only values of a checked format: the VM id (a GUID) and hex key fingerprints. In the other direction, `install` never puts its values inside the script it runs in the distro; they go to `wsl.exe` as arguments, which Go quotes into one command line and `wsl.exe` splits back unchanged (checked with spaces, quotes, `$` and newlines). |

**Not protected:**

- Code running in WSL can use the forwarded agent while it runs, exactly as
  with SSH agent forwarding, and through interop it can run Windows GnuPG
  directly. Require a touch on the YubiKey so that every use of the key needs
  you, for each of the `sig`, `enc` and `aut` keys:
  `ykman openpgp keys set-touch sig on`.
- The SSH socket passes the whole ssh-agent protocol through. Whatever it is
  handed to can also add a key to gpg-agent (after a Windows prompt for a
  passphrase, which may be left empty).
- Denial of service from inside the VM. Anyone there can learn the random
  vsock port (`ss -A vsock` lists it while a request is in flight) and keep
  the 32 handshake slots busy, so your requests get dropped. They still
  cannot use the agent. The port goes to the host over stdin only to keep it
  off the command line.
- An ssh-agent client that half-closes the connection right after a request
  loses the reply: named pipes have no half-close. No known client does this.

## Where it is in the code

Everything above is enforced in a few places, all Go standard library:

| What | Where |
|---|---|
| The handshake: magic, channel, token, status | `internal/proto/proto.go` |
| Token check, handshake time and slot limits, the two channels | `handshake` and `open` in `cmd/wyk-host/main.go` |
| The only Windows agents reachable: the OpenSSH pipe and the *extra* socket | `dial` in `cmd/wyk-host/main.go`, `extraSocket` in `cmd/wyk-host/gpg.go`, `cmd/wyk-host/ssh.go` |
| Listening on the WSL VM's id only | `Listen` in `internal/hvsock/hvsock_windows.go`, called with `wslinfo --vm-id`; its `Conn` is the most delicate code here (handle lifetime under concurrent close) |
| Token and port on stdin, never on a command line | `tryHost` and `handOver` in `cmd/wyk-relay/main.go` |
| Values that reach a Windows command line | `guidPattern` (`cmd/wyk-relay/main.go`), `isFingerprint` (`cmd/wyk-relay/gnupg.go`) |
| Values that reach the distro from Windows | `runRelay` in `cmd/wyk-host/install.go` and the script it runs, `cmd/wyk-host/relay.sh` (shellchecked and tested in CI) |
| What install writes and uninstall deletes | `cmd/wyk-relay/units.go` (systemd units), `dotfiles.go` (marked blocks only), `hostfiles.go` (the two binaries; `removeHost` deletes nothing recursively) |

Tests hold these down: `cmd/wyk-host/main_test.go` for the token, channel,
timeout and slot limit (each fails if its check is removed),
`cmd/wyk-relay/relay_script_test.go` for `relay.sh`, and the tests next to
the format checks, the handshake encoding and the file edits.

## Releases

Release files are built on GitHub-hosted runners from the tagged commit, with
reproducible flags (`scripts/build.sh`). A release file is `wyk-host.exe` with
`wyk-relay` embedded, so one signature covers both. It is Authenticode-signed
by Mikhail Filippov with a key on a YubiKey, after a person approves the exact
build; the release job checks that the signed file is that build plus the
signature. Every release file carries a build provenance attestation:

```sh
gh attestation verify wyk-v1.0.0-amd64.exe --repo mfilippov/wyk \
  --signer-workflow mfilippov/wyk/.github/workflows/release.yml --deny-self-hosted-runners
```
