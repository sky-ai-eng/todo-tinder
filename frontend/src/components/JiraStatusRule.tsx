interface JiraStatus {
  id: string
  name: string
}

export interface JiraStatusRuleValue {
  members: string[]
  canonical?: string
}

interface Props {
  label: string
  description: string
  allStatuses: JiraStatus[]
  value: JiraStatusRuleValue
  onChange: (next: JiraStatusRuleValue) => void
  requireCanonical: boolean
  canonicalPrompt?: string
}

export default function JiraStatusRule({
  label,
  description,
  allStatuses,
  value,
  onChange,
  requireCanonical,
  canonicalPrompt,
}: Props) {
  const toggle = (name: string) => {
    if (value.members.includes(name)) {
      const nextMembers = value.members.filter((n) => n !== name)
      const nextCanonical = value.canonical === name ? undefined : value.canonical
      onChange({ members: nextMembers, canonical: nextCanonical })
    } else {
      const nextMembers = [...value.members, name]
      const nextCanonical =
        requireCanonical && !value.canonical && value.members.length === 0 ? name : value.canonical
      onChange({ members: nextMembers, canonical: nextCanonical })
    }
  }

  const showCanonicalWarning = requireCanonical && value.members.length > 0 && !value.canonical

  return (
    <div className="space-y-3">
      <div>
        <div className="text-[12px] font-medium text-text-primary">{label}</div>
        <div className="text-[11px] text-text-tertiary mt-0.5">{description}</div>
      </div>

      <div className="flex flex-wrap gap-2">
        {allStatuses.map((s) => {
          const selected = value.members.includes(s.name)
          return (
            <button
              key={s.id}
              type="button"
              onClick={() => toggle(s.name)}
              className={`text-[11px] px-3 py-1.5 rounded-full border transition-colors ${
                selected
                  ? 'bg-accent/[0.1] border-accent/30 text-accent font-medium'
                  : 'bg-white/50 border-border-subtle text-text-tertiary hover:text-text-secondary hover:border-border-subtle/80'
              }`}
            >
              {s.name}
            </button>
          )
        })}
      </div>

      {requireCanonical && (
        <div className="space-y-1.5">
          <div className="text-[11px] text-text-tertiary">
            {canonicalPrompt || 'Set Jira status to'}
          </div>
          <select
            value={value.canonical || ''}
            onChange={(e) =>
              onChange({
                members: value.members,
                canonical: e.target.value || undefined,
              })
            }
            disabled={value.members.length === 0}
            className={`w-full bg-white/50 border rounded-xl px-3 py-2 text-[13px] text-text-primary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors disabled:opacity-40 disabled:cursor-not-allowed ${
              showCanonicalWarning ? 'border-dismiss/40' : 'border-border-subtle'
            }`}
          >
            <option value="">
              {value.members.length === 0 ? 'Pick a status above first' : 'Choose one…'}
            </option>
            {value.members.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
          {showCanonicalWarning && (
            <div className="text-[11px] text-dismiss">
              Pick one of the statuses above — TF needs a specific target to transition into.
            </div>
          )}
        </div>
      )}
    </div>
  )
}
