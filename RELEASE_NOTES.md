Had to force an extra version due to bad tests.

## Bug Fixes

- **Which client's copy of a stream is downloaded now follows yt-dlp.** When several clients list the same stream, Moombox keeps one copy, and on a signed-in session it preferred the TV client's — on a misreading of yt-dlp's client priority, which is not its format preference. yt-dlp keeps the first requested client's copy, and its signed-in order is web_embedded, then TV, then web. Moombox now does the same. Nothing was ever downloaded by the old order, because the TV client returned no formats for signed-in sessions until 2.9.3 restored it; without this change, 2.9.3 would have moved every signed-in recording onto URLs from an identity yt-dlp ranks second and calls a workaround. Live broadcasts are unaffected either way: they are rarely embeddable, so the TV copy wins there for yt-dlp and Moombox alike.

## Internal

- The rolling-window throughput test's wall-clock budget no longer measures runner speed: a loaded Windows runner caught up with the correct single head probe in 4.16 s against a 4 s budget that could not be raised without crossing the batched shape's 4.5 s floor. The probe delay is 2 s now, which puts the budget 8 s under that floor.
