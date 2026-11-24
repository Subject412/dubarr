# Arr metadata as an additional eligibility criterion (amends ADR-0005)

A Target is eligible for sync/mux when the Processing Gate's filename criterion AND its optional arr criterion (Radarr/Sonarr tags and root folders) both pass. With `source_mode=filename` the behaviour is exactly ADR-0005. Arr tags are matched by label, root folders by canonical path, and a Target with no arr library row fails closed (`waiting_for_tags`) while the filter is enforced. Tag metadata is refreshed on the settled 300 s arr poll, so eligibility can lag an arr tag edit by up to one poll interval.

We chose AND across sources so adding an arr criterion can only narrow eligibility, never silently widen it. This extends ADR-0005 (tag-driven processing eligibility for targets) with arr metadata only; no third-party tool's API or internals are introduced as a dependency, and `gate/rule.go`'s "solely by the base filename" wording is superseded.

Status: accepted
