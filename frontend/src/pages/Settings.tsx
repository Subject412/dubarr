import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useSearchParams } from 'react-router-dom'
import type {
  ArrOptions,
  AudioOptions,
  ImportsOptions,
  LanguagesPolicy,
  MuxOptions,
  SecurityOptions,
  SettingsKey,
  StoragePaths,
  SyncDefaults,
  SyncStrategy,
  TargetTagFilter,
  WatchOptions,
} from '../lib/api'
import { api } from '../lib/api'
import { DEFAULT_SETTINGS as DEFAULTS } from '../lib/defaults'
import { useSaveSettings, useSettings } from '../lib/hooks'
import { settingsHelp } from '../lib/settings-help'
import {
  isSettingsSectionKey,
  SETTINGS_SECTIONS,
  settingsSection,
  settingsSectionId,
  settingsSectionKeyFromId,
  type SettingsSectionKey,
} from '../lib/settings-sections'
import { ArrConnectionsSection } from '../components/ArrConnections'
import { PathField, PathListEditor } from '../components/PathPicker'
import { StrategyEditor } from '../components/StrategyEditor'
import { ThemeSelector } from '../components/ThemeSelector'
import { TranscodeRulesEditor } from '../components/TranscodeRulesEditor'
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

/** Friendly labels for the reserved Reference Language tokens (GLOSSARY). */
const REFERENCE_TOKEN_PRESETS = [
  { value: 'org', label: 'org — original language from *arr' },
  { value: 'all', label: 'all — any shared language' },
]

/**
 * The tag sources the card can display. Only `filename` is applied: nothing
 * feeds the Processing Gate arr metadata yet (`gate.ArrMetadata` is built in
 * tests only), so `arr` and `both` are shown as stored-but-inactive instead of
 * being silently rewritten to `filename`.
 */
const TAG_SOURCE_LABELS: Record<TargetTagFilter['source_mode'], string> = {
  filename: 'Filename tags',
  arr: 'Radarr/Sonarr tags',
  both: 'Filename and Radarr/Sonarr tags',
}

/** Read-only truth for a filter stored with an arr source mode. */
function StoredArrSourceNote({ filter }: { filter: TargetTagFilter }) {
  const mode = filter.source_mode ?? 'filename'
  const criteria = (
    [
      ['Required *arr tags', filter.required_arr_tags ?? []],
      ['Excluded *arr tags', filter.excluded_arr_tags ?? []],
      ['Root folders', filter.root_folder_paths ?? []],
    ] as Array<[string, string[]]>
  ).filter(([, values]) => values.length > 0)

  return (
    <div
      role="note"
      aria-label="Stored tag source"
      className="space-y-1.5 rounded-lg border border-ink-700 bg-ink-800/50 px-3 py-2 text-sm"
    >
      <p className="font-medium text-zinc-200">
        Stored tag source: {TAG_SOURCE_LABELS[mode]}
      </p>
      <p className="text-zinc-400">
        Radarr/Sonarr criteria are not applied yet — only tags in the Target filename decide which
        files are processed. The stored *arr values below are kept unchanged and saved back with
        this section.
      </p>
      {criteria.length > 0 && (
        <dl className="space-y-0.5 text-xs text-zinc-500">
          {criteria.map(([label, values]) => (
            <div key={label} className="flex gap-1">
              <dt>{label}:</dt>
              <dd>{values.join(', ')}</dd>
            </div>
          ))}
        </dl>
      )}
    </div>
  )
}

/**
 * One source mode at a time. `filename` is the only mode that can be selected;
 * a stored `arr`/`both` stays visible (and unchangeable) next to a note saying
 * its criteria are not applied — never silently dropped from the UI.
 */
