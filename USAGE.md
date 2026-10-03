# Using `cull`: from the card to Capture One

This is a whole session, in order: copy the card, judge the frames, check them, then
import into Capture One. Every command has `--help` with examples. The full flag
reference is in the [README](README.md).

**Every example here is real output, recorded 2026-10-03** (the screenshots are rendered
from recordings of the live terminal). It's a 17-frame M11-P shoot: a small card made
from the repo's sample frames, judged with `--backend claude-code`. Paths are relative
to where the commands ran. Your numbers will differ. Lines are trimmed only where the
text says so.

## 0. Setup (once)

**Pre-built for macOS** (one universal binary for Apple silicon and Intel):

```sh
curl -fsSL https://github.com/jefflaplante/cull/releases/latest/download/cull_macos_universal.tar.gz | tar -xz
sudo mv cull_macos_universal/cull /usr/local/bin/
```

**From source** (Go 1.26 or newer):

```sh
make build      # bin/cull
make install    # optional: $GOPATH/bin/cull, so `cull` is on your PATH
```
 Judging needs one of these:

| Backend | How it authenticates |
|---|---|
| **API** (default) | Reads `~/.anthropic/api_key`, `$ANTHROPIC_API_KEY`, or `--api-key-file`. |
| **Subscription** (`--backend claude-code`) | Uses your Claude login through the `claude` CLI, so no key is needed. |
| **Local** (`--backend openai --model <name>`) | Any OpenAI-compatible server. It's free, but small models judge poorly. |

## 1. Copy the card (`cull offload`)

```sh
cull offload LEICA_M Pictures --name "Forest portraits" --location "Forest Park, Portland"
```

