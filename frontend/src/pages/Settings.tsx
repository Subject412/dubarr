import { useEffect, useRef, useState, type ReactNode } from 'react'
import type {
  ArrOptions,
  ImportsOptions,
  LanguagesPolicy,
  MuxOptions,
  SecurityOptions,
  SettingsKey,
  StoragePaths,
  SyncDefaults,
  TargetTagFilter,
  WatchOptions,
} from '../lib/api'
import { api } from '../lib/api'
import { DEFAULT_SETTINGS as DEFAULTS } from '../lib/defaults'
import { useSaveSettings, useSettings } from '../lib/hooks'
import { PathField, PathListEditor } from '../components/PathPicker'
import { ThemeSelector } from '../components/ThemeSelector'
import {
  Alert,
  Badge,
  Button,
  Card,
  Field,
  Input,
  ListEditor,
  PageHeader,
  Select,
  Spinner,
  StringListEditor,
  Toggle,
} from '../components/ui'

function Section<T extends object>({
  title,
  description,
  sectionKey,
  value,
  save,
  onSaved,
  children,
}: {
  title: string
  description: string
  sectionKey: SettingsKey
  value: T
  save: (key: SettingsKey, value: T) => Promise<unknown>
  /** Runs after a successful save with the saved and pre-save values. */
  onSaved?: (saved: T, previous: T) => void
  children: (draft: T, update: (patch: Partial<T>) => void) => ReactNode
}) {
  const [draft, setDraft] = useState<T>(value)
  const [status, setStatus] = useState<'idle' | 'saving' | 'saved' | 'error'>('idle')
  const [error, setError] = useState('')
  const [dirty, setDirty] = useState(false)
  const lastValue = useRef(value)

  // A save elsewhere replaces the whole settings envelope, handing every card
  // a fresh object identity. Adopt new server values, but never clobber a card
  // that still has unsaved edits.
  // oxlint-disable react/set-state-in-effect
  useEffect(() => {
    if (lastValue.current === value) return
    lastValue.current = value
    if (!dirty) setDraft(value)
  }, [value, dirty])
  // oxlint-enable react/set-state-in-effect

  const update = (patch: Partial<T>) => {
    setDirty(true)
    setDraft((current) => ({ ...current, ...patch }))
  }

  const onSave = async () => {
    setStatus('saving')
    setError('')
    try {
      await save(sectionKey, draft)
      setDirty(false)
      setStatus('saved')
      onSaved?.(draft, value)
    } catch (err) {
      setStatus('error')
      setError(err instanceof Error ? err.message : 'could not save section')
    }
  }

  return (
    <Card
      title={title}
      description={description}
      actions={
        <>
          {status === 'saved' && <Badge tone="success">saved</Badge>}
          <Button
            size="sm"
            variant="secondary"
            onClick={() => void onSave()}
            disabled={status === 'saving'}
            aria-label={`Save ${title}`}
          >
            {status === 'saving' ? 'Saving…' : 'Save section'}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        {children(draft, update)}
        {status === 'error' && <Alert>{error}</Alert>}
      </div>
    </Card>
  )
}

function NumberField({
  label,
  value,
  onChange,
  hint,
}: {
  label: string
  value: number
  onChange: (next: number) => void
  hint?: string
}) {
  return (
    <Field label={label} hint={hint} htmlFor={undefined}>
      <Input
        type="number"
        aria-label={label}
        value={Number.isFinite(value) ? value : 0}
        onChange={(event) => onChange(Number.parseInt(event.target.value, 10) || 0)}
      />
    </Field>
  )
}

export function SettingsPage() {
  const settings = useSettings()
  const saveSettings = useSaveSettings()
  const [exportError, setExportError] = useState('')
  const [exporting, setExporting] = useState(false)
  const [watchRestartNotice, setWatchRestartNotice] = useState(false)

  const flagWatchRestart = () => setWatchRestartNotice(true)

  const data = settings.data?.settings

  const save = (key: SettingsKey, value: object) => saveSettings.mutateAsync({ [key]: value })

  const downloadExport = async () => {
    setExporting(true)
    setExportError('')
    try {
      const payload = await api.exportSettings()
      const blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json' })
      const url = URL.createObjectURL(blob)
      const anchor = document.createElement('a')
      anchor.href = url
      const stamp = (payload.exported_at || 'export').replace(/[:]/g, '-')
      anchor.download = `dubarr-settings-${stamp}.json`
      document.body.appendChild(anchor)
      anchor.click()
      anchor.remove()
      URL.revokeObjectURL(url)
    } catch (err) {
      setExportError(err instanceof Error ? err.message : 'export failed')
    } finally {
      setExporting(false)
    }
  }

  return (
    <div>
      <PageHeader
        title="Settings"
        description="Appliance configuration. Each card saves its own section (read-modify-write, so other sections are untouched)."
        actions={
          <Button variant="secondary" size="sm" onClick={() => void downloadExport()} disabled={exporting}>
            {exporting ? 'Exporting…' : 'Export JSON'}
          </Button>
        }
      />

      <div className="mb-5">
        <ThemeSelector />
      </div>

      {settings.isLoading && <Spinner label="Loading settings" />}
      {settings.error && <Alert>Could not load settings: {settings.error.message}</Alert>}
      {exportError && <Alert>{exportError}</Alert>}
      {watchRestartNotice && (
        <Alert tone="warning">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <span>
              Restart required to apply watch changes. The watch folder, scan interval and tag
              filter are read at startup.
            </span>
            <Button size="sm" variant="ghost" onClick={() => setWatchRestartNotice(false)}>
              Dismiss
            </Button>
          </div>
        </Alert>
      )}

      {data && (
        <div className="space-y-5">
          <Section<StoragePaths>
            title="Storage paths"
            description="Watch folder, vault, working area and media roots (absolute paths)."
            sectionKey="storage.paths"
            value={(data['storage.paths'] as StoragePaths) ?? DEFAULTS['storage.paths']}
            save={save}
            onSaved={(saved, previous) => {
              if (saved.watch_dir !== previous.watch_dir) flagWatchRestart()
            }}
          >
            {(draft, update) => (
              <>
                <div className="grid gap-4 md:grid-cols-2">
                  <PathField
                    label="Watch folder"
                    value={draft.watch_dir}
                    onChange={(watch_dir) => update({ watch_dir })}
                  />
                  <PathField
                    label="Vault folder"
                    value={draft.vault_dir}
                    onChange={(vault_dir) => update({ vault_dir })}
                  />
                  <PathField
                    label="Work folder"
                    value={draft.work_dir}
                    onChange={(work_dir) => update({ work_dir })}
                  />
                </div>
                <PathListEditor
                  label="Media roots"
                  values={draft.media_roots ?? []}
                  onChange={(media_roots) => update({ media_roots })}
                  hint="Libraries Dubarr is allowed to read from."
                />
              </>
            )}
          </Section>

          <Section<LanguagesPolicy>
            title="Languages policy"
            description="Which dubbing tracks are imported and which reference tracks are kept."
            sectionKey="languages.policy"
            value={(data['languages.policy'] as LanguagesPolicy) ?? DEFAULTS['languages.policy']}
            save={save}
          >
            {(draft, update) => (
              <>
                <StringListEditor
                  label="Target import languages"
                  inputLabel="Add target language"
                  values={draft.target_import_languages ?? []}
                  onChange={(target_import_languages) => update({ target_import_languages })}
                  placeholder="pl"
                />
                <StringListEditor
                  label="Extra reference languages"
                  inputLabel="Add reference language"
                  values={draft.extra_reference_languages ?? []}
                  onChange={(extra_reference_languages) => update({ extra_reference_languages })}
                  placeholder="en"
                />
                <Field label="Export mode" htmlFor="export-mode">
                  <Select
                    id="export-mode"
                    value={draft.export_mode}
                    onChange={(event) =>
                      update({ export_mode: event.target.value === 'all' ? 'all' : 'rule_based' })
                    }
                  >
                    <option value="rule_based">Rule based</option>
                    <option value="all">All tracks</option>
                  </Select>
                </Field>
                <Toggle
                  label="Original language from *arr"
                  description="Use Radarr/Sonarr metadata to identify the original track."
                  checked={draft.original_language_from_arr}
                  onChange={(original_language_from_arr) => update({ original_language_from_arr })}
                />
                <Toggle
                  label="Exclude commentary tracks"
                  description="Commentary audio never becomes the reference or target track."
                  checked={draft.exclude_commentary}
                  onChange={(exclude_commentary) => update({ exclude_commentary })}
                />
              </>
            )}
          </Section>

          <Section<TargetTagFilter>
            title="Target tag filter"
            description="Tdarr destination filename gate (ADR-0005)."
            sectionKey="target.tag_filter"
            value={(data['target.tag_filter'] as TargetTagFilter) ?? DEFAULTS['target.tag_filter']}
            save={save}
            onSaved={(saved, previous) => {
              if (JSON.stringify(saved) !== JSON.stringify(previous)) flagWatchRestart()
            }}
          >
            {(draft, update) => (
              <>
                <StringListEditor
                  label="Required filename tags"
                  inputLabel="Add required tag"
                  values={draft.required_filename_tags ?? []}
                  onChange={(required_filename_tags) => update({ required_filename_tags })}
                  placeholder="[ArcA310]"
                />
                <StringListEditor
                  label="Excluded filename tags"
                  inputLabel="Add excluded tag"
                  values={draft.excluded_filename_tags ?? []}
                  onChange={(excluded_filename_tags) => update({ excluded_filename_tags })}
                />
                <div className="grid gap-4 md:grid-cols-2">
                  <Field label="Tag match mode" htmlFor="tag-match-mode">
                    <Select
                      id="tag-match-mode"
                      value={draft.tag_match_mode}
                      onChange={(event) =>
                        update({
                          tag_match_mode: event.target.value as TargetTagFilter['tag_match_mode'],
                        })
                      }
                    >
                      <option value="ANY">ANY — at least one tag</option>
                      <option value="ALL">ALL — every tag</option>
                      <option value="REGEX">REGEX — regular expression</option>
                    </Select>
                  </Field>
                  <div className="space-y-1">
                    <Toggle
                      label="Case sensitive"
                      checked={draft.tag_case_sensitive}
                      onChange={(tag_case_sensitive) => update({ tag_case_sensitive })}
                    />
                    <Toggle
                      label="Enforce tag filter"
                      description="Reject targets without a required tag."
                      checked={draft.enforce_tag_filter}
                      onChange={(enforce_tag_filter) => update({ enforce_tag_filter })}
                    />
                  </div>
                </div>
              </>
            )}
          </Section>

          <Section<SecurityOptions>
            title="Security options"
            description="Safety switches around the vault and AV1 hand-off."
            sectionKey="security.options"
            value={(data['security.options'] as SecurityOptions) ?? DEFAULTS['security.options']}
            save={save}
          >
            {(draft, update) => (
              <>
                <Toggle
                  label="Vault read-only"
                  description="Mounted vault is never modified by Dubarr."
                  checked={draft.vault_read_only}
                  onChange={(vault_read_only) => update({ vault_read_only })}
                />
                <Toggle
                  label="Allow force tag override"
                  description="Operators may bypass the destination tag gate per item."
                  checked={draft.allow_force_tag_override}
                  onChange={(allow_force_tag_override) => update({ allow_force_tag_override })}
                />
                <Toggle
                  label="Require AV1 warning acknowledgement"
                  checked={draft.require_av1_warning}
                  onChange={(require_av1_warning) => update({ require_av1_warning })}
                />
                <Toggle
                  label="Require AV1 strict mode"
                  checked={draft.require_av1_strict}
                  onChange={(require_av1_strict) => update({ require_av1_strict })}
                />
              </>
            )}
          </Section>

          <Section<SyncDefaults>
            title="Sync defaults"
            description="Automatic offset search bounds and sampling windows."
            sectionKey="sync.defaults"
            value={(data['sync.defaults'] as SyncDefaults) ?? DEFAULTS['sync.defaults']}
            save={save}
          >
            {(draft, update) => (
              <>
                <div className="grid gap-4 md:grid-cols-3">
                  <NumberField
                    label="Max offset (s)"
                    value={draft.max_offset_sec}
                    onChange={(max_offset_sec) => update({ max_offset_sec })}
                  />
                  <NumberField
                    label="Checkpoint window (s)"
                    value={draft.checkpoint_window_sec}
                    onChange={(checkpoint_window_sec) => update({ checkpoint_window_sec })}
                  />
                  <NumberField
                    label="Preview window (s)"
                    value={draft.preview_window_sec}
                    onChange={(preview_window_sec) => update({ preview_window_sec })}
                  />
                </div>
                <ListEditor
                  label="Checkpoint positions (%)"
                  values={draft.checkpoint_positions_pct ?? []}
                  onChange={(checkpoint_positions_pct) => update({ checkpoint_positions_pct })}
                />
                <ListEditor
                  label="Preview positions (%)"
                  values={draft.preview_positions_pct ?? []}
                  onChange={(preview_positions_pct) => update({ preview_positions_pct })}
                />
              </>
            )}
          </Section>

          <Section<MuxOptions>
            title="Mux options"
            description="Atomic muxing side effects."
            sectionKey="mux.options"
            value={(data['mux.options'] as MuxOptions) ?? DEFAULTS['mux.options']}
            save={save}
          >
            {(draft, update) => (
              <>
                <Toggle
                  label="Keep .bak backup"
                  checked={draft.keep_bak}
                  onChange={(keep_bak) => update({ keep_bak })}
                />
                <Field label="Track title written into Matroska" htmlFor="track-title">
                  <Input
                    id="track-title"
                    value={draft.track_title_in_mkv}
                    onChange={(event) => update({ track_title_in_mkv: event.target.value })}
                  />
                </Field>
              </>
            )}
          </Section>

          <Section<WatchOptions>
            title="Watch options"
            description="Donor-folder scanning cadence and stability window."
            sectionKey="watch.options"
            value={(data['watch.options'] as WatchOptions) ?? DEFAULTS['watch.options']}
            save={save}
            onSaved={(saved, previous) => {
              if (saved.scan_interval_sec !== previous.scan_interval_sec) flagWatchRestart()
            }}
          >
            {(draft, update) => (
              <div className="grid gap-4 md:grid-cols-2">
                <NumberField
                  label="Scan interval (s)"
                  value={draft.scan_interval_sec}
                  onChange={(scan_interval_sec) => update({ scan_interval_sec })}
                />
                <NumberField
                  label="Stability check interval (s)"
                  hint="Minimum 4 seconds per IDEA.md §3.5."
                  value={draft.stability_check_interval_sec}
                  onChange={(stability_check_interval_sec) => update({ stability_check_interval_sec })}
                />
              </div>
            )}
          </Section>

          <Section<ImportsOptions>
            title="Donor patterns"
            description="Sibling filenames that mark a dropped file as a donor dub. Matching is case-insensitive; a pattern without * is a substring match, with * a glob."
            sectionKey="imports.options"
            value={(data['imports.options'] as ImportsOptions) ?? DEFAULTS['imports.options']}
            save={save}
          >
            {(draft, update) => (
              <StringListEditor
                label="Donor patterns"
                inputLabel="Add donor pattern"
                values={draft.donor_patterns ?? []}
                onChange={(donor_patterns) => update({ donor_patterns })}
                placeholder="PL.DUB"
              />
            )}
          </Section>

          <Section<ArrOptions>
            title="*arr options"
            description="Radarr/Sonarr polling."
            sectionKey="arr.options"
            value={(data['arr.options'] as ArrOptions) ?? DEFAULTS['arr.options']}
            save={save}
          >
            {(draft, update) => (
              <>
                <NumberField
                  label="Poll interval (s)"
                  value={draft.poll_interval_sec}
                  onChange={(poll_interval_sec) => update({ poll_interval_sec })}
                />
                <Toggle
                  label="Manual sync enabled"
                  checked={draft.manual_sync}
                  onChange={(manual_sync) => update({ manual_sync })}
                />
              </>
            )}
          </Section>
        </div>
      )}
    </div>
  )
}