function TagSourceFields({ filter }: { filter: TargetTagFilter }) {
  const mode = filter.source_mode ?? 'filename'
  const arrStored = mode !== 'filename'

  return (
    <>
      <Field
        label="Tag source"
        htmlFor="tag-source-mode"
        hint={
          arrStored
            ? 'Stored but not applied: only filename tags filter files today.'
            : 'Only tags in the Target filename are matched.'
        }
        help={settingsHelp('target.tag_filter', 'source_mode')}
      >
        <Select id="tag-source-mode" value={mode} disabled>
          <option value={mode}>
            {TAG_SOURCE_LABELS[mode]}
            {arrStored ? ' (stored, not applied)' : ''}
          </option>
        </Select>
      </Field>
      {arrStored && <StoredArrSourceNote filter={filter} />}
    </>
  )
}

function Section<T extends object>({
  sectionKey,
  value,
  save,
  onSaved,
  errorInBody = false,
  children,
}: {
  sectionKey: SettingsKey
  value: T
  save: (key: SettingsKey, value: T) => Promise<unknown>
  /** Runs after a successful save with the saved and pre-save values. */
  onSaved?: (saved: T, previous: T) => void
  /**
   * Hand the save error to the card body (a third `children` argument) instead
   * of rendering the generic alert below the form, for editors that place the
   * server's message beside the control it rejected.
   */
  errorInBody?: boolean
  children: (draft: T, update: (patch: Partial<T>) => void, error: string) => ReactNode
}) {
  const [draft, setDraft] = useState<T>(value)
  const { title, description } = settingsSection(sectionKey)
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
      id={settingsSectionId(sectionKey)}
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
        {children(draft, update, status === 'error' ? error : '')}
        {status === 'error' && !errorInBody && <Alert>{error}</Alert>}
      </div>
    </Card>
  )
}

function NumberField({
  label,
  value,
  onChange,
  hint,
  help,
  min,
  max,
  boundsMessage,
}: {
  label: string
  value: number
  onChange: (next: number) => void
  hint?: string
  help?: string
  /** Inclusive bounds. Set one to make the field refuse an out-of-range entry. */
  min?: number
  max?: number
  /** Shown when an entry is outside min..max; the draft keeps its last valid value. */
  boundsMessage?: string
}) {
  const bounded = min !== undefined || max !== undefined
  const low = min ?? Number.NEGATIVE_INFINITY
  const high = max ?? Number.POSITIVE_INFINITY
  // A bounded field keeps the operator's keystrokes locally so a partly typed
  // or refused value stays visible (clearing the box must not snap back to the
  // previous number); only a value inside the bounds reaches the draft.
  const [text, setText] = useState(String(value))
  const [error, setError] = useState('')

  // oxlint-disable react/set-state-in-effect
  useEffect(() => {
    if (bounded) setText(String(value))
  }, [bounded, value])
  // oxlint-enable react/set-state-in-effect

  const commit = (raw: string) => {
    setText(raw)
    if (raw.trim() === '') {
      setError('')
      return
    }
    const next = Number.parseInt(raw, 10)
    if (Number.isNaN(next)) {
      setError('')
      return
    }
    if (next < low || next > high) {
      setError(boundsMessage ?? `${label} is out of range`)
      return
    }
    setError('')
    onChange(next)
  }

  return (
    <Field
      label={label}
      hint={hint}
      help={help}
      htmlFor={undefined}
      error={bounded ? error : undefined}
    >
      <Input
        type="number"
        aria-label={label}
        min={min}
        max={max}
        value={bounded ? text : Number.isFinite(value) ? value : 0}
        onChange={(event) =>
          bounded
            ? commit(event.target.value)
            : onChange(Number.parseInt(event.target.value, 10) || 0)
        }
      />
    </Field>
  )
}

/**
 * Seeds the analysis mode a `sync.defaults` row written before U-WIN.6 does not
 * carry, and repairs an empty one. 'windows' is the compiled default
 * (settings.DefaultAnalysisSource), mirrored by DEFAULTS['sync.defaults'] and
 * the select's own fallback. Anything that is not a real mode renders as the
 * default, so the control is never blank — the finding-4 symptom.
 */
function withAnalysisSource(section: SyncDefaults): SyncDefaults {
  const mode = section.analysis_source
  return { ...section, analysis_source: mode === 'windows' || mode === 'full' ? mode : 'windows' }
}