**Here, `LEICA_M` stands in for the card** (on a Mac it's `/Volumes/LEICA\ M`) and
`Pictures` for your photo library folder.

**It plans first:** the folder name and where its date came from, what it will copy, and
every refusal (a name clash, too little space) before any byte is written. Then it copies
with a live progress bar:

![offload copying, with a live progress bar](docs/images/offload-copying.png)

When the copy is done:

```
copied 17, skipped 0 (already there), failed 0: 1.1 GB in 2s (624 MB/s, verified)
all 17 files verified on Pictures/2025-12-28 Forest portraits: safe to format the card
```

- **Every file is checked twice.** The card is read once, with its SHA-256 taken during
  that read. Each copy is then dropped from memory, read back from the disk, and must
  match before it gets its real name.
- **"Safe to format"** appears only when every file verified and the drive's own write
  cache was flushed.
- **Speed:** 624 MB/s here, because this "card" is a folder on the same SSD. A real
  M11-P card over USB runs at about 140–216 MB/s; 992 frames (67.7 GB) took 8 minutes.
- **The folder date** comes from the earliest capture time. This camera's clock was
  wrong, which is why it says 2025. Use `--date 2026-10-02` to set it yourself.
- **More options:**
  - `--backup <root>` fills a second drive from the same single read;
  - `--rename "{date}_{name}_{n:4}"` numbers files across cards;
  - `--dry-run` only prints the plan.
- **Re-running is safe:** only what's missing is copied.

The plan refuses, before writing anything, when the copy won't fit:

```
$ cull offload --dry-run "/Volumes/LEICA M" ~/Pictures --name "Forest portraits"
shoot folder: 2025-12-27 Forest portraits (date from earliest capture date, M1103127.DNG)
  into /Users/jeff/Pictures/2025-12-27 Forest portraits
992 of 992 DNGs to copy, 67.7 GB (M1103127.DNG … M1104130.DNG)
error: not enough free space for /Users/jeff/Pictures: 29.0 GB free, 68.9 GB needed (67.7 GB to copy there plus a margin)
```

**After copying, offload scans the new folder** (free, no model calls):
- it reads each frame's preview and EXIF, and detects faces;
- it sets aside junk frames: near-black, blown white, or blank;
- it groups near-identical frames into sets.

![the scan after offload, with a live progress bar](docs/images/offload-scanning.png)

```
previews: long edge min/median/max = 9504/9504/9504 px; sources: tiff-ifd=17; orientation: 1=3 8=14; faces: 9/17, eyes measured on 9
next: cull judge --estimate "Pictures/2025-12-28 Forest portraits"
```

**`cull scan <dir>`** does the same for a folder you copied yourself.

## 2. See what judging would cost (free)

```
$ cull judge --estimate "Pictures/2025-12-28 Forest portraits"
estimate: 17 frames × ~6k in / ~1k out tokens ≈ 102000 in / 17000 out ≈ $0.37 at list price (claude-sonnet-5-5)
ranking ≈ 3 call(s), $0.09 at list price, if every frame lands in an 8-frame set (pairs cost more per frame; frames in no set cost nothing)
```

The ranking figure assumes full 8-frame sets, so it runs high: this shoot had two pairs.
With `--backend claude-code` nothing is billed per token; it uses your subscription
quota instead.

## 3. Judge (this spends money or quota)

```sh
cull judge --backend claude-code --write-xmp "Pictures/2025-12-28 Forest portraits"
```

**The model assesses each frame:**
- sharpness at the eyes, on a native-resolution crop;
- exposure;
- composition;
- eyes and expression;
- up to 8 content keywords.

**Go, not the model, decides keep, review or cull.** On a terminal you get a live view:
per-frame lines, a progress bar with the time left, and running totals.

![judge running: per-frame verdicts, progress and keep/review/cull tallies](docs/images/judge-running.png)

**Once every frame is judged, near-identical frames are ranked against each other:**

![judge ranking the two sets after every frame was judged](docs/images/judge-ranking.png)

The end of the run (each ranking reason is a paragraph; shortened here):

```
ranked set 1 (2 frames): M1104115.DNG wins — The two frames are almost identical … Sharpness at the eyes is the top priority, so Frame 2 wins narrowly.
ranked set 2 (2 frames): M1104116.DNG wins — The two frames are nearly identical … Frame 1 wins on expression and the more natural moment.
report: …/Pictures/2025-12-28 Forest portraits/cull-report.json
results: map[cull:1 keep:14 review:2]
tokens this run: in=153598 out=13840 (subscription, not billed per token)
```

The whole run took 1 minute 56 seconds.

**What it did here:**
- **Cull:** M1103817, missed focus (sharpness 2.5).
- **Review:**
  - M1104110: soft, with her eyes partly closed (a twirl, so she was moving);
  - M1103821: 0.50% of its raw highlights clip.
- **Keep:** everything else.

**Useful flags:**
- `--batch` (API at half price, slower);
- `--max-cost 5`;
- `--no-rank`;
- `--keep-best 1`;
- `--quota-stop 0.9` (claude-code);
- `-v` (focus target, tokens and cost per frame);
- `-q` (summary only);
- `--plain` (no live view).

**Interrupted?** Rerun with `--resume`.

**Status at any point:**

```
$ cull status "Pictures/2025-12-28 Forest portraits"
/Users/jeff/git/cull/photos/demo/Pictures/2025-12-28 Forest portraits: 17 DNGs; report cull-report.json (claude-code/sonnet → claude-sonnet-5-5, effort default)
  assessed 17 · junk 0 · errors 0 · not yet judged 0
  model: keep 14 · review 2 · cull 1 · moved out of the shoot folder (culled/ or keep/ review/ cull/) 0
  you: labelled 0/17 · rated 0 · disagree with the model 0
  sets: 2 (ranked 2, by scores 0)
  spent: not billed per token (claude-code)
next: label a sample in cull review /Users/jeff/git/cull/photos/demo/Pictures/2025-12-28 Forest portraits (0/30 so far), then cull calibrate /Users/jeff/git/cull/photos/demo/Pictures/2025-12-28 Forest portraits
```

## 4. Review in your browser

```sh
cull review "Pictures/2025-12-28 Forest portraits"
```

It opens a contact sheet. Each click or key saves straight to `cull-labels.jsonl` and
to the frame's `.xmp` sidecar. `--no-xmp` saves only the labels log; `--static` writes
an offline page instead of serving.

| key | action |
|---|---|
| ← → ↑ ↓ | move (↑/↓ go by grid row) |
| Enter / Esc | open the detail view / back to the grid |
| K / R / C / U | your verdict: keep, review, cull, clear |
| 1–5, 0 | stars, or no stars |
| Z | 100% view of the whole frame, opened on the subject; drag or arrow keys pan |
| S | compare the frame's set side by side at one scale; 1–9 keeps that frame |
| N | next unlabelled frame |
| [ / ] | previous / next set |
| - / = | one keeper fewer / more in the frame's set (top N by the model's rank keep, the rest cull) |
| , / . | exposure −/+0.1 EV (< / > for ±0.5; \ resets): previewed on the page, applied in Capture One by `apply-c1 --exposure` |

- **Sets:** each set is boxed, with its keeper count in the header. **−** / **+** keep
  the top N by rank; the **✓ keeper** toggle flips one frame.
- **Detail view:** shows the model's reasons and keywords, and the shoot's tags in the
  header. For a frame in a set it also shows the frame's rank and a filmstrip of the set.
- **Your labels always win** over the model's verdicts, for sidecars, moves and Capture
  One.

## 5. Tags: project, event, location, keywords

Tags given at offload are stored in the report, and every later run keeps them.
`cull tag` shows or changes them:

```
$ cull tag "Pictures/2025-12-28 Forest portraits"
location: Forest Park, Portland

$ cull tag --event "Fall session" --keyword family "Pictures/2025-12-28 Forest portraits"
event: Fall session
location: Forest Park, Portland
keywords: family
saved; rewrite the sidecars with: cull decide --write-xmp "Pictures/2025-12-28 Forest portraits"
```

## 6. Check the tool against your labels, and tune it (free)

```sh
cull calibrate "Pictures/2025-12-28 Forest portraits"
cull decide --review-below-sharpness 7 "Pictures/2025-12-28 Forest portraits"
```

- **`calibrate`** reports false culls (you said keep, it culled), missed culls, and the
  review rate. It also sweeps `--review-below-sharpness`, and lists any junk frame you
  labelled keep.
