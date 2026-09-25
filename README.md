# AI Hub published files

Data files that installed copies of AI Hub read to keep themselves current, and the source of the phone helper. The hub's own code is not here.

- `catalogue/platforms.json`: the platform list (version 2026-09-25, 98 platforms). A newer file is downloaded by every copy and used at once.
- `catalogue/skills.json`: the published skills list (version 2026-09-18, 10 skills): public GitHub skills the Skills screen offers to install. Nothing on it is installed by itself.
- `releases/latest.json`: the newest release (0.2.2). A copy running an older version shows a banner with the download link.
- `phone/tailnet/`: the phone helper (Go, MIT licence), the program that lets a phone reach AI Hub through Tailscale embedded in the hub. `.github/workflows/tailnet.yml` builds it on every `tailnet-v*` tag and attaches the Windows binary to a release, so the binary AI Hub ships can be checked against, and signed from, this source.

The files are written by the AI Hub build and published with `npm run publish`. Nothing here is edited by hand.
