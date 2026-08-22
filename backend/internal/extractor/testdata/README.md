# Test fixtures

## `gofile-wt.obf.js`

A copy of the script gofile serves at `https://gofile.io/js/wt.obf.js`, saved
on 2026-08-22, holding the secret `12af056dacea0b`.

It is here so the secret extraction in `gofilewt.go` can be tested against
what gofile actually produces. A fixture built here proves the scheme
round-trips through itself, which would hold just as well if the scheme were
subtly the wrong one — and this cannot be tested against the live site,
because the way to find out whether a secret is right is to sign a request
with it, and gofile blocks addresses that sign badly.

Everything the extraction has to get right is in this file and nothing else:
the shuffled base64 alphabet, the string table, the per-call-site keys, the
rotation, and all of it surrounded by the code the patterns have to pick it
out of.

It does not need updating when gofile rotates the secret. The test asserts
what *this* copy holds, so it goes on catching a decoder that breaks; whether
the live script still matches is a separate question, and the untracked live
tests are what answer it.

**This file is gofile's, not ours.** It is kept for interoperability testing
and is not covered by this repository's licence.
