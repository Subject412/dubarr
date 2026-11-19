# Tdarr eligibility via filename tags, not a Tdarr API

Targets are eligible for sync/mux only when their filenames carry the required processed-tags (configurable list, ANY/ALL/REGEX match); donor ingestion into the Vault is never gated. We chose filename tags over Tdarr API integration to avoid coupling to Tdarr's internals, accepting that the marker must be something Tdarr actually writes. The concrete default tag list is deployment-specific and set in Settings.

Status: accepted
