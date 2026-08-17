# ah4c

## The tune is the product

This proxy exists to hand Channels DVR a video stream. Everything else here —
captions, the speech models, the graphics driver, the web pages — is decoration
on top of that, and decoration never costs a recording.

**The DVR gives up after 30 seconds.** From the request arriving to the first
bytes reaching it. That is the entire budget and it is not negotiable, because
it belongs to the DVR and not to us. A tune that takes 31 seconds is a failed
recording, and a failed recording is the worst thing this program can do.

Before adding anything, answer: can this run while a tune is in flight? If the
answer is anything but a confident no, it must not run then.

### Rules that have been learned the hard way

Each of these cost a recording before it was written down.

1. **Never start uninterruptible work without proven quiet.** Not "the machine
   looks quiet right now" — a container that has just come up is quiet because
   the DVR has not asked yet, and the storm is two seconds away. Use
   `waitTuneQuietHeld`, which requires quiet to be *held*.

2. **Never override the quiet gate on a timer.** "It has waited long enough, go
   ahead anyway" is how the driver install killed a tune after the gate had
   correctly held it off for forty seconds. If quiet never comes, the work
   waits — possibly for ever. That is the correct outcome.

3. **`nice` and `ionice` are not permission to proceed.** They help; they do not
   make heavy I/O safe. An array does not care what priority the process
   writing to it has. They are a backstop for being wrong, not a license.

4. **Long work must yield throughout, not just at the start.** A gate at the
   door of a job that runs for minutes protects nothing: the world changes
   while it runs. Downloads pause per read (`tunesPending()`); model loads
   re-check; anything that cannot be paused must not be started (rule 1).

5. **Nothing on the tune path may block.** `maybeWrapCaptions` and everything it
   calls must be effectively free. Loading, probing, dlopening and downloading
   all happen on other goroutines, after the stream is already flowing.

6. **If a gate can starve something, separate the two concerns.** The engine's
   open waited on the driver restore, so an unbounded wait stopped captions
   entirely. The answer is not to bound the gate — it is to release the waiter
   and let the gated work keep waiting.

## Working here

- **American spelling everywhere.** Code, comments, commit messages, log lines,
  user-facing text. See `~/.claude/CLAUDE.md`.
- **Each speech model owns a file.** `cohere.go`, `nemotron.go`, `moonshine.go`
  hold the catalog entry and whatever that model asks of the code around it.
  `captions.go` is the common part and knows nothing about which model it is
  serving. A model is attached in exactly two places — `captionModelCatalog` and
  `quirksFor` — so deleting its file breaks three lines the compiler names.
- **Go to the model's or the engine's documentation before tuning a number.**
  Phrase lengths, quantization, KV precision, language codes and hallucination
  behavior are all documented, and every one of them has been guessed wrong
  here at least once. A number invented and given a confident justification is
  worse than an obvious placeholder.
- **A rule that decides whether to discard audio or text gets its own function
  and a test.** Two of them have been silently wrong — one because an edit never
  saved and the mistake was only visible in a commit message. Conditions buried
  in loops cannot be checked.
- **Say what was measured and what was assumed.** The log prints the real-time
  factor, the backend actually chosen, and the numbers behind anything held
  back, so a disagreement can be settled with evidence rather than opinion.

## Branches

`main` and `ui-refactor` are byte-identical apart from `Dockerfile` and
`.github/workflows/build.yml`, deliberately. Caption work is committed to `main`
and cherry-picked to `ui-refactor`; both get pushed. Do not sync those two files
and do not build images off `ui-refactor` — it is the upstream pull request.
