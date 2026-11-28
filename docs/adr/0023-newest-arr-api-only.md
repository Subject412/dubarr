# Speak the newest *arr API; never probe for a version

Dubarr talks to two products whose HTTP APIs move on their own schedule. Sonarr's episode list has
never embedded the downloaded file unless the caller asked (`includeEpisodeFile=true` in the v3
signature, `?includeSubresources=episodeFile` in v5). Dubarr never asked, so every episode came back
`hasFile: true, episodeFile: null` — a *legal* answer meaning "you did not request the file". Dubarr
read it as "no file" for all 12 400 episodes in the library: no errors, no warnings, every series
`missing_target`, while the 486 movies — whose list embeds `movieFile` unconditionally — were fine.

The lesson is not "always check the version". It is that a client which never asks for what it needs
cannot tell an omission from an absence: `episodeFile: null` is a valid answer to the question
dubarr actually asked, so nothing in the response and nothing in a version probe would have flagged
it.

## The decision

Dubarr pins one API base per product: **Sonarr v5, Radarr v3** — the newest each actually ships.
There is no capability probe, no version detection cached at startup, and no fallback to an older
base when the first request fails.

A 404 on a route that always exists is therefore a *configuration* answer, not a data answer, and is
reported as one: `sonarr: this instance does not serve /api/v5 (404) — dubarr 0.6.0 requires
Sonarr 5`. Per-item 404s (a movie or series that is gone) keep their ordinary meaning and are never
dressed up as version trouble.

## Why not probe

A probe doubles the contracts dubarr must maintain and hides the failure mode that matters. If the
client asks "which versions do you support?", the honest answer is "both", and the response that
misled us is legal in the older contract. Probing also makes the behaviour non-deterministic: the
same build would act differently against two instances, which is exactly the difference that makes a
support report unreadable.

## Consequences

- Upgrading dubarr can require upgrading Sonarr. That is stated in the release notes rather than
  absorbed silently: a wrong-version instance fails its connection test with the message above.
- The `arrmock` fakes serve exactly one base per product and answer 404 on the other, so a client
  regression — forgetting the sub-resource, drifting back to a `/v3` path — fails a test instead of
  passing quietly. The fakes are deliberately stricter than a real Sonarr 5, which still answers v3;
  a fake that answered both would reproduce the bug it exists to catch.
- The pinned bases live in one place (`internal/arr/client.go`) and the release that pinned them is
  named in the error text. Bumping either is a deliberate commit, not an environmental accident.
- Radarr's newest is still v3, so the policy is "newest available", not "v5 everywhere". When Radarr
  ships a newer API, this ADR says what to do: move the base and the tests together.
