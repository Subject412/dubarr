import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useSearchParams } from 'react-router-dom'
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
import { settingsHelp } from '../lib/settings-help'
import {
  isSettingsSectionKey,
  SETTINGS_SECTIONS,
  settingsSection,
  settingsSectionId,
  settingsSectionKeyFromId,
} from '../lib/settings-sections'
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
  sectionKey,
  value,
  save,
  onSaved,
  children,
}: {
  sectionKey: SettingsKey
  value: T
  save: (key: SettingsKey, value: T) => Promise<unknown>
  /** Runs after a successful save with the saved and pre-save values. */
  onSaved?: (saved: T, previous: T) => void
  children: (draft: T, update: (patch: Partial<T>) => void) => ReactNode
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
  help,
}: {
  label: string
  value: number
  onChange: (next: number) => void
  hint?: string
  help?: string
}) {
  return (
    <Field label={label} hint={hint} help={help} htmlFor={undefined}>
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

  // Category navigation lives in the URL (`/settings?section=<key>`) so the
  // location is shareable and browser back/forward keeps working. Cards are
  // only scrolled, never remounted, so unsaved drafts survive a switch.
  const [searchParams, setSearchParams] = useSearchParams()
  const sectionParam = searchParams.get('section')
  const activeSection = isSettingsSectionKey(sectionParam) ? sectionParam : null
  const activeSectionRef = useRef<SettingsKey | null>(activeSection)
  // URL writes made by the observer itself must not scroll the page again.
  const scrollFromObserver = useRef<SettingsKey | null>(null)
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
                    label="Vault folder"
                    help={settingsHelp('storage.paths', 'vault_dir')}
                    value={draft.vault_dir}
                    onChange={(vault_dir) => update({ vault_dir })}
                  />
                  <PathField
                    label="Work folder"
                    help={settingsHelp('storage.paths', 'work_dir')}
                    value={draft.work_dir}
                    onChange={(work_dir) => update({ work_dir })}
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
                  label="Vault read-only"
                  help={settingsHelp('security.options', 'vault_read_only')}
                  checked={draft.vault_read_only}
                  onChange={(vault_read_only) => update({ vault_read_only })}
                />
                <Toggle
                  label="Allow force tag override"
                  help={settingsHelp('security.options', 'allow_force_tag_override')}
                  checked={draft.allow_force_tag_override}
                  onChange={(allow_force_tag_override) => update({ allow_force_tag_override })}
                />
                <Toggle
                  label="Require AV1 warning acknowledgement"
                  help={settingsHelp('security.options', 'require_av1_warning')}
                  checked={draft.require_av1_warning}
                  onChange={(require_av1_warning) => update({ require_av1_warning })}
                />
                <Toggle
                  label="Require AV1 strict mode"
                  help={settingsHelp('security.options', 'require_av1_strict')}
                  checked={draft.require_av1_strict}
                  onChange={(require_av1_strict) => update({ require_av1_strict })}
                />
              </>
            )}
          </Section>

          <Section<SyncDefaults>
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
                    help={settingsHelp('sync.defaults', 'max_offset_sec')}
                  />
                  <NumberField
                    label="Checkpoint window (s)"
                    value={draft.checkpoint_window_sec}
                    onChange={(checkpoint_window_sec) => update({ checkpoint_window_sec })}
                    help={settingsHelp('sync.defaults', 'checkpoint_window_sec')}
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

          <Section<MuxOptions>
            sectionKey="mux.options"
            value={(data['mux.options'] as MuxOptions) ?? DEFAULTS['mux.options']}
            save={save}
          >
            {(draft, update) => (
              <>
                <Toggle
                  label="Keep .bak backup"
                  help={settingsHelp('mux.options', 'keep_bak')}
                  checked={draft.keep_bak}
                  onChange={(keep_bak) => update({ keep_bak })}
                />
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

          <Section<WatchOptions>
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
                  help={settingsHelp('watch.options', 'scan_interval_sec')}
                />
                <NumberField
                  label="Stability check interval (s)"
                  hint="Minimum 4 seconds per IDEA.md §3.5."
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

          <Section<ArrOptions>
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
                  help={settingsHelp('arr.options', 'poll_interval_sec')}
                />
                <Toggle
                  label="Manual sync enabled"
                  help={settingsHelp('arr.options', 'manual_sync')}
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
