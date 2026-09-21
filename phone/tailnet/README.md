# AI Hub phone helper

The small program that lets a phone reach [AI Hub](https://github.com/leosalexander-hash/ai-hub-published) running on a PC. It embeds one Tailscale node with Tailscale's [tsnet](https://pkg.go.dev/tailscale.com/tsnet) library, so nothing else is installed on the PC. AI Hub starts it, it joins the user's own Tailscale network through a sign-in link, listens there with HTTPS from the network's certificate, and forwards every request to AI Hub on the loopback with a marker header, so AI Hub knows the request came from a phone and asks for the phone session. When AI Hub asks for it, the same listener is published on the public internet through Tailscale Funnel.

It talks to AI Hub with one JSON object per line on standard output and stops when its standard input closes, so it never outlives AI Hub.

## Build

Go 1.27 or later. From this folder:

```
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ai-hub-tailnet-win32-x64.exe .
```

The file name follows Node's platform and architecture names (`win32`, `darwin`, `linux`; `x64`, `arm64`), which is how AI Hub looks it up. The release workflow in `.github/workflows/tailnet.yml` builds the Windows binary on every `tailnet-v*` tag and attaches it with its SHA-256 to a GitHub release.

## Test

```
go test ./...
```

## Licence

MIT, see [LICENSE](LICENSE). Tailscale's tsnet is BSD-3-Clause.
