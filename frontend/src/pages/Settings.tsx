import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useSearchParams } from 'react-router-dom'
import type {
  ArrOptions,
  AudioOptions,
  LanguagesPolicy,
  MuxOptions,
  SecurityOptions,
  SettingsKey,
  StoragePaths,
  SyncDefaults,
  SyncStrategy,
  TagSourceMode,
  TargetTagFilter,
  VaultNaming,
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
import {
  PRESET_LABELS,
  PRESET_NAMES,
  PRESET_POSITIONS_PCT,
  type PresetPress,
} from '../lib/strategy'
import { ArrConnectionsSection } from '../components/ArrConnections'
import { PathField, PathListEditor } from '../components/PathPicker'
import { StrategyEditor } from '../components/StrategyEditor'
import { ThemeSelector } from '../components/ThemeSelector'
import { TranscodeRulesEditor } from '../components/TranscodeRulesEditor'
import { VaultNamingSettings } from '../components/VaultNamingSettings'
import {
  Alert,
  Badge,
  Button,
  Card,
  Field,
  Input,
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
 * The three tag sources the Processing Gate reads, all of them live. The sync
 * worker hands Radarr/Sonarr metadata to every job (`Arr: gate.ArrMetadata{…}`
 * in `syncworker/worker.go`) and `gate.Rule.Classify` consults `SourceMode`:
 * `filename` matches the Target filename, `arr` matches the *arr tags and root
 * folders, `both` requires the filename tags *and* the arr criteria.
 */
const TAG_SOURCE_OPTIONS: ReadonlyArray<{ value: TagSourceMode; label: string }> = [
  { value: 'filename', label: 'Filename tags' },
  { value: 'arr', label: '*arr metadata' },
  { value: 'both', label: 'Both' },
]

/** What each source actually reads, the way the gate applies it (D11 rule). */
const TAG_SOURCE_HINTS: Record<TagSourceMode, string> = {
  filename: 'Only tags in the Target filename are matched.',
  arr: 'Only Radarr/Sonarr tags and root folders are matched. Filename tags are ignored.',
  both: 'A file must pass the filename tags and the *arr criteria.',
}

/**
 * The source selector plus the *arr criteria editors. All three sources are
 * choosable; the stored arrays are patched, never rebuilt, so switching modes
 * keeps the other mode's criteria intact.
 */
function TagSourceFields({
  filter,
  onChange,
  enforce,
}: {
  filter: TargetTagFilter
  onChange: (next: TargetTagFilter) => void
  /** A missing *arr row only blocks a file while the gate is enforced. */
  enforce: boolean
}) {
  const mode = filter.source_mode ?? 'filename'
  const arrActive = mode !== 'filename'
  const noCriteria =
    (filter.required_arr_tags ?? []).length === 0 &&
    (filter.excluded_arr_tags ?? []).length === 0 &&
    (filter.root_folder_paths ?? []).length === 0

  return (
    <>
      <Field
        label="Tag source"
        htmlFor="tag-source-mode"
        hint={TAG_SOURCE_HINTS[mode]}
        help={settingsHelp('target.tag_filter', 'source_mode')}
      >
        <Select
          id="tag-source-mode"
          value={mode}
          onChange={(event) =>
            onChange({ ...filter, source_mode: event.target.value as TagSourceMode })
          }
        >
          {TAG_SOURCE_OPTIONS.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </Select>
      </Field>
      {arrActive && (
        <>
          {/* D-TAGS: arr/both with no stored criteria is a legitimate choice,
              not a broken form — an empty list matches every title the gate has
              arr metadata for. Say so; never invent default tags. */}
          {(noCriteria || enforce) && (
            <div className="space-y-1.5 rounded-lg border border-ink-700 bg-ink-800/50 px-3 py-2 text-sm text-zinc-400">
              {noCriteria && (
                <p role="note" aria-label="Empty *arr criteria">
                  No *arr criteria are stored, so this source matches every title Dubarr has
                  matched to a Radarr/Sonarr entry. Add a required tag, an excluded tag or a root
                  folder to narrow it — Dubarr does not fill in default tags.
                </p>
              )}
              {enforce && (
                <p>
                  Files Dubarr has not matched to a Radarr/Sonarr title are rejected while
                  enforcement is on.
                </p>
              )}
            </div>
          )}
          <StringListEditor
            label="Required *arr tags"
            inputLabel="Add required *arr tag"
            values={filter.required_arr_tags ?? []}
            onChange={(required_arr_tags) => onChange({ ...filter, required_arr_tags })}
            help={settingsHelp('target.tag_filter', 'required_arr_tags')}
          />
          <StringListEditor
            label="Excluded *arr tags"
            inputLabel="Add excluded *arr tag"
            values={filter.excluded_arr_tags ?? []}
            onChange={(excluded_arr_tags) => onChange({ ...filter, excluded_arr_tags })}
            help={settingsHelp('target.tag_filter', 'excluded_arr_tags')}
          />
          <div className="space-y-1">
            <StringListEditor
              label="Root folders"
              inputLabel="Add root folder"
              values={filter.root_folder_paths ?? []}
              onChange={(root_folder_paths) => onChange({ ...filter, root_folder_paths })}
              placeholder="/media/movies"
              help={settingsHelp('target.tag_filter', 'root_folder_paths')}
            />
            {/* A plain text list: the local path picker browses this machine,
                not the Radarr/Sonarr host, so it would pick the wrong folder. */}
            <p className="text-xs text-zinc-500">
              Type the paths here — the local path picker browses this machine, not the *arr host.
            </p>
          </div>
        </>
      )}
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

/**
 * The stored checkpoint positions, read back (U09, D-CKPT shape B).
 *
 * Both percent lists used to be free-text editors here. A typed list could
 * silently disagree with the starting point shown in Matching techniques — the
 * card's draft stayed dirty across a preset press, so the label and the list
 * drifted apart and the next save wrote the typed list back over the preset.
 * The preset is now the only way to move the list, and this card reads it out:
 * how many checkpoints, where they sit, whether they are the list a preset
 * writes, and — for a stored list that matches no preset — exactly that, rather
 * than an empty card or a control the operator cannot reconcile.
 *
 * `preview_positions_pct` is deliberately not shown: nothing reads it, and the
 * removal leaves it stored and unchanged.
 */
function CheckpointPositionsSummary({ positions }: { positions: number[] }) {
  const preset = PRESET_NAMES.find((name) => {
    const candidate = PRESET_POSITIONS_PCT[name]
    return (
      candidate !== undefined &&
      candidate.length === positions.length &&
      candidate.every((value, index) => value === positions[index])
    )
  })
  const pointWord = positions.length === 1 ? 'checkpoint' : 'checkpoints'
  // Only Fast and Balanced write the positions — PRESET_POSITIONS_PCT has no
  // entry for Maximum accuracy, whose press changes the method list alone. The
  // note offers exactly those two, and says what the third does instead: naming
  // Maximum accuracy as a way to set the list sent the operator to a press that
  // left it where it was. This is the same split the strategy card's own note
  // draws, so the two cards never disagree.
  const presetsPath = 'press Fast or Balanced in Matching techniques to set them'
  const maximumAccuracyNote =
    'Maximum accuracy leaves them alone — it only changes the methods'

  if (positions.length === 0) {
    return (
      <div
        data-testid="checkpoint-positions"
        className="rounded-lg border border-ink-700 bg-ink-800/50 px-3 py-2 text-sm text-zinc-400"
      >
        <p role="note" data-testid="checkpoint-positions-custom">
          No checkpoint positions are stored — {presetsPath}; a press there writes the
          positions in one go with the methods. {maximumAccuracyNote}.
        </p>
      </div>
    )
  }

  return (
    <div
      data-testid="checkpoint-positions"
      className="space-y-1 rounded-lg border border-ink-700 bg-ink-800/50 px-3 py-2 text-sm"
    >
      <p className="text-zinc-200">
        <span data-testid="checkpoint-positions-count" className="font-medium">
          {positions.length}
        </span>{' '}
        {pointWord} measured at{' '}
        <span data-testid="checkpoint-positions-value">{positions.join(', ')} %</span>
        {preset ? ` — the ${PRESET_LABELS[preset]} positions.` : ' — a stored list.'}
      </p>
      {!preset && (
        <p role="note" data-testid="checkpoint-positions-custom" className="text-zinc-400">
          This list matches no preset, so the starting point in Matching techniques does not
          describe it. Checkpoints are measured along the film at fixed percentages and a
          starting point sets them — {presetsPath}. {maximumAccuracyNote}.
        </p>
      )}
      <p className="text-xs text-zinc-500">
        Checkpoints are measured along the film at fixed percentages — six for Balanced, three
        for Fast. The preset sets them; they are no longer edited here.
      </p>
    </div>
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

  // A preset press in the Matching techniques card also rewrites the checkpoint
  // positions, and those belong to `sync.defaults` (D4a: Fast measures fewer
  // points). The editor cannot reach another card's draft, so the press is held
  // here: it seeds the Sync defaults card through its value, and whichever of the
  // two cards saves next sends BOTH sections in one PUT. One payload is one
  // transaction on the server (settings.Store.Apply validates everything before
  // writing anything), so the stored state can never hold half a preset — the
  // `preset:"fast"` over six points this replaced. The press is dropped once it
  // is stored, or when the editor withdraws it (`onPreset(null)`).
  const [presetPress, setPresetPress] = useState<PresetPress | null>(null)

  // The analysis mode is a `sync.defaults` field, and a row stored before
  // U-WIN.6 carries no analysis_source. The card is seeded with the compiled
  // default (the same 'windows' the select falls back to, and the same value as
  // DEFAULTS['sync.defaults'] / settings.DefaultAnalysisSource) so the control
  // is never blank and an untouched save sends the mode it displays instead of
  // leaving the key out of the section. The memo keeps the object identity
  // stable: Section re-seeds its draft whenever the value prop changes.
  const syncDefaultsBase = useMemo(
    () =>
      withAnalysisSource(
        (data?.['sync.defaults'] as SyncDefaults | undefined) ?? DEFAULTS['sync.defaults'],
      ),
    [data],
  )

  const syncDefaults = useMemo(
    () =>
      presetPress?.positions
        ? { ...syncDefaultsBase, checkpoint_positions_pct: presetPress.positions }
        : syncDefaultsBase,
    [syncDefaultsBase, presetPress],
  )

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

  /**
   * The two cards a preset press can touch. With a press staged the save carries
   * the pair — `sync.strategy` from whichever card is saving, the positions from
   * the press — so one user action stores both sections or neither. The payload
   * is one PUT because the store applies it in a single transaction; two PUTs
   * would leave a window in which one half is stored.
   */
  const savePresetSection = (key: SettingsKey, value: object) => {
    if (!presetPress) return save(key, value)
    const strategy = key === 'sync.strategy' ? (value as SyncStrategy) : presetPress.strategy
    // A press without positions (Maximum accuracy) still leaves the defaults
    // section exactly as stored, so the pair stays complete either way.
    const defaults =
      key === 'sync.defaults'
        ? (value as SyncDefaults)
        : {
            ...syncDefaultsBase,
            checkpoint_positions_pct:
              presetPress.positions ?? syncDefaultsBase.checkpoint_positions_pct,
          }
    return saveSettings.mutateAsync({ 'sync.strategy': strategy, 'sync.defaults': defaults })
  }

  /** A press that leaves the positions alone is complete on its own. */
  const stagePreset = (press: PresetPress | null) =>
    setPresetPress(press?.positions ? press : null)

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

          <Section<VaultNaming>
            sectionKey="vault.naming"
            value={(data['vault.naming'] as VaultNaming) ?? DEFAULTS['vault.naming']}
            save={save}
            errorInBody
          >
            {(draft, update, error) => (
              <VaultNamingSettings value={draft} onChange={update} error={error} />
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
                <TagSourceFields
                  filter={draft}
                  onChange={(next) => update(next)}
                  enforce={draft.enforce_tag_filter}
                />
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
            save={savePresetSection}
            onSaved={() => setPresetPress(null)}
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
                {/* U09 (D-CKPT, shape B): the two percent lists are read back,
                    never edited. The stored values and their defaults are
                    untouched — a preset is the only way to move them. */}
                <CheckpointPositionsSummary positions={draft.checkpoint_positions_pct ?? []} />
              </>
            )}
          </Section>

          <Section<SyncStrategy>
            sectionKey="sync.strategy"
            value={(data['sync.strategy'] as SyncStrategy) ?? DEFAULTS['sync.strategy']}
            save={savePresetSection}
            onSaved={() => setPresetPress(null)}
            errorInBody
          >
            {(draft, update, error) => (
              <StrategyEditor
                draft={draft}
                update={update}
                error={error}
                presetPending={presetPress !== null}
                onPreset={stagePreset}
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
                {/* U10: the track title is not a setting. Nothing to configure. */}
                <p className="text-xs text-zinc-500">
                  The new track is named after its language (Polish, French …) and tagged{' '}
                  <code className="font-mono text-zinc-400">pol</code>,{' '}
                  <code className="font-mono text-zinc-400">fre</code> … in the container.
                </p>
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