export function SettingsPage() {
  const settings = useSettings()
  const saveSettings = useSaveSettings()
  const [exportError, setExportError] = useState('')
  const [exporting, setExporting] = useState(false)
  const [watchRestartNotice, setWatchRestartNotice] = useState(false)

  const flagWatchRestart = () => setWatchRestartNotice(true)

  const data = settings.data?.settings

  // A preset press in the Matching techniques card also rewrites the checkpoint
  // positions, and those belong to `sync.defaults` (D4a: Fast measures fewer
  // points). The editor cannot reach another card's draft, so the positions it
  // chose are held here and handed to the Sync defaults card through its value —
  // the card shows them, and its own Save persists them. The override is
  // dropped once that card saves, so the page falls back to what the server
  // stored and a preset never writes a section the operator did not save.
  const [presetPositions, setPresetPositions] = useState<number[] | null>(null)

  // The analysis mode is a `sync.defaults` field, and a row stored before
  // U-WIN.6 carries no analysis_source. The card is seeded with the compiled
  // default (the same 'windows' the select falls back to, and the same value as
  // DEFAULTS['sync.defaults'] / settings.DefaultAnalysisSource) so the control
  // is never blank and an untouched save sends the mode it displays instead of
  // leaving the key out of the section. The memo keeps the object identity
  // stable: Section re-seeds its draft whenever the value prop changes.
  const syncDefaults = useMemo(() => {
    const base = withAnalysisSource(
      (data?.['sync.defaults'] as SyncDefaults | undefined) ?? DEFAULTS['sync.defaults'],
    )
    return presetPositions ? { ...base, checkpoint_positions_pct: presetPositions } : base
  }, [data, presetPositions])

  // The Mux output folder is a `storage.paths` setting, so its control stays in
  // the Storage card, but it only applies when the SAVED mux options write to a
  // separate folder. The mode is read from the page data (what every card is
  // seeded from), never from the mux card's unsaved draft: switching the mode
  // hides or reveals the folder only after the Mux options card is saved.
  const savedOutputMode = (data?.['mux.options'] as MuxOptions | undefined)?.output_mode ?? 'replace'

  // Category navigation lives in the URL (`/settings?section=<key>`) so the
  // location is shareable and browser back/forward keeps working. Cards are
  // only scrolled, never remounted, so unsaved drafts survive a switch.
  const [searchParams, setSearchParams] = useSearchParams()
  const sectionParam = searchParams.get('section')
  const activeSection = isSettingsSectionKey(sectionParam) ? sectionParam : null
  const activeSectionRef = useRef<SettingsSectionKey | null>(activeSection)
  // URL writes made by the observer itself must not scroll the page again.
  const scrollFromObserver = useRef<SettingsSectionKey | null>(null)
  // Programmatic scrolling fires observer entries; ignore them briefly so the
  // explicit selection is not immediately overwritten.
  const ignoreObserverUntil = useRef(0)
  const sectionsReady = Boolean(data)

  useEffect(() => {
    if (!activeSection) return
    activeSectionRef.current = activeSection
    if (scrollFromObserver.current === activeSection) {
      scrollFromObserver.current = null
      return
    }
    const target = document.getElementById(settingsSectionId(activeSection))
    if (!target) return
    ignoreObserverUntil.current = Date.now() + 700
    target.scrollIntoView?.({ block: 'start' })
  }, [activeSection, sectionsReady])

  // Scrolling manually through the page keeps the submenu highlight and the
  // URL in step, without adding history entries.
  useEffect(() => {
    if (!sectionsReady || typeof IntersectionObserver === 'undefined') return
    const nodes = SETTINGS_SECTIONS.map((section) =>
      document.getElementById(settingsSectionId(section.key)),
    ).filter((node): node is HTMLElement => Boolean(node))
    if (nodes.length === 0) return

    const observer = new IntersectionObserver(
      (entries) => {
        if (Date.now() < ignoreObserverUntil.current) return
        const topmost = entries
          .filter((entry) => entry.isIntersecting)
          .sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top)[0]
        const key = topmost ? settingsSectionKeyFromId(topmost.target.id) : null
        if (!key || key === activeSectionRef.current) return
        activeSectionRef.current = key
        scrollFromObserver.current = key
        setSearchParams({ section: key }, { replace: true })
      },
      { root: document.querySelector('main'), rootMargin: '0px 0px -60% 0px' },
    )
    nodes.forEach((node) => observer.observe(node))
    return () => observer.disconnect()
  }, [sectionsReady, setSearchParams])

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
              Restart required to apply watch changes. The watch folder is read at startup; the scan
              interval, stability window and tag filter apply without a restart.
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
                    help={settingsHelp('storage.paths', 'watch_dir')}
                    value={draft.watch_dir}
                    onChange={(watch_dir) => update({ watch_dir })}
                  />
                  <PathField
                    label="Audio Vault"
                    help={settingsHelp('storage.paths', 'vault_dir')}
                    value={draft.vault_dir}
                    onChange={(vault_dir) => update({ vault_dir })}
                  />
                  <PathField
                    label="Temp folder"
                    help={settingsHelp('storage.paths', 'work_dir')}
                    value={draft.work_dir}
                    onChange={(work_dir) => update({ work_dir })}
                  />
                  <PathField
                    label="Mux output folder"
                    help={settingsHelp('storage.paths', 'output_dir')}
                    hidden={savedOutputMode === 'replace'}
                    value={draft.output_dir ?? ''}
                    onChange={(output_dir) => update({ output_dir })}
                  />
                </div>
                <PathListEditor
                  label="Media roots"
                  values={draft.media_roots ?? []}
                  onChange={(media_roots) => update({ media_roots })}
                  help={settingsHelp('storage.paths', 'media_roots')}
                />
              </>
            )}
          </Section>

          <Section<LanguagesPolicy>
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
                  help={settingsHelp('languages.policy', 'target_import_languages')}
                />
                <StringListEditor
                  label="Extra reference languages"
                  inputLabel="Add reference language"
                  values={draft.extra_reference_languages ?? []}
                  onChange={(extra_reference_languages) => update({ extra_reference_languages })}
                  placeholder="en"
                  help={settingsHelp('languages.policy', 'extra_reference_languages')}
                  presets={REFERENCE_TOKEN_PRESETS}
                />
                <Field
                  label="Export mode"
                  htmlFor="export-mode"
                  help={settingsHelp('languages.policy', 'export_mode')}
                >
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
                  help={settingsHelp('languages.policy', 'original_language_from_arr')}
                  checked={draft.original_language_from_arr}
                  onChange={(original_language_from_arr) => update({ original_language_from_arr })}
                />
                <Toggle
                  label="Prefer original language as reference"
                  help={settingsHelp('languages.policy', 'prefer_original_language')}
                  checked={draft.prefer_original_language}
                  onChange={(prefer_original_language) => update({ prefer_original_language })}
                />
                <Toggle
                  label="Exclude commentary tracks"
                  help={settingsHelp('languages.policy', 'exclude_commentary')}
                  checked={draft.exclude_commentary}
                  onChange={(exclude_commentary) => update({ exclude_commentary })}
                />
              </>
            )}
          </Section>

          <Section<TargetTagFilter>
            sectionKey="target.tag_filter"
            value={(data['target.tag_filter'] as TargetTagFilter) ?? DEFAULTS['target.tag_filter']}
            save={save}
          >
            {(draft, update) => (
              <>
                <TagSourceFields filter={draft} />
                <StringListEditor
                  label="Required filename tags"
                  inputLabel="Add required tag"
                  values={draft.required_filename_tags ?? []}
                  onChange={(required_filename_tags) => update({ required_filename_tags })}
                  placeholder="[TAG]"
                  help={settingsHelp('target.tag_filter', 'required_filename_tags')}
                />
                <StringListEditor
                  label="Excluded filename tags"
                  inputLabel="Add excluded tag"
                  values={draft.excluded_filename_tags ?? []}
                  onChange={(excluded_filename_tags) => update({ excluded_filename_tags })}
                  help={settingsHelp('target.tag_filter', 'excluded_filename_tags')}
                />
                <div className="grid gap-4 md:grid-cols-2">
                  <Field
                    label="Tag match mode"
                    htmlFor="tag-match-mode"
                    help={settingsHelp('target.tag_filter', 'tag_match_mode')}
                  >
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
                      help={settingsHelp('target.tag_filter', 'tag_case_sensitive')}
                      checked={draft.tag_case_sensitive}
                      onChange={(tag_case_sensitive) => update({ tag_case_sensitive })}
                    />
                    <Toggle
                      label="Enforce tag filter"
                      help={settingsHelp('target.tag_filter', 'enforce_tag_filter')}
                      checked={draft.enforce_tag_filter}
                      onChange={(enforce_tag_filter) => update({ enforce_tag_filter })}
                    />
                  </div>
                </div>
              </>
            )}
          </Section>

          <Section<SecurityOptions>
            sectionKey="security.options"
            value={(data['security.options'] as SecurityOptions) ?? DEFAULTS['security.options']}
            save={save}
          >
            {(draft, update) => (
              <>
                <Toggle
                  label="Allow force tag override"
                  help={settingsHelp('security.options', 'allow_force_tag_override')}
                  checked={draft.allow_force_tag_override}
                  onChange={(allow_force_tag_override) => update({ allow_force_tag_override })}
                />
              </>
            )}
          </Section>

          <Section<SyncDefaults>
            sectionKey="sync.defaults"
            value={syncDefaults}
            save={save}
            onSaved={() => setPresetPositions(null)}
          >
            {(draft, update) => (
              <>
                <div className="grid gap-4 md:grid-cols-3">
                  <NumberField
                    label="Max offset (s)"
                    value={draft.max_offset_sec}
                    onChange={(max_offset_sec) => update({ max_offset_sec })}
                    help={settingsHelp('sync.defaults', 'max_offset_sec')}
                  />
                  <NumberField
                    label="Checkpoint window (s)"
                    value={draft.checkpoint_window_sec}
                    onChange={(checkpoint_window_sec) => update({ checkpoint_window_sec })}
                    help={settingsHelp('sync.defaults', 'checkpoint_window_sec')}
                  />
                  {/* A row stored before U-WIN.6 has no analysis_source; the
                      card is seeded with the compiled default, and the select
                      falls back to it too, so a blank control (the
                      finding-4 symptom) cannot happen. */}
                  <Field
                    label="Analysis source"
                    htmlFor="sync-analysis-source"
                    help={settingsHelp('sync.defaults', 'analysis_source')}
                  >
                    <Select
                      id="sync-analysis-source"
                      value={draft.analysis_source ?? 'windows'}
                      onChange={(event) =>
                        update({
                          analysis_source: event.target.value === 'full' ? 'full' : 'windows',
                        })
                      }
                    >
                      <option value="windows">Windows</option>
                      <option value="full">Full</option>
                    </Select>
                  </Field>
                  {/* A row stored before U-DSP.8 has no parallel_checkpoints
                      key, so the control falls back to the shipped default
                      instead of rendering an empty worker count. */}
                  <NumberField
                    label="Parallel checkpoints"
                    value={draft.parallel_checkpoints ?? 1}
                    onChange={(parallel_checkpoints) => update({ parallel_checkpoints })}
                    help={settingsHelp('sync.defaults', 'parallel_checkpoints')}
                    min={1}
                    max={8}
                    boundsMessage="parallel_checkpoints must be 1..8"
                  />
                  <NumberField
                    label="Preview window (s)"
                    value={draft.preview_window_sec}
                    onChange={(preview_window_sec) => update({ preview_window_sec })}
                    help={settingsHelp('sync.defaults', 'preview_window_sec')}
                  />
                </div>
                <ListEditor
                  label="Checkpoint positions (%)"
                  values={draft.checkpoint_positions_pct ?? []}
                  onChange={(checkpoint_positions_pct) => update({ checkpoint_positions_pct })}
                  help={settingsHelp('sync.defaults', 'checkpoint_positions_pct')}
                />
                <ListEditor
                  label="Preview positions (%)"
                  values={draft.preview_positions_pct ?? []}
                  onChange={(preview_positions_pct) => update({ preview_positions_pct })}
                  help={settingsHelp('sync.defaults', 'preview_positions_pct')}
                />
              </>
            )}
          </Section>

          <Section<SyncStrategy>
            sectionKey="sync.strategy"
            value={(data['sync.strategy'] as SyncStrategy) ?? DEFAULTS['sync.strategy']}
            save={save}
            errorInBody
          >
            {(draft, update, error) => (
              <StrategyEditor
                draft={draft}
                update={update}
                error={error}
                onCheckpointPositions={setPresetPositions}
              />
            )}
          </Section>

          <Section<MuxOptions>
            sectionKey="mux.options"
            value={(data['mux.options'] as MuxOptions) ?? DEFAULTS['mux.options']}
            save={save}
          >
            {(draft, update) => (
              <>
                <Field
                  label="Output mode"
                  htmlFor="mux-output-mode"
                  help={settingsHelp('mux.options', 'output_mode')}
                >
                  <Select
                    id="mux-output-mode"
                    value={draft.output_mode ?? 'replace'}
                    onChange={(event) =>
                      update({ output_mode: event.target.value === 'separate' ? 'separate' : 'replace' })
                    }
                  >
                    <option value="replace">Replace the target in place</option>
                    <option value="separate">Write to the mux output folder</option>
                  </Select>
                </Field>
                <Field
                  label="Track title written into Matroska"
                  htmlFor="track-title"
                  help={settingsHelp('mux.options', 'track_title_in_mkv')}
                >
                  <Input
                    id="track-title"
                    value={draft.track_title_in_mkv}
                    onChange={(event) => update({ track_title_in_mkv: event.target.value })}
                  />
                </Field>
              </>
            )}
          </Section>

          <Section<AudioOptions>
            sectionKey="audio.options"
            value={(data['audio.options'] as AudioOptions) ?? DEFAULTS['audio.options']}
            save={save}
          >
            {(draft, update) => (
              <TranscodeRulesEditor value={draft} onChange={(next) => update(next)} />
            )}
          </Section>

          <Section<WatchOptions>
            sectionKey="watch.options"
            value={(data['watch.options'] as WatchOptions) ?? DEFAULTS['watch.options']}
            save={save}
          >
            {(draft, update) => (
              <div className="grid gap-4 md:grid-cols-2">
                <NumberField
                  label="Scan interval (s)"
                  value={draft.scan_interval_sec}
                  onChange={(scan_interval_sec) => update({ scan_interval_sec })}
                  help={settingsHelp('watch.options', 'scan_interval_sec')}
                />
                <NumberField
                  label="Stability check interval (s)"
                  hint="Minimum 4 seconds."
                  value={draft.stability_check_interval_sec}
                  onChange={(stability_check_interval_sec) => update({ stability_check_interval_sec })}
                  help={settingsHelp('watch.options', 'stability_check_interval_sec')}
                />
              </div>
            )}
          </Section>

          <Section<ImportsOptions>
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
                help={settingsHelp('imports.options', 'donor_patterns')}
              />
            )}
          </Section>

          <ArrConnectionsSection />

          <Section<ArrOptions>
            sectionKey="arr.options"
            value={(data['arr.options'] as ArrOptions) ?? DEFAULTS['arr.options']}
            save={save}
          >
            {(draft, update) => (
              <NumberField
                label="Poll interval (s)"
                value={draft.poll_interval_sec}
                onChange={(poll_interval_sec) => update({ poll_interval_sec })}
                help={settingsHelp('arr.options', 'poll_interval_sec')}
              />
            )}
          </Section>
        </div>
      )}
    </div>
  )
}
