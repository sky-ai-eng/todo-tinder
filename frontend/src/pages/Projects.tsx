import { useState, useEffect, useCallback } from 'react'
import { Link } from 'react-router-dom'
import { Plus } from 'lucide-react'
import type { Project } from '../types'
import { readError } from '../lib/api'
import { toast } from '../components/Toast/toastStore'
import ProjectCreateModal from '../components/ProjectCreateModal'

// Projects index. List view only — the per-project view lives in
// ProjectDetail.tsx and the Curator chat panel will graft into it
// in SKY-226. We keep the visual language tight enough that a project
// with zero pinned repos / no tracker / no description still renders
// as a recognizable card rather than collapsing into nothing.
//
// Empty-state contract (per SKY-217): zero projects renders a centered
// "Create your first project" CTA, not an empty grid. The full grid
// only appears once at least one project exists.
export default function Projects() {
  const [projects, setProjects] = useState<Project[]>([])
  const [loading, setLoading] = useState(true)
  const [createOpen, setCreateOpen] = useState(false)

  const refresh = useCallback(async () => {
    try {
      const res = await fetch('/api/projects')
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to load projects'))
        return
      }
      const data: Project[] = await res.json()
      setProjects(data)
    } catch (err) {
      toast.error(`Failed to load projects: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  const handleCreated = useCallback(
    (created: Project) => {
      setCreateOpen(false)
      setProjects((prev) => [...prev, created].sort((a, b) => a.name.localeCompare(b.name)))
      // Re-fetch to pick up server-generated fields we don't model
      // optimistically (e.g. anything the server post-processes).
      refresh()
    },
    [refresh],
  )

  if (loading) {
    return (
      <div className="max-w-6xl mx-auto">
        <div className="text-text-tertiary text-[13px]">Loading projects…</div>
      </div>
    )
  }

  return (
    <div className="max-w-6xl mx-auto">
      <header className="flex items-center justify-between mb-8">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight text-text-primary">Projects</h1>
          <p className="text-[13px] text-text-secondary mt-1">
            Group work by concept. Pin repos and tracker projects for the Curator to reason about.
          </p>
        </div>
        {projects.length > 0 && (
          <button
            type="button"
            onClick={() => setCreateOpen(true)}
            className="
              inline-flex items-center gap-2 rounded-full
              bg-accent text-white text-[13px] font-medium
              px-4 py-2 transition-all
              hover:opacity-90
            "
          >
            <Plus size={14} />
            New project
          </button>
        )}
      </header>

      {projects.length === 0 ? (
        <EmptyState onCreate={() => setCreateOpen(true)} />
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-5">
          {projects.map((p) => (
            <ProjectCard key={p.id} project={p} />
          ))}
        </div>
      )}

      {createOpen && (
        <ProjectCreateModal onClose={() => setCreateOpen(false)} onCreated={handleCreated} />
      )}
    </div>
  )
}

function EmptyState({ onCreate }: { onCreate: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center py-24">
      <div className="text-text-tertiary text-[13px] max-w-md text-center mb-6">
        Projects bundle pinned repos, a Jira/Linear project, and a knowledge base — the Curator
        works inside that scope when you chat with it.
      </div>
      <button
        type="button"
        onClick={onCreate}
        className="
          inline-flex items-center gap-2 rounded-full
          bg-accent text-white text-[13px] font-medium
          px-5 py-2.5 transition-all
          hover:opacity-90
        "
      >
        <Plus size={14} />
        Create your first project
      </button>
    </div>
  )
}

function ProjectCard({ project }: { project: Project }) {
  const desc = (project.description || '').trim()
  return (
    <Link
      to={`/projects/${encodeURIComponent(project.id)}`}
      className="
        group relative overflow-hidden rounded-2xl border border-border-glass
        bg-gradient-to-br from-white/70 via-white/50 to-white/35
        p-5 shadow-sm shadow-black/[0.03] backdrop-blur-xl
        transition-[box-shadow,border-color] duration-300
        hover:border-white/90 hover:shadow-md hover:shadow-black/[0.05]
        block
      "
    >
      <span
        aria-hidden
        className="pointer-events-none absolute -left-8 -top-8 h-24 w-24 rounded-full bg-white/30 blur-2xl"
      />
      <div className="relative">
        <h3 className="text-[14px] font-semibold tracking-tight text-text-primary truncate">
          {project.name}
        </h3>
        {desc && (
          <p className="mt-2 text-[12px] leading-relaxed text-text-secondary line-clamp-2">
            {desc}
          </p>
        )}
        <div className="mt-3 flex flex-wrap gap-1.5">
          {project.jira_project_key && (
            <Chip label={`Jira: ${project.jira_project_key}`} tone="accent" />
          )}
          {project.linear_project_key && (
            <Chip label={`Linear: ${project.linear_project_key}`} tone="accent" />
          )}
          {project.pinned_repos.map((slug) => (
            <Chip key={slug} label={slug} tone="muted" />
          ))}
          {project.pinned_repos.length === 0 &&
            !project.jira_project_key &&
            !project.linear_project_key && (
              <span className="text-[11px] text-text-tertiary italic">
                No repos or tracker linked yet
              </span>
            )}
        </div>
        <div className="mt-4 text-[11px] text-text-tertiary tabular-nums">
          Updated {formatAge(project.updated_at)}
        </div>
      </div>
    </Link>
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

// formatAge keeps the card foot quiet — relative times for fresh
// updates, absolute dates after the activity has settled. Mirrors the
// shape Repos uses so users get the same temporal feel across pages.
function formatAge(iso: string): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return iso
  const diffMs = Date.now() - t
  const sec = Math.floor(diffMs / 1000)
  if (sec < 60) return 'just now'
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m ago`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr}h ago`
  const day = Math.floor(hr / 24)
  if (day < 14) return `${day}d ago`
  return new Date(t).toLocaleDateString()
}
