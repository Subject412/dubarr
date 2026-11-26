import { useEffect, useState, type FormEvent } from 'react'
import { Link, Navigate, useNavigate } from 'react-router-dom'
import { ApiError, api, type LanguagesPolicy, type SecurityOptions, type StoragePaths, type TargetTagFilter } from '../lib/api'
import { DEFAULT_SETTINGS } from '../lib/defaults'
import { useAuthStatus, useSaveSettings, useSettings, useSetup } from '../lib/hooks'
import { ArrConnectionsPanel } from '../components/ArrConnections'
import { PathField, PathListEditor } from '../components/PathPicker'
import { cn } from '../lib/cn'
import {
  Alert,
  Badge,
  Button,
  Field,
  Input,
  Select,
  StringListEditor,
  Toggle,
} from '../components/ui'

const STEPS = ['Administrator', 'Storage', 'Languages & tags', '*arr connections', 'Security', 'Finish'] as const

export function SetupPage() {
  const navigate = useNavigate()
  const status = useAuthStatus()
  const setup = useSetup()
  const saveSettings = useSaveSettings()
  const [step, setStep] = useState(0)

  const settings = useSettings(step >= 1)

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [adminError, setAdminError] = useState('')

  const [storage, setStorage] = useState<StoragePaths>(DEFAULT_SETTINGS['storage.paths'])
  const [storageError, setStorageError] = useState('')

  const [languages, setLanguages] = useState<LanguagesPolicy>(DEFAULT_SETTINGS['languages.policy'])
  const [tagFilter, setTagFilter] = useState<TargetTagFilter>(DEFAULT_SETTINGS['target.tag_filter'])
  const [languageError, setLanguageError] = useState('')

  const [security, setSecurity] = useState<SecurityOptions>(DEFAULT_SETTINGS['security.options'])
  const [stepError, setStepError] = useState('')
  const [adminCreated, setAdminCreated] = useState(false)
  const [requiresLogin, setRequiresLogin] = useState(false)

  // Adopt anything already persisted (e.g. reseeding work_dir from the server).
  // oxlint-disable react/set-state-in-effect
  useEffect(() => {
    const data = settings.data?.settings
    if (!data) return
    if (data['storage.paths']) setStorage(data['storage.paths'])
    if (data['languages.policy']) setLanguages(data['languages.policy'])
    if (data['target.tag_filter']) setTagFilter(data['target.tag_filter'])
    if (data['security.options']) setSecurity(data['security.options'])
  }, [settings.data])
  // oxlint-enable react/set-state-in-effect

  // An already-initialised, signed-in visitor does not belong in the wizard.
  // `adminCreated` is set synchronously right after a successful setup mutation,
  // before any further awaits, so the auth-status refetch that the mutation
  // invalidates cannot commit while this guard still believes the wizard is unowned.
  if (!adminCreated && status.data?.initialized && status.data.authenticated) {
    return <Navigate to="/library" replace />
  }

  // Parts of the install the operator would still be missing if they left now.
  // Purely derived from local state: this drives the warning text only, and the
  // 'Set up later' escape below performs no writes at all.
  const unsetParts: string[] = []
  if (storage.watch_dir.trim() === '') unsetParts.push('watch folder')
  if (storage.vault_dir.trim() === '') unsetParts.push('vault folder')
  if ((storage.media_roots ?? []).length === 0) unsetParts.push('media roots')

  const skipSetup = () => {
    // Deliberately write-free: leave whatever is already persisted untouched and
    // let the operator finish each section from Settings.
    navigate('/library', { replace: true })
  }

  const saveStep = async (key: string, value: object) => {
    setStepError('')
    await saveSettings.mutateAsync({ [key]: value })
  }

  const submitAdmin = async (event: FormEvent) => {
    event.preventDefault()
    setAdminError('')
    if (username.trim() === '') {
      setAdminError('Username is required.')
      return
    }
    if (password.length < 8) {
      setAdminError('Password must be at least 8 characters.')
      return
    }
    if (password !== confirm) {
      setAdminError('Passwords do not match.')
      return
    }
    try {
      await setup.mutateAsync({ username: username.trim(), password })
      // Claim the wizard before any further await: the mutation invalidates the
      // auth-status query, and that refetch is delivered on a later macrotask.
      // If it landed while `adminCreated` was still false the guard above would
      // redirect to /library and unmount this wizard mid-flow.
      setAdminCreated(true)
      // /api/auth/setup sets the session cookie itself; fall back to login if a
      // deployment ever returns 201 without a session. This is best effort and
      // must never strand the wizard: the account already exists server-side.
      try {
        const probe = await api.authStatus()
        if (!probe.authenticated) await api.login(username.trim(), password)
      } catch {
        // /api/auth/setup sets the session cookie itself. If this best-effort
        // probe fails the account still exists; continue the wizard rather than
        // stranding the operator on a form that can no longer be submitted.
      }
      setStep(1)
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        setRequiresLogin(true)
        setAdminError('Setup already completed on this appliance. Sign in with the existing account.')
        return
      }
      setAdminError(error instanceof Error ? error.message : 'Could not create the administrator.')
    }
  }

  const submitStorage = async (event: FormEvent) => {
    event.preventDefault()
    setStorageError('')
    const missing = [storage.watch_dir, storage.vault_dir].find((path) => path.trim() === '')
    if (missing !== undefined) {
      setStorageError('Watch folder and vault folder are required.')
      return
    }
    const invalid = [
      storage.watch_dir,
      storage.vault_dir,
      storage.work_dir,
      ...(storage.media_roots ?? []),
    ]
      .filter((path) => path.trim() !== '')
      .find((path) => !path.startsWith('/'))
    if (invalid !== undefined) {
      setStorageError(`Path "${invalid}" must be absolute.`)
      return
    }
    try {
      await saveStep('storage.paths', storage)
      setStep(2)
    } catch (error) {
      setStorageError(error instanceof Error ? error.message : 'Could not save storage paths.')
    }
  }

  const submitLanguages = async (event: FormEvent) => {
    event.preventDefault()
    setLanguageError('')
    if (languages.target_import_languages.length === 0) {
      setLanguageError('Add at least one target import language.')
      return
    }
    try {
      await saveSettings.mutateAsync({
        'languages.policy': languages,
        'target.tag_filter': tagFilter,
      })
      setStep(3)
    } catch (error) {
      setLanguageError(error instanceof Error ? error.message : 'Could not save language policy.')
    }
  }

  const submitSecurity = async (event: FormEvent) => {
    event.preventDefault()
    try {
      await saveStep('security.options', security)
      setStep(5)
    } catch (error) {
      setStepError(error instanceof Error ? error.message : 'Could not save security options.')
    }
  }

  return (
    <div className="min-h-full bg-ink-950 px-4 py-10 sm:py-16">
      <div className="mx-auto w-full max-w-3xl">
        <header className="mb-8 flex items-center gap-3">
          <span className="grid h-11 w-11 place-items-center rounded-xl bg-accent-500 text-xl font-bold text-ink-950">
            D
          </span>
          <div>
            <h1 className="text-xl font-semibold tracking-tight text-zinc-100">Dubarr first-run setup</h1>
            <p className="text-sm text-zinc-500">Configure the appliance before your first library scan.</p>
          </div>
        </header>

        <ol className="mb-6 flex flex-wrap gap-2" aria-label="Setup progress">
          {STEPS.map((name, index) => (
            <li key={name} className="flex items-center gap-2">
              <span
                className={cn(
                  'grid h-6 w-6 place-items-center rounded-full border text-xs font-semibold',
                  index < step && 'border-accent-500 bg-accent-500 text-ink-950',
                  index === step && 'border-accent-500 text-accent-400',
                  index > step && 'border-ink-600 text-zinc-500',
                )}
                aria-current={index === step ? 'step' : undefined}
              >
                {index < step ? '✓' : index + 1}
              </span>
              <span
                className={cn(
                  'text-xs',
                  index === step ? 'text-zinc-100' : 'text-zinc-500',
                  index > step && 'hidden sm:inline',
                )}
              >
                {name}
              </span>
            </li>
          ))}
        </ol>

        <div className="rounded-xl border border-ink-800 bg-ink-900 p-6 shadow-xl">
          {step === 0 && (
            <form onSubmit={(event) => void submitAdmin(event)} className="space-y-5" noValidate>
              <div>
                <h2 className="text-lg font-semibold text-zinc-100">Administrator account</h2>
                <p className="text-sm text-zinc-400">
                  This account owns the appliance. Password must be at least 8 characters.
                </p>
              </div>
              {adminError && <Alert>{adminError}</Alert>}
              {requiresLogin && (
                <p className="text-sm text-zinc-400">
                  <Link className="text-accent-400 underline" to="/login">
                    Go to sign in
                  </Link>
                </p>
              )}
              <Field label="Username" htmlFor="setup-username">
                <Input
                  id="setup-username"
                  autoComplete="username"
                  value={username}
                  onChange={(event) => setUsername(event.target.value)}
                />
              </Field>
              <Field label="Password" hint="Minimum 8 characters." htmlFor="setup-password">
                <Input
                  id="setup-password"
                  type="password"
                  autoComplete="new-password"
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                />
              </Field>
              <Field label="Confirm password" htmlFor="setup-confirm">
                <Input
                  id="setup-confirm"
                  type="password"
                  autoComplete="new-password"
                  value={confirm}
                  onChange={(event) => setConfirm(event.target.value)}
                />
              </Field>
              <div className="flex justify-end">
                <Button type="submit" disabled={setup.isPending}>
                  {setup.isPending ? 'Creating…' : 'Create administrator'}
                </Button>
              </div>
            </form>
          )}

          {step === 1 && (
            <form onSubmit={(event) => void submitStorage(event)} className="space-y-5">
              <div>
                <h2 className="text-lg font-semibold text-zinc-100">Storage paths</h2>
                <p className="text-sm text-zinc-400">
                  Absolute paths the appliance watches, protects and works in.
                </p>
              </div>
              {storageError && <Alert>{storageError}</Alert>}
              <div className="grid gap-4 md:grid-cols-2">
                <PathField
                  label="Watch folder"
                  hint="Incoming donor media."
                  value={storage.watch_dir}
                  onChange={(watch_dir) => setStorage({ ...storage, watch_dir })}
                />
                <PathField
                  label="Vault folder"
                  hint="Read-only source of original tracks."
                  value={storage.vault_dir}
                  onChange={(vault_dir) => setStorage({ ...storage, vault_dir })}
                />
                <PathField
                  label="Work folder"
                  value={storage.work_dir}
                  onChange={(work_dir) => setStorage({ ...storage, work_dir })}
                />
              </div>
              <PathListEditor
                label="Media roots"
                hint="Libraries available to Dubarr (optional now, required before syncing)."
                values={storage.media_roots ?? []}
                onChange={(media_roots) => setStorage({ ...storage, media_roots })}
              />
              <div className="flex justify-between">
                <Button type="button" variant="ghost" onClick={() => setStep(0)}>
                  Back
                </Button>
                <Button type="submit" disabled={saveSettings.isPending}>
                  {saveSettings.isPending ? 'Saving…' : 'Save & continue'}
                </Button>
              </div>
            </form>
          )}

          {step === 2 && (
            <form onSubmit={(event) => void submitLanguages(event)} className="space-y-5">
              <div>
                <h2 className="text-lg font-semibold text-zinc-100">Languages &amp; tags</h2>
                <p className="text-sm text-zinc-400">
                  Tracks to import, reference languages to keep, and which files the processing filter accepts.
                </p>
              </div>
              {languageError && <Alert>{languageError}</Alert>}
              <StringListEditor
                label="Target import languages"
                inputLabel="Add target language"
                values={languages.target_import_languages}
                onChange={(target_import_languages) => setLanguages({ ...languages, target_import_languages })}
                placeholder="pl"
              />
              <StringListEditor
                label="Extra reference languages"
                inputLabel="Add reference language"
                values={languages.extra_reference_languages}
                onChange={(extra_reference_languages) =>
                  setLanguages({ ...languages, extra_reference_languages })
                }
                placeholder="en"
              />
              <div className="grid gap-3 md:grid-cols-2">
                <Toggle
                  label="Original language from *arr"
                  checked={languages.original_language_from_arr}
                  onChange={(original_language_from_arr) =>
                    setLanguages({ ...languages, original_language_from_arr })
                  }
                />
                <Toggle
                  label="Exclude commentary"
                  checked={languages.exclude_commentary}
                  onChange={(exclude_commentary) => setLanguages({ ...languages, exclude_commentary })}
                />
              </div>

              <div className="border-t border-ink-800 pt-4">
                <StringListEditor
                  label="Required filename tags"
                  inputLabel="Add required tag"
                  values={tagFilter.required_filename_tags}
                  onChange={(required_filename_tags) => setTagFilter({ ...tagFilter, required_filename_tags })}
                  placeholder="[TAG]"
                />
              </div>
              <div className="grid gap-4 md:grid-cols-2">
                <Field label="Tag match mode" htmlFor="setup-tag-mode">
                  <Select
                    id="setup-tag-mode"
                    value={tagFilter.tag_match_mode}
                    onChange={(event) =>
                      setTagFilter({
                        ...tagFilter,
                        tag_match_mode: event.target.value as TargetTagFilter['tag_match_mode'],
                      })
                    }
                  >
                    <option value="ANY">ANY</option>
                    <option value="ALL">ALL</option>
                    <option value="REGEX">REGEX</option>
                  </Select>
                </Field>
                <div className="space-y-1">
                  <Toggle
                    label="Case sensitive tags"
                    checked={tagFilter.tag_case_sensitive}
                    onChange={(tag_case_sensitive) => setTagFilter({ ...tagFilter, tag_case_sensitive })}
                  />
                  <Toggle
                    label="Enforce tag filter"
                    checked={tagFilter.enforce_tag_filter}
                    onChange={(enforce_tag_filter) => setTagFilter({ ...tagFilter, enforce_tag_filter })}
                  />
                </div>
              </div>
              <div className="flex justify-between">
                <Button type="button" variant="ghost" onClick={() => setStep(1)}>
                  Back
                </Button>
                <Button type="submit" disabled={saveSettings.isPending}>
                  {saveSettings.isPending ? 'Saving…' : 'Save & continue'}
                </Button>
              </div>
            </form>
          )}

          {step === 3 && (
            <div className="space-y-5">
              <div>
                <h2 className="text-lg font-semibold text-zinc-100">Radarr &amp; Sonarr</h2>
                <p className="text-sm text-zinc-400">
                  Optional: save connections now so original-language detection and library sync work
                  immediately. You can skip this and add or rotate API keys later under Settings.
                </p>
              </div>
              <ArrConnectionsPanel />
              <div className="flex justify-between">
                <Button type="button" variant="ghost" onClick={() => setStep(2)}>
                  Back
                </Button>
                <div className="flex gap-2">
                  <Button type="button" variant="ghost" onClick={() => setStep(4)}>
                    Skip for now
                  </Button>
                  <Button type="button" onClick={() => setStep(4)}>
                    Continue
                  </Button>
                </div>
              </div>
            </div>
          )}

          {step === 4 && (
            <form onSubmit={(event) => void submitSecurity(event)} className="space-y-5">
              <div>
                <h2 className="text-lg font-semibold text-zinc-100">Security</h2>
                <p className="text-sm text-zinc-400">Appliance-wide safety switches.</p>
              </div>
              {stepError && <Alert>{stepError}</Alert>}
              <div className="flex flex-wrap items-center gap-2 rounded-lg border border-ink-800 bg-ink-950/50 px-3 py-2">
                <span className="text-sm text-zinc-400">OS keyring</span>
                <Badge tone={status.data?.keyring_available ? 'success' : 'warning'}>
                  {status.data?.keyring_available ? 'available' : 'unavailable'}
                </Badge>
                <span className="text-xs text-zinc-500">
                  Master-key mode is chosen at boot; this page cannot change it.
                </span>
              </div>
              <Toggle
                label="Allow force tag override"
                description="Permit per-item bypass of the processing filter."
                checked={security.allow_force_tag_override}
                onChange={(allow_force_tag_override) => setSecurity({ ...security, allow_force_tag_override })}
              />
              <Toggle
                label="Require AV1 warning"
                checked={security.require_av1_warning}
                onChange={(require_av1_warning) => setSecurity({ ...security, require_av1_warning })}
              />
              <Toggle
                label="Require AV1 strict mode"
                checked={security.require_av1_strict}
                onChange={(require_av1_strict) => setSecurity({ ...security, require_av1_strict })}
              />
              <div className="flex justify-between">
                <Button type="button" variant="ghost" onClick={() => setStep(3)}>
                  Back
                </Button>
                <Button type="submit" disabled={saveSettings.isPending}>
                  {saveSettings.isPending ? 'Saving…' : 'Save & continue'}
                </Button>
              </div>
            </form>
          )}

          {step === 5 && (
            <div className="space-y-5">
              <div>
                <h2 className="text-lg font-semibold text-zinc-100">Ready to go</h2>
                <p className="text-sm text-zinc-400">Review the configuration Dubarr will start with.</p>
              </div>
              <dl className="divide-y divide-ink-800 rounded-lg border border-ink-800">
                <div className="flex justify-between gap-4 px-3 py-2 text-sm">
                  <dt className="text-zinc-500">Administrator</dt>
                  <dd className="text-zinc-200">{username || 'created'}</dd>
                </div>
                <div className="flex justify-between gap-4 px-3 py-2 text-sm">
                  <dt className="text-zinc-500">Watch / vault</dt>
                  <dd className="truncate font-mono text-xs text-zinc-300">
                    {storage.watch_dir} · {storage.vault_dir}
                  </dd>
                </div>
                <div className="flex justify-between gap-4 px-3 py-2 text-sm">
                  <dt className="text-zinc-500">Target languages</dt>
                  <dd className="text-zinc-200">{languages.target_import_languages.join(', ') || '—'}</dd>
                </div>
                <div className="flex justify-between gap-4 px-3 py-2 text-sm">
                  <dt className="text-zinc-500">Required tags</dt>
                  <dd className="text-zinc-200">
                    {tagFilter.required_filename_tags.join(', ') || 'none'}
                  </dd>
                </div>
              </dl>
              <div className="flex justify-between">
                <Button type="button" variant="ghost" onClick={() => setStep(4)}>
                  Back
                </Button>
                <Button type="button" onClick={() => navigate('/library', { replace: true })}>
                  Open Library
                </Button>
              </div>
            </div>
          )}
        </div>

        {/* Secondary escape route. Rendered only once the administrator exists
            (step 0 is mandatory) and before the Finish step, where 'Open Library'
            is the same action. It never writes: it just leaves. */}
        {step >= 1 && step < 5 && (
          <div className="mt-6 flex flex-col items-center gap-2 text-center text-xs">
            <p className="text-zinc-600">
              Settings can be changed later under Settings → each section saves independently.
            </p>
            {/* Local `storage` still holds DEFAULT_SETTINGS until the settings
                query resolves, so a wrong warning would flash for operators who
                already have paths persisted. Only warn once data has settled. */}
            {!settings.isPending && settings.data !== undefined && unsetParts.length > 0 && (
              <p className="text-amber-300/80">
                Still unset: {unsetParts.join(', ')}. You can finish these under Settings.
              </p>
            )}
            <Button type="button" variant="ghost" size="sm" onClick={skipSetup}>
              Set up later
            </Button>
          </div>
        )}
      </div>
    </div>
  )
}