- **`calibrate --compare a.json b.json`** shows how often two runs over the same frames
  disagree. Verdicts can't be more trustworthy than they are stable.
- **`decide`** re-applies the policy with no model calls. The policy is saved in the
  report, and a flag you type overrides only its own setting.

## 7. Before importing: sidecars, and sorting into folders

```
$ cull decide --write-xmp --sort "Pictures/2025-12-28 Forest portraits"
decided 17 frame(s); no decision changed; sorted 17 into keep/, review/ and cull/, 0 back into the shoot folder
```

That leaves:

```
keep/: 14 DNGs, 14 sidecars
review/: 2 DNGs, 2 sidecars
cull/: 1 DNGs, 1 sidecars
```

**Import `keep/` and `review/` into Capture One** (or Lightroom, or anything else).
- **Sort before importing:** a catalog loses track of frames moved after import.
- **Undo:** `cull restore <dir>` puts every frame back where it was.
- **Culls only:** `--move-culled` moves just the culls, into `culled/`.

**Each sidecar carries:**
- your stars;
- a colour label (green keep, yellow review, red cull);
- `cull:<verdict>`;
- the content keywords and your tags, as plain words and as paths that Capture One and
  Lightroom nest.

This one is M1103865, kept:

```xml
<?xpacket begin="﻿" id="W5M0MpCehiHzreSzNTczkc9d"?>
<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="cull">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about=""
    xmlns:xmp="http://ns.adobe.com/xap/1.0/"
    xmlns:dc="http://purl.org/dc/elements/1.1/"
    xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/"
    xmlns:lr="http://ns.adobe.com/lightroom/1.0/"
    xmp:Label="Green">
   <dc:subject>
    <rdf:Bag>
     <rdf:li>cull:keep</rdf:li>
     <rdf:li>portrait</rdf:li>
     <rdf:li>young woman</rdf:li>
     <rdf:li>glasses</rdf:li>
     <rdf:li>curly hair</rdf:li>
     <rdf:li>smile</rdf:li>
     <rdf:li>firewood</rdf:li>
     <rdf:li>woodpile</rdf:li>
     <rdf:li>low key</rdf:li>
     <rdf:li>Fall session</rdf:li>
     <rdf:li>Forest Park, Portland</rdf:li>
     <rdf:li>family</rdf:li>
    </rdf:Bag>
   </dc:subject>
   <lr:hierarchicalSubject>
    <rdf:Bag>
     <rdf:li>content|portrait</rdf:li>
     <rdf:li>content|young woman</rdf:li>
     <rdf:li>content|glasses</rdf:li>
     <rdf:li>content|curly hair</rdf:li>
     <rdf:li>content|smile</rdf:li>
     <rdf:li>content|firewood</rdf:li>
     <rdf:li>content|woodpile</rdf:li>
     <rdf:li>content|low key</rdf:li>
     <rdf:li>event|Fall session</rdf:li>
     <rdf:li>location|Forest Park, Portland</rdf:li>
    </rdf:Bag>
   </lr:hierarchicalSubject>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end="w"?>
```

**Before formatting the card,** you can re-check every copy against the checksums
recorded when it was made. It finds frames that `--sort` moved too:

```
$ cull offload --verify "Pictures/2025-12-28 Forest portraits"
17 verified, 0 missing or different
```

## 8. After importing: push changes into Capture One

```sh
cull apply-c1 --probe "Pictures/2025-12-28 Forest portraits" | osascript -   # read-only check, run it first
cull apply-c1 "Pictures/2025-12-28 Forest portraits" > apply.applescript     # dry run: read the script
cull apply-c1 --run "Pictures/2025-12-28 Forest portraits"                   # apply it
```

**It updates the images already in your catalog:** colour tags, keywords (content and
tags, as plain words) and your stars.

**Edits:**
- `--exposure` sets the EV you chose in review.
- `--exposure` and `--crop` also apply the model's suggestions, but only where Capture One
  still holds the defaults: edits you made there stay.

**The dry run is a readable AppleScript.** For this shoot it was 639 lines, looking up
each distinct keyword once.

## Ranking on its own

```sh
cull rank --estimate <dir>    # exact cost, free
cull rank <dir>               # rank the sets that need it
cull rank --batch <dir>       # half price
```

`rank` uses the report's backend, model and stored policy. It skips sets whose saved
order is still valid.

## Things to know

- **Your DNGs are never modified.**
  - `offload` only reads the card, and never replaces a file.
  - `--sort` and `--move-culled` move frames within the shoot folder, recorded in the
    report and undone by `cull restore`.
  - Sidecars that cull didn't write are never overwritten unless you pass
    `--overwrite-xmp`.
- **Verdicts are only as good as calibration.** Label a sample in `review`, check
  `calibrate`, and keep `--outranked review` (the default) until you trust the ranking.
- **Junk frames** (near-black, blown white or blank) are decided without a model call:
  `--junk cull`, the default. The thresholds were measured on one photographer's
  outdoor portraits. On a night-sky or white-backdrop shoot, check `calibrate`'s junk
  line.
- **Sets:** only frames with nearly identical framing are grouped. Retakes after
  reframing usually aren't. `--seq-look` widens the match.
