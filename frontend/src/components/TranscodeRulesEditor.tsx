import type { AudioOptions, TranscodeCodec, TranscodeRule } from '../lib/api'
import { settingsHelp } from '../lib/settings-help'
import {
  Badge,
  Button,
  Field,
  InfoTip,
  Input,
  Select,
  StringListEditor,
  Toggle,
} from './ui'

const CODEC_OPTIONS: { value: TranscodeCodec; label: string }[] = [
  { value: 'keep', label: 'keep — copy untouched' },
  { value: 'opus', label: 'Opus' },
  { value: 'aac', label: 'AAC' },
  { value: 'flac', label: 'FLAC (lossless)' },
  { value: 'pcm_s16le', label: 'PCM 16-bit (lossless)' },
  { value: 'pcm_s24le', label: 'PCM 24-bit (lossless)' },
  { value: 'pcm_f32le', label: 'PCM 32-bit float (lossless)' },
]

/** Common ffmpeg layout names; the backend accepts the full canonical set. */
const LAYOUTS = [
  'mono',
  'stereo',
  '2.1',
  '3.0',
  '4.0',
  '5.0',
  '5.1',
  '6.1',
  '7.1',
]

function newRule(): TranscodeRule {
  return { enabled: true, when: { source_codecs: [] }, then: { codec: 'opus' } }
}

// pruneActions keeps the action payload valid when the output codec changes:
// a lossless codec must not keep a lossy bitrate, and `keep` must not carry any
// other action. The fields are removed (undefined), so they are omitted from
// the saved JSON instead of being sent as nulls.
function pruneActions(
  codec: TranscodeCodec,
  prev: TranscodeRule['then'],
): Partial<TranscodeRule['then']> {
  const next: Partial<TranscodeRule['then']> = { codec }
  if (codec === 'keep') {
    next.bitrate_bps = undefined
    next.channels = undefined
    next.channel_layout = undefined
    next.sample_rate = undefined
    return next
  }
  if (codec !== 'opus' && codec !== 'aac') {
    next.bitrate_bps = undefined
  }
  // Keep any previously chosen channels/layout/sample rate only when they
  // still make sense; they are harmless for every re-encoding action.
  next.channels = prev.channels
  next.channel_layout = prev.channel_layout
  next.sample_rate = prev.sample_rate
  return next
}

function NumField({
  label,
  value,
  onChange,
  help,
}: {
  label: string
  value: number | null | undefined
  onChange: (next: number | null) => void
  help?: string
}) {
  return (
    <Field label={label} help={help}>
      <Input
        type="number"
        aria-label={label}
        value={value ?? ''}
        onChange={(event) => {
          const raw = event.target.value.trim()
          onChange(raw === '' ? null : Number.parseInt(raw, 10))
        }}
      />
    </Field>
  )
}

function TextField({
  label,
  value,
  onChange,
  placeholder,
  help,
}: {
  label: string
  value: string
  onChange: (next: string) => void
  placeholder?: string
  help?: string
}) {
  return (
    <Field label={label} help={help}>
      <Input
        aria-label={label}
        value={value ?? ''}
        placeholder={placeholder}
        onChange={(event) => onChange(event.target.value)}
      />
    </Field>
  )
}

/**
 * Editor for the ordered, global `audio.options` Transcode Rules. Order is
 * significant (first enabled match wins); every change reports the whole rule
 * list upward so Settings can save one section payload.
 */
