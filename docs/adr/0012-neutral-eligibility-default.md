# Neutral eligibility default

A fresh install ships with an empty required-tag list, so the Processing Gate does not block any Target until the operator configures a criterion. The previous shipped default (`[ArcA310]`) was one deployment's marker and silently excluded every other library.

We accept one extra setup step in exchange for out-of-the-box behaviour that is neutral and does not encode a third party's tag. This supersedes the `PLAN.md` settled default; `enforce_tag_filter` stays `true`, which only has an effect once a criterion exists. Existing installations with a stored tag list are untouched.

Status: accepted
