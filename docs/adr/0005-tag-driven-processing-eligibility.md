# Tag-driven processing eligibility for targets

The Processing Gate decides which **target** files Dubarr acts on. It is a user-configured tag filter, not a coupling to any third-party tool: the operator chooses which filename tags mark a file as ready for processing. Tdarr (or any other tool) is only one possible producer of such tags, and it appears here solely as an example marker format.

**Criteria.** Required/excluded filename tags with ANY / ALL / REGEX match mode, case-sensitivity and the enforce toggle (`settings.target.tag_filter`), plus the optional arr criterion of ADR-0009 (arr tags and root folders, AND-ed across sources, failing closed when the arr criterion is active and no row exists).

**Targets are gated; donors are never tag-gated.** Donor ingestion follows the Export Policy and Vault rules of ADR-0002 — every file dropped into `WATCH_DIR` or imported through the GUI is processed as a donor (probe, arr identification with automatic then manual fallback, approval queue, export-policy extraction into the Audio Vault), regardless of its filename tags. Nothing in this gate may be read as donor gating.

A target without the configured tags is left completely alone: no sync, no Studio preview, no mux — even when matching donor tracks are already available in the Vault. Coverage spans **all** jobs that consume the target, so an ineligible target is rejected at sync-job creation, at mux re-check and in Studio preview alike.

An ineligible target reports `waiting_for_tags`. The only bypass is the existing force override (`security.options.allow_force_tag_override`), and it concerns **targets only** — it can never admit a donor.

An empty required-tag list is neutral (ADR-0012): nothing is blocked until the operator configures a criterion.

We accept that eligibility depends on metadata another tool writes, rather than owning that processing step. Filename tags keep the gate dependency-free and testable, and they let any external pipeline drive eligibility by renaming, without Dubarr importing another product's API.

Status: accepted

Supersedes: ADR-0005's original title and framing ("Tdarr eligibility via filename tags, not a Tdarr API", "the marker must be something Tdarr actually writes"). Amended by ADR-0009.