export function TranscodeRulesEditor({
  value,
  onChange,
}: {
  value: AudioOptions
  onChange: (next: AudioOptions) => void
}) {
  const rules = value.rules ?? []

  const updateRule = (index: number, patch: Partial<TranscodeRule>) =>
    onChange({ rules: rules.map((rule, i) => (i === index ? { ...rule, ...patch } : rule)) })
  const updateWhen = (index: number, patch: Partial<TranscodeRule['when']>) =>
    onChange({
      rules: rules.map((rule, i) => (i === index ? { ...rule, when: { ...rule.when, ...patch } } : rule)),
    })
  const updateThen = (index: number, patch: Partial<TranscodeRule['then']>) =>
    onChange({
      rules: rules.map((rule, i) => (i === index ? { ...rule, then: { ...rule.then, ...patch } } : rule)),
    })
  const move = (index: number, delta: -1 | 1) => {
    const target = index + delta
    if (target < 0 || target >= rules.length) return
    const next = [...rules]
    ;[next[index], next[target]] = [next[target], next[index]]
    onChange({ rules: next })
  }
  const remove = (index: number) => onChange({ rules: rules.filter((_, i) => i !== index) })

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-1.5">
        <span className="text-sm font-medium text-zinc-300">Transcode Rules</span>
        <InfoTip
          id="audio-options-rules"
          label="Transcode Rules"
          text={settingsHelp('audio.options', 'rules') ?? ''}
        />
      </div>
      <p className="text-xs text-zinc-500">
        Rules run top to bottom against the archived track&apos;s metadata. The first enabled
        rule that matches re-encodes a copy in the temp folder before muxing; everything else
        is copied untouched. The Audio Vault is never modified.
      </p>
      {rules.length === 0 && <span className="text-xs text-zinc-500">No rules — audio is copied losslessly.</span>}
      {rules.map((rule, index) => {
        const n = index + 1
        return (
          <div key={index} className="space-y-3 rounded-lg border border-ink-700 bg-ink-950/40 p-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div className="flex items-center gap-2">
                <Badge tone={rule.enabled ? 'success' : 'neutral'}>Rule {n}</Badge>
                <Toggle
                  label={`Rule ${n} enabled`}
                  checked={Boolean(rule.enabled)}
                  onChange={(enabled) => updateRule(index, { enabled })}
                  help={settingsHelp('audio.options', 'enabled')}
                />
              </div>
              <div className="flex items-center gap-1">
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  aria-label={`Move rule ${n} up`}
                  disabled={index === 0}
                  onClick={() => move(index, -1)}
                >
                  ↑
                </Button>
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  aria-label={`Move rule ${n} down`}
                  disabled={index === rules.length - 1}
                  onClick={() => move(index, 1)}
                >
                  ↓
                </Button>
                <Button
                  type="button"
                  variant="danger"
                  size="sm"
                  aria-label={`Delete rule ${n}`}
                  onClick={() => remove(index)}
                >
                  Delete
                </Button>
              </div>
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-3">
                <span className="block text-sm font-medium text-zinc-300">If all of these match</span>
                <StringListEditor
                  label={`Rule ${n} source codecs`}
                  inputLabel="Add source codec"
                  values={rule.when.source_codecs ?? []}
                  onChange={(source_codecs) => updateWhen(index, { source_codecs })}
                  placeholder="pcm_s16le"
                  help={settingsHelp('audio.options', 'when_source_codecs')}
                />
                <TextField
                  label={`Rule ${n} source codec regex`}
                  value={rule.when.source_codec_regex ?? ''}
                  onChange={(source_codec_regex) => updateWhen(index, { source_codec_regex })}
                  placeholder="^pcm_.*$"
                  help={settingsHelp('audio.options', 'when_source_codec_regex')}
                />
                <NumField
                  label={`Rule ${n} minimum bitrate (bps)`}
                  value={rule.when.min_bitrate_bps}
                  onChange={(min_bitrate_bps) => updateWhen(index, { min_bitrate_bps })}
                  help={settingsHelp('audio.options', 'when_min_bitrate_bps')}
                />
                <NumField
                  label={`Rule ${n} maximum bitrate (bps)`}
                  value={rule.when.max_bitrate_bps}
                  onChange={(max_bitrate_bps) => updateWhen(index, { max_bitrate_bps })}
                  help={settingsHelp('audio.options', 'when_max_bitrate_bps')}
                />
                <NumField
                  label={`Rule ${n} channel count`}
                  value={rule.when.channel_count}
                  onChange={(channel_count) => updateWhen(index, { channel_count })}
                  help={settingsHelp('audio.options', 'when_channel_count')}
                />
                <Field
                  label={`Rule ${n} channel layout`}
                  help={settingsHelp('audio.options', 'when_channel_layout')}
                >
                  <Select
                    aria-label={`Rule ${n} channel layout`}
                    value={rule.when.channel_layout ?? ''}
                    onChange={(event) => updateWhen(index, { channel_layout: event.target.value })}
                  >
                    <option value="">any layout</option>
                    {LAYOUTS.map((layout) => (
                      <option key={layout} value={layout}>
                        {layout}
                      </option>
                    ))}
                  </Select>
                </Field>
                <NumField
                  label={`Rule ${n} sample rate (Hz)`}
                  value={rule.when.sample_rate}
                  onChange={(sample_rate) => updateWhen(index, { sample_rate })}
                  help={settingsHelp('audio.options', 'when_sample_rate')}
                />
                <TextField
                  label={`Rule ${n} language code`}
                  value={rule.when.lang_iso2 ?? ''}
                  onChange={(lang_iso2) => updateWhen(index, { lang_iso2 })}
                  placeholder="pl"
                  help={settingsHelp('audio.options', 'when_lang_iso2')}
                />
                <TextField
                  label={`Rule ${n} variant`}
                  value={rule.when.variant ?? ''}
                  onChange={(variant) => updateWhen(index, { variant })}
                  placeholder="dubbing"
                  help={settingsHelp('audio.options', 'when_variant')}
                />
              </div>

              <div className="space-y-3">
                <span className="block text-sm font-medium text-zinc-300">Then encode to</span>
                <Field label={`Rule ${n} output codec`} help={settingsHelp('audio.options', 'then_codec')}>
                  <Select
                    aria-label={`Rule ${n} output codec`}
                    value={rule.then.codec ?? 'opus'}
                    onChange={(event) =>
                      updateThen(index, pruneActions(event.target.value as TranscodeCodec, rule.then))
                    }
                  >
                    {CODEC_OPTIONS.map((option) => (
                      <option key={option.value} value={option.value}>
                        {option.label}
                      </option>
                    ))}
                  </Select>
                </Field>
                {(rule.then.codec === 'opus' || rule.then.codec === 'aac') && (
                  <NumField
                    label={`Rule ${n} bitrate (bps)`}
                    value={rule.then.bitrate_bps}
                    onChange={(bitrate_bps) => updateThen(index, { bitrate_bps })}
                    help={settingsHelp('audio.options', 'then_bitrate_bps')}
                  />
                )}
                {rule.then.codec !== 'keep' && (
                  <>
                    <NumField
                      label={`Rule ${n} output channels`}
                      value={rule.then.channels}
                      onChange={(channels) => updateThen(index, { channels })}
                      help={settingsHelp('audio.options', 'then_channels')}
                    />
                    <Field
                      label={`Rule ${n} output layout`}
                      help={settingsHelp('audio.options', 'then_channel_layout')}
                    >
                      <Select
                        aria-label={`Rule ${n} output layout`}
                        value={rule.then.channel_layout ?? ''}
                        onChange={(event) => updateThen(index, { channel_layout: event.target.value })}
                      >
                        <option value="">match channels</option>
                        {LAYOUTS.map((layout) => (
                          <option key={layout} value={layout}>
                            {layout}
                          </option>
                        ))}
                      </Select>
                    </Field>
                    <NumField
                      label={`Rule ${n} output sample rate (Hz)`}
                      value={rule.then.sample_rate}
                      onChange={(sample_rate) => updateThen(index, { sample_rate })}
                      help={settingsHelp('audio.options', 'then_sample_rate')}
                    />
                  </>
                )}
              </div>
            </div>
          </div>
        )
      })}
      <Button
        type="button"
        variant="secondary"
        onClick={() => onChange({ rules: [...rules, newRule()] })}
      >
        Add rule
      </Button>
    </div>
  )
}
