# AI Hub published files

Data files that installed copies of AI Hub read to keep themselves current. No application code lives here.

- `catalogue/platforms.json`: the platform list (version 2026-09-19, 97 platforms). A newer file is downloaded by every copy and used at once.
- `catalogue/skills.json`: the published skills list (version 2026-09-18, 10 skills): public GitHub skills the Skills screen offers to install. Nothing on it is installed by itself.
- `releases/latest.json`: the newest release (0.2.0). A copy running an older version shows a banner with the download link.

The files are written by the AI Hub build and published with `npm run publish`. Nothing here is edited by hand.
