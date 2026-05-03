import { useState, useEffect, useCallback, useMemo } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { ArrowLeft, Trash2, Pencil, Check, X } from 'lucide-react'
import Markdown from 'react-markdown'
import type { Project, KnowledgeFile } from '../types'
import { readError } from '../lib/api'
import { toast } from '../components/Toast/toastStore'
import RepoMultiSelect from '../components/RepoMultiSelect'
import TrackerProjectPickers from '../components/TrackerProjectPickers'

// ProjectDetail is the per-project workspace. Three sections, top-to-
// bottom:
//   1. Header — name + description (inline-editable), summary chips.
//   2. Configuration — pinned repos editor + tracker projects pickers.
//   3. Knowledge base — markdown files under the project's
//      knowledge-base directory, rendered read-only.
//
// The chat panel slot lives on the right side of the layout but isn't
// implemented here — SKY-226 grafts in a streaming chat with renderers,
// queueing, and cancellation. The placeholder column reserves the
// space so SKY-226 doesn't trigger a re-layout when it lands.
export default function ProjectDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [project, setProject] = useState<Project | null>(null)
  const [loading, setLoading] = useState(true)
  const [missing, setMissing] = useState(false)

  const refresh = useCallback(async () => {
    if (!id) return
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(id)}`)
      if (res.status === 404) {
        setMissing(true)
        return
      }
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to load project'))
        return
      }
      const data: Project = await res.json()
      setProject(data)
    } catch (err) {
      toast.error(`Failed to load project: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setLoading(false)
    }
  }, [id])

  useEffect(() => {
    refresh()
  }, [refresh])

  const patch = useCallback(
    async (body: Record<string, unknown>) => {
      if (!id) return
      try {
        const res = await fetch(`/api/projects/${encodeURIComponent(id)}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to update project'))
          return false
        }
        const fresh: Project = await res.json()
        setProject(fresh)
        return true
      } catch (err) {
        toast.error(`Failed to update project: ${err instanceof Error ? err.message : String(err)}`)
        return false
      }
    },
    [id],
  )

  const handleDelete = useCallback(async () => {
    if (!id || !project) return
    if (!confirm(`Delete project "${project.name}"? This can't be undone.`)) return
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(id)}`, { method: 'DELETE' })
      if (!res.ok && res.status !== 204) {
        toast.error(await readError(res, 'Failed to delete project'))
        return
      }
      const cleanupWarning = res.headers.get('X-Cleanup-Warning')
      if (cleanupWarning) {
        toast.warning(cleanupWarning)
      } else {
        toast.success(`Deleted project "${project.name}"`)
      }
      navigate('/projects')
    } catch (err) {
      toast.error(`Failed to delete project: ${err instanceof Error ? err.message : String(err)}`)
    }
  }, [id, project, navigate])

  if (loading) {
    return (
      <div className="max-w-7xl mx-auto">
        <div className="text-text-tertiary text-[13px]">Loading project…</div>
      </div>
    )
  }

  if (missing || !project) {
    return (
      <div className="max-w-7xl mx-auto">
        <Link
          to="/projects"
          className="inline-flex items-center gap-1 text-[13px] text-text-secondary hover:text-text-primary mb-6"
        >
          <ArrowLeft size={14} /> Projects
        </Link>
        <div className="text-text-secondary text-[13px]">
          Project not found. It may have been deleted.
        </div>
      </div>
    )
  }

  return (
    <div className="max-w-7xl mx-auto">
      <div className="flex items-center justify-between mb-6">
        <Link
          to="/projects"
          className="inline-flex items-center gap-1 text-[13px] text-text-secondary hover:text-text-primary"
        >
          <ArrowLeft size={14} /> Projects
        </Link>
        <button
          type="button"
          onClick={handleDelete}
          className="
            inline-flex items-center gap-1.5 rounded-full
            px-3 py-1.5 text-[12px]
            text-dismiss/80 hover:text-dismiss hover:bg-dismiss/[0.08]
            transition-all
          "
        >
          <Trash2 size={12} />
          Delete project
        </button>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-[2fr_1fr] gap-6">
        <div className="space-y-6">
          <ProjectHeader
            project={project}
            onPatchName={(name) => patch({ name })}
            onPatchDescription={(description) => patch({ description })}
          />

          <ConfigPanel project={project} onPatch={patch} />

          <KnowledgePanel projectId={project.id} />
        </div>

        <ChatSlotPlaceholder />
      </div>
    </div>
  )
}

// ProjectHeader handles inline edit for name + description. We use
// dedicated edit modes rather than always-editable inputs so the
// rendered project view feels read-y at rest.
function ProjectHeader({
  project,
  onPatchName,
  onPatchDescription,
}: {
  project: Project
  onPatchName: (name: string) => Promise<boolean | undefined>
  onPatchDescription: (description: string) => Promise<boolean | undefined>
}) {
  const [editingName, setEditingName] = useState(false)
  const [editingDesc, setEditingDesc] = useState(false)
  const [draftName, setDraftName] = useState(project.name)
  const [draftDesc, setDraftDesc] = useState(project.description)

  // The drafts are seeded when the user enters edit mode (via the
  // click handlers below) and overwritten by user input from there.
  // No effect-driven sync is needed: the at-rest path renders
  // project.name / project.description directly, and entering edit
  // mode is an explicit user action that captures the current value.

  const beginEditName = () => {
    setDraftName(project.name)
    setEditingName(true)
  }

  const beginEditDesc = () => {
    setDraftDesc(project.description)
    setEditingDesc(true)
  }

  const saveName = async () => {
    if (!draftName.trim() || draftName.trim() === project.name) {
      setEditingName(false)
      return
    }
    const ok = await onPatchName(draftName.trim())
    if (ok) setEditingName(false)
  }

  const saveDesc = async () => {
    if (draftDesc === project.description) {
      setEditingDesc(false)
      return
    }
    const ok = await onPatchDescription(draftDesc)
    if (ok) setEditingDesc(false)
  }

  return (
    <Card>
      <div className="flex items-start justify-between gap-3">
        {editingName ? (
          <div className="flex-1 flex items-center gap-2">
            <input
              type="text"
              value={draftName}
              onChange={(e) => setDraftName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') saveName()
                if (e.key === 'Escape') {
                  setDraftName(project.name)
                  setEditingName(false)
                }
              }}
              autoFocus
              className="
                flex-1 rounded-lg border border-border-subtle
                bg-white/80 px-3 py-1.5 text-lg font-semibold tracking-tight
                text-text-primary
                focus:outline-none focus:border-accent
              "
            />
            <button
              type="button"
              onClick={saveName}
              className="text-claim hover:bg-claim/10 p-1.5 rounded-full"
            >
              <Check size={14} />
            </button>
            <button
              type="button"
              onClick={() => {
                setDraftName(project.name)
                setEditingName(false)
              }}
              className="text-text-tertiary hover:bg-black/[0.03] p-1.5 rounded-full"
            >
              <X size={14} />
            </button>
          </div>
        ) : (
          <h1
            className="text-2xl font-semibold tracking-tight text-text-primary cursor-pointer group inline-flex items-center gap-2"
            onClick={beginEditName}
          >
            {project.name}
            <Pencil size={12} className="text-text-tertiary opacity-0 group-hover:opacity-100" />
          </h1>
        )}
      </div>

      <div className="mt-3">
        {editingDesc ? (
          <div className="space-y-2">
            <textarea
              value={draftDesc}
              onChange={(e) => setDraftDesc(e.target.value)}
              autoFocus
              rows={3}
              className="
                w-full rounded-lg border border-border-subtle
                bg-white/80 px-3 py-2 text-[13px] text-text-primary
                resize-none focus:outline-none focus:border-accent
              "
            />
            <div className="flex justify-end gap-2">
              <button
                type="button"
                onClick={() => {
                  setDraftDesc(project.description)
                  setEditingDesc(false)
                }}
                className="text-[12px] text-text-secondary hover:text-text-primary px-2 py-1 rounded-full"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={saveDesc}
                className="text-[12px] bg-accent text-white px-3 py-1 rounded-full hover:opacity-90"
              >
                Save
              </button>
            </div>
          </div>
        ) : (
          <p
            className="text-[13px] text-text-secondary leading-relaxed cursor-pointer group inline-flex items-start gap-2 hover:text-text-primary"
            onClick={beginEditDesc}
          >
            {project.description ? (
              project.description
            ) : (
              <span className="italic text-text-tertiary">Add a description…</span>
            )}
            <Pencil
              size={12}
              className="text-text-tertiary opacity-0 group-hover:opacity-100 mt-1 shrink-0"
            />
          </p>
        )}
      </div>

      <div className="mt-4 flex flex-wrap gap-1.5">
        {project.jira_project_key && (
          <Chip label={`Jira: ${project.jira_project_key}`} tone="accent" />
        )}
        {project.linear_project_key && (
          <Chip label={`Linear: ${project.linear_project_key}`} tone="accent" />
        )}
        {project.pinned_repos.map((slug) => (
          <Chip key={slug} label={slug} tone="muted" />
        ))}
      </div>
    </Card>
  )
}

function ConfigPanel({
  project,
  onPatch,
}: {
  project: Project
  onPatch: (body: Record<string, unknown>) => Promise<boolean | undefined>
}) {
  // Local state mirrors the server-side values; user edits are saved
  // explicitly via the Save button rather than auto-saved on every
  // toggle. Keeps the network traffic predictable and gives the user
  // a single undo point per edit session.
  const [pinnedRepos, setPinnedRepos] = useState(project.pinned_repos)
  const [jiraKey, setJiraKey] = useState(project.jira_project_key)
  const [linearKey, setLinearKey] = useState(project.linear_project_key)
  const [saving, setSaving] = useState(false)

  // Resync if the project changes upstream (e.g. another tab edited it).
  useEffect(() => setPinnedRepos(project.pinned_repos), [project.pinned_repos])
  useEffect(() => setJiraKey(project.jira_project_key), [project.jira_project_key])
  useEffect(() => setLinearKey(project.linear_project_key), [project.linear_project_key])

  const dirty = useMemo(() => {
    if (jiraKey !== project.jira_project_key) return true
    if (linearKey !== project.linear_project_key) return true
    if (pinnedRepos.length !== project.pinned_repos.length) return true
    const a = [...pinnedRepos].sort()
    const b = [...project.pinned_repos].sort()
    for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return true
    return false
  }, [pinnedRepos, jiraKey, linearKey, project])

  const save = async () => {
    setSaving(true)
    try {
      await onPatch({
        pinned_repos: pinnedRepos,
        jira_project_key: jiraKey,
        linear_project_key: linearKey,
      })
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card>
      <h2 className="text-[13px] font-semibold tracking-tight text-text-primary uppercase mb-4">
        Configuration
      </h2>

      <div className="space-y-5">
        <div>
          <span className="block text-[12px] font-medium text-text-secondary mb-1.5">
            Pinned repos
          </span>
          <RepoMultiSelect value={pinnedRepos} onChange={setPinnedRepos} />
        </div>

        <div>
          <span className="block text-[12px] font-medium text-text-secondary mb-1.5">
            Tracker projects
          </span>
          <TrackerProjectPickers
            jiraKey={jiraKey}
            linearKey={linearKey}
            onJiraChange={setJiraKey}
            onLinearChange={setLinearKey}
          />
        </div>
      </div>

      {dirty && (
        <div className="mt-5 flex justify-end gap-2">
          <button
            type="button"
            onClick={() => {
              setPinnedRepos(project.pinned_repos)
              setJiraKey(project.jira_project_key)
              setLinearKey(project.linear_project_key)
            }}
            disabled={saving}
            className="text-[12px] text-text-secondary hover:text-text-primary px-3 py-1.5 rounded-full disabled:opacity-50"
          >
            Reset
          </button>
          <button
            type="button"
            onClick={save}
            disabled={saving}
            className="
              text-[12px] bg-accent text-white
              px-3 py-1.5 rounded-full hover:opacity-90 disabled:opacity-50
            "
          >
            {saving ? 'Saving…' : 'Save changes'}
          </button>
        </div>
      )}
    </Card>
  )
}

function KnowledgePanel({ projectId }: { projectId: string }) {
  const [files, setFiles] = useState<KnowledgeFile[]>([])
  const [loading, setLoading] = useState(true)
  const [expanded, setExpanded] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const res = await fetch(`/api/projects/${encodeURIComponent(projectId)}/knowledge`)
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to load knowledge base'))
          return
        }
        const data: KnowledgeFile[] = await res.json()
        if (!cancelled) setFiles(data)
      } catch (err) {
        if (!cancelled) {
          toast.error(
            `Failed to load knowledge base: ${err instanceof Error ? err.message : String(err)}`,
          )
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    load()
    return () => {
      cancelled = true
    }
  }, [projectId])

  return (
    <Card>
      <h2 className="text-[13px] font-semibold tracking-tight text-text-primary uppercase mb-4">
        Knowledge base
      </h2>

      {loading ? (
        <div className="text-[12px] text-text-tertiary">Loading…</div>
      ) : files.length === 0 ? (
        <div className="text-[12px] text-text-tertiary italic">
          No knowledge files yet. The Curator will populate this as you chat.
        </div>
      ) : (
        <div className="space-y-2">
          {files.map((file) => {
            const isExpanded = expanded === file.path
            return (
              <div
                key={file.path}
                className="rounded-lg border border-border-subtle bg-white/40 overflow-hidden"
              >
                <button
                  type="button"
                  onClick={() => setExpanded(isExpanded ? null : file.path)}
                  className="
                    w-full flex items-center justify-between gap-3
                    px-3 py-2 text-left
                    hover:bg-black/[0.02] transition-colors
                  "
                >
                  <span className="text-[12px] font-medium text-text-primary truncate">
                    {file.path}
                  </span>
                  <span className="text-[10px] text-text-tertiary tabular-nums shrink-0">
                    {formatBytes(file.size_bytes)}
                  </span>
                </button>
                {isExpanded && (
                  <div className="border-t border-border-subtle px-4 py-3 prose prose-sm max-w-none text-[12px] text-text-secondary leading-relaxed">
                    <Markdown>{file.content}</Markdown>
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}
    </Card>
  )
}

function ChatSlotPlaceholder() {
  return (
    <Card className="lg:sticky lg:top-24 lg:h-[calc(100vh-8rem)] flex flex-col">
      <h2 className="text-[13px] font-semibold tracking-tight text-text-primary uppercase mb-2">
        Curator chat
      </h2>
      <div className="flex-1 flex items-center justify-center text-center px-6">
        <div className="text-[12px] text-text-tertiary leading-relaxed italic">
          Chat panel arrives in a follow-up ticket.
          <br />
          The Curator runtime is already running — you can hit it via the API in the meantime.
        </div>
      </div>
    </Card>
  )
}

function Card({ children, className = '' }: { children: React.ReactNode; className?: string }) {
  return (
    <section
      className={`
        relative overflow-hidden rounded-2xl border border-border-glass
        bg-gradient-to-br from-white/70 via-white/50 to-white/35
        p-5 shadow-sm shadow-black/[0.03] backdrop-blur-xl
        ${className}
      `}
    >
      <span
        aria-hidden
        className="pointer-events-none absolute -left-8 -top-8 h-24 w-24 rounded-full bg-white/30 blur-2xl"
      />
      <div className="relative">{children}</div>
    </section>
  )
}

function Chip({ label, tone }: { label: string; tone: 'accent' | 'muted' }) {
  const cls =
    tone === 'accent'
      ? 'bg-accent-soft text-accent'
      : 'bg-black/[0.03] text-text-secondary border border-border-subtle'
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-[11px] ${cls}`}>
      {label}
    </span>
  )
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}
