import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import RepoPickerModal from '../components/RepoPickerModal'

interface JiraStatus {
  id: string
  name: string
}

type Step = 'auth' | 'repo_select' | 'jira_config'

export default function Setup() {
  const navigate = useNavigate()
  const [step, setStep] = useState<Step>('auth')
  const [githubEnabled, setGithubEnabled] = useState(true)
  const [jiraEnabled, setJiraEnabled] = useState(false)
  const [form, setForm] = useState({
    github_url: '',
    github_pat: '',
    jira_url: '',
    jira_pat: '',
    jira_projects: '',
    jira_pickup_statuses: [] as string[],
    jira_in_progress_status: '',
  })
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [jiraStatuses, setJiraStatuses] = useState<JiraStatus[]>([])
  const [statusesLoading, setStatusesLoading] = useState(false)

  const update = (field: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm((f) => ({ ...f, [field]: e.target.value }))

  const canSubmit = (githubEnabled && form.github_url && form.github_pat) ||
                    (jiraEnabled && form.jira_url && form.jira_pat)

  const submitAuth = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')

    if (!canSubmit) {
      setError('Enable at least one service and fill in its fields')
      return
    }

    setLoading(true)
    try {
      const body = {
        github_url: githubEnabled ? form.github_url : '',
        github_pat: githubEnabled ? form.github_pat : '',
        jira_url: jiraEnabled ? form.jira_url : '',
        jira_pat: jiraEnabled ? form.jira_pat : '',
      }
      const res = await fetch('/api/auth/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Setup failed')
        return
      }

      // Next step: repo selection if GitHub enabled, then Jira config if Jira enabled
      if (githubEnabled) {
        setStep('repo_select')
      } else if (jiraEnabled) {
        setStep('jira_config')
      } else {
        navigate('/')
      }
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  const saveRepos = async (repos: string[]) => {
    setLoading(true)
    setError('')
    try {
      const res = await fetch('/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          github_enabled: true,
          github_repos: repos,
        }),
      })
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Failed to save repos')
        setLoading(false)
        return
      }

      if (jiraEnabled) {
        setStep('jira_config')
      } else {
        navigate('/')
      }
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  const fetchStatuses = async () => {
    const projects = form.jira_projects.split(',').map((s) => s.trim()).filter(Boolean)
    if (projects.length === 0) return
    setStatusesLoading(true)
    try {
      const params = projects.map((p) => `project=${encodeURIComponent(p)}`).join('&')
      const res = await fetch(`/api/jira/statuses?${params}`)
      if (res.ok) {
        setJiraStatuses(await res.json())
      } else {
        const data = await res.json()
        setError(data.error || 'Failed to fetch statuses')
      }
    } catch {
      setError('Could not fetch Jira statuses')
    } finally {
      setStatusesLoading(false)
    }
  }

  const saveJiraConfig = async () => {
    setLoading(true)
    setError('')
    try {
      const projects = form.jira_projects.split(',').map((s) => s.trim()).filter(Boolean)
      const res = await fetch('/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          jira_enabled: true,
          jira_projects: projects,
          jira_pickup_statuses: form.jira_pickup_statuses,
          jira_in_progress_status: form.jira_in_progress_status,
        }),
      })
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Failed to save')
        return
      }
      navigate('/')
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      {step === 'auth' && (
        <form
          onSubmit={submitAuth}
          className="w-full max-w-lg backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]"
        >
          <div>
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">Todo Tinder Setup</h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Tokens are stored in your OS keychain and never leave your machine.
              Enable the services you use.
            </p>
          </div>

          {/* GitHub */}
          <fieldset className={`space-y-3 transition-opacity ${!githubEnabled ? 'opacity-40' : ''}`}>
            <div className="flex items-center justify-between">
              <legend className="text-[13px] font-medium text-text-secondary">GitHub</legend>
              <Toggle enabled={githubEnabled} onChange={setGithubEnabled} />
            </div>
            {githubEnabled && (
              <>
                <input
                  type="url"
                  placeholder="https://github.yourcompany.com"
                  value={form.github_url}
                  onChange={update('github_url')}
                  className={inputClass}
                />
                <input
                  type="password"
                  placeholder="GitHub Personal Access Token"
                  value={form.github_pat}
                  onChange={update('github_pat')}
                  className={inputClass}
                />
                <p className="text-[11px] text-text-tertiary">
                  Requires <code className="text-text-secondary">repo</code> and{' '}
                  <code className="text-text-secondary">read:org</code> scopes.
                </p>
              </>
            )}
          </fieldset>

          {/* Jira */}
          <fieldset className={`space-y-3 transition-opacity ${!jiraEnabled ? 'opacity-40' : ''}`}>
            <div className="flex items-center justify-between">
              <legend className="text-[13px] font-medium text-text-secondary">Jira</legend>
              <Toggle enabled={jiraEnabled} onChange={setJiraEnabled} />
            </div>
            {jiraEnabled && (
              <>
                <input
                  type="url"
                  placeholder="https://jira.yourcompany.com"
                  value={form.jira_url}
                  onChange={update('jira_url')}
                  className={inputClass}
                />
                <input
                  type="password"
                  placeholder="Jira Personal Access Token"
                  value={form.jira_pat}
                  onChange={update('jira_pat')}
                  className={inputClass}
                />
              </>
            )}
          </fieldset>

          {error && (
            <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss">
              {error}
            </div>
          )}

          <button
            type="submit"
            disabled={loading || !canSubmit}
            className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {loading ? 'Validating...' : 'Connect & Continue'}
          </button>
        </form>
      )}

      {step === 'repo_select' && (
        <RepoPickerModal
          selected={[]}
          onSave={saveRepos}
          onClose={() => {/* no-op: can't skip repo selection */}}
          inline
        />
      )}

      {step === 'jira_config' && (
        <div className="w-full max-w-lg backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]">
          <div>
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">Configure Jira</h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Enter your project keys, then fetch the available workflow statuses to configure which tickets to pick up.
            </p>
          </div>

          <div className="space-y-3">
            <label className="block">
              <span className="text-[11px] text-text-tertiary mb-1.5 block">Projects (comma-separated)</span>
              <div className="flex gap-2">
                <input
                  type="text"
                  placeholder="PROJ, INFRA"
                  value={form.jira_projects}
                  onChange={update('jira_projects')}
                  className={inputClass + ' flex-1'}
                />
                <button
                  type="button"
                  onClick={fetchStatuses}
                  disabled={statusesLoading || !form.jira_projects.trim()}
                  className="shrink-0 text-[11px] text-accent hover:text-accent/80 disabled:opacity-40 border border-accent/20 rounded-xl px-3 py-2 transition-colors"
                >
                  {statusesLoading ? 'Loading...' : 'Fetch Statuses'}
                </button>
              </div>
            </label>

            {jiraStatuses.length > 0 && (
              <>
                <label className="block">
                  <span className="text-[11px] text-text-tertiary mb-1.5 block">
                    Pickup statuses (poll for unassigned tickets in these states)
                  </span>
                  <div className="flex flex-wrap gap-2">
                    {jiraStatuses.map((s) => (
                      <StatusChip
                        key={s.id}
                        label={s.name}
                        selected={form.jira_pickup_statuses.includes(s.name)}
                        onClick={() =>
                          setForm((f) => ({
                            ...f,
                            jira_pickup_statuses: f.jira_pickup_statuses.includes(s.name)
                              ? f.jira_pickup_statuses.filter((n) => n !== s.name)
                              : [...f.jira_pickup_statuses, s.name],
                          }))
                        }
                      />
                    ))}
                  </div>
                </label>

                <label className="block">
                  <span className="text-[11px] text-text-tertiary mb-1.5 block">
                    In-progress status (set when you claim a ticket)
                  </span>
                  <div className="flex flex-wrap gap-2">
                    {jiraStatuses.map((s) => (
                      <StatusChip
                        key={s.id}
                        label={s.name}
                        selected={form.jira_in_progress_status === s.name}
                        onClick={() =>
                          setForm((f) => ({
                            ...f,
                            jira_in_progress_status: f.jira_in_progress_status === s.name ? '' : s.name,
                          }))
                        }
                      />
                    ))}
                  </div>
                </label>
              </>
            )}
          </div>

          {error && (
            <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss">
              {error}
            </div>
          )}

          <div className="flex gap-3">
            <button
              type="button"
              onClick={() => navigate('/')}
              className="flex-1 text-[13px] text-text-secondary hover:text-text-primary border border-border-subtle rounded-xl px-4 py-2.5 transition-colors"
            >
              Skip for Now
            </button>
            <button
              type="button"
              onClick={saveJiraConfig}
              disabled={loading}
              className="flex-1 bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
            >
              {loading ? 'Saving...' : 'Save & Start'}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

const inputClass =
  'w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors'

function Toggle({ enabled, onChange }: { enabled: boolean; onChange: (v: boolean) => void }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={enabled}
      onClick={() => onChange(!enabled)}
      className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
        enabled ? 'bg-accent' : 'bg-black/[0.08]'
      }`}
    >
      <span
        className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow-sm transform transition-transform ${
          enabled ? 'translate-x-4' : 'translate-x-0'
        }`}
      />
    </button>
  )
}

function StatusChip({ label, selected, onClick }: { label: string; selected: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`text-[11px] px-3 py-1.5 rounded-full border transition-colors ${
        selected
          ? 'bg-accent/[0.1] border-accent/30 text-accent font-medium'
          : 'bg-white/50 border-border-subtle text-text-tertiary hover:text-text-secondary hover:border-border-subtle/80'
      }`}
    >
      {label}
    </button>
  )
}
